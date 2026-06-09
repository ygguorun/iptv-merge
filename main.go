package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
	"gopkg.in/yaml.v3"
)

const (
	defaultConfigPath = "config.yaml"
	defaultServerPort = 7777
	defaultUserAgent  = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/117.0.0.0 Safari/537.36"
	unknownOrder      = 1_000_000_000
)

var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

type URLSource struct {
	URL       string
	UserAgent string
}

type MatchRule map[string]string

type ChannelDictionary struct {
	Name         string                 `yaml:"name"`
	Channels     map[string][]MatchRule `yaml:"channels"`
	ChannelOrder []string               `yaml:"-"`
}

type Settings struct {
	Port               int    `yaml:"port"`
	SourceTimeout      string `yaml:"source_timeout"`
	SourceCacheTTL     string `yaml:"source_cache_ttl"`
	ResultCacheTTL     string `yaml:"result_cache_ttl"`
	ServerReadTimeout  string `yaml:"server_read_timeout"`
	ServerWriteTimeout string `yaml:"server_write_timeout"`
	ServerIdleTimeout  string `yaml:"server_idle_timeout"`
}

type Config struct {
	Urls          []string            `yaml:"urls"`
	ChannelGroups []ChannelDictionary `yaml:"channel-groups"`
	Server        Settings            `yaml:"server"`
}

type cliOptions struct {
	ConfigPath  string
	ShowHelp    bool
	ShowVersion bool
}

func (dictionary *ChannelDictionary) UnmarshalYAML(value *yaml.Node) error {
	type rawChannelDictionary struct {
		Name     string                 `yaml:"name"`
		Channels map[string][]MatchRule `yaml:"channels"`
	}

	var raw rawChannelDictionary
	if err := value.Decode(&raw); err != nil {
		return err
	}

	dictionary.Name = raw.Name
	dictionary.Channels = raw.Channels
	dictionary.ChannelOrder = parseConfiguredChannelOrder(value)
	return nil
}

func parseConfiguredChannelOrder(dictionaryNode *yaml.Node) []string {
	channelsNode := mappingValue(dictionaryNode, "channels")
	if channelsNode == nil || channelsNode.Kind != yaml.MappingNode {
		return nil
	}

	order := make([]string, 0, len(channelsNode.Content)/2)
	seen := make(map[string]struct{}, len(channelsNode.Content)/2)
	for index := 0; index+1 < len(channelsNode.Content); index += 2 {
		name := channelsNode.Content[index].Value
		if name == "*" {
			continue
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		order = append(order, name)
	}
	return order
}

func mappingValue(mappingNode *yaml.Node, key string) *yaml.Node {
	if mappingNode == nil || mappingNode.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index+1 < len(mappingNode.Content); index += 2 {
		if mappingNode.Content[index].Value == key {
			return mappingNode.Content[index+1]
		}
	}
	return nil
}

func parseCLIArgs(args []string) (cliOptions, error) {
	options := cliOptions{ConfigPath: defaultConfigPath}
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "-h":
			options.ShowHelp = true
			return options, nil
		case arg == "-v" || arg == "--version":
			options.ShowVersion = true
			return options, nil
		case arg == "-c":
			if index+1 >= len(args) || strings.TrimSpace(args[index+1]) == "" {
				return options, fmt.Errorf("-c requires a configuration file path")
			}
			index++
			options.ConfigPath = strings.TrimSpace(args[index])
		case strings.HasPrefix(arg, "-c="):
			configPath := strings.TrimSpace(strings.TrimPrefix(arg, "-c="))
			if configPath == "" {
				return options, fmt.Errorf("-c requires a configuration file path")
			}
			options.ConfigPath = configPath
		default:
			return options, fmt.Errorf("unsupported argument %q", arg)
		}
	}
	return options, nil
}

func printUsage() {
	fmt.Fprintf(os.Stdout, "Usage: %s [-c config.yaml]\n\n", os.Args[0])
	fmt.Fprintln(os.Stdout, "Options:")
	fmt.Fprintln(os.Stdout, "  -c <path>     configuration file path (default: config.yaml)")
	fmt.Fprintln(os.Stdout, "  -h            show this help")
	fmt.Fprintln(os.Stdout, "  -v, --version  show version information")
}

func printVersion() {
	fmt.Fprintf(os.Stdout, "iptv-merge %s\ncommit: %s\nbuilt: %s\n", version, commit, buildDate)
}

type resolvedSettings struct {
	Port               int
	SourceTimeout      time.Duration
	SourceCacheTTL     time.Duration
	ResultCacheTTL     time.Duration
	ServerReadTimeout  time.Duration
	ServerWriteTimeout time.Duration
	ServerIdleTimeout  time.Duration
}

type cacheEntry struct {
	data       string
	expiryTime time.Time
}

type TvgInfo struct {
	ID   string
	Name string
	Logo string
}

type Channel struct {
	Name               string
	Group              string
	URL                string
	Duration           string
	Tvg                TvgInfo
	Fields             map[string]string
	SourceIndex        int
	SourceChannelIndex int
	DictionaryIndex    int
	ChannelOrder       int
	RuleIndex          int
	Fallback           bool
}

type fieldMatcher struct {
	name string
	re   *regexp.Regexp
}

type compiledRule struct {
	index    int
	matchers []fieldMatcher
}

type compiledChannel struct {
	name  string
	order int
	rules []compiledRule
}

type compiledDictionary struct {
	name     string
	index    int
	channels []compiledChannel
	fallback []compiledRule
}

type compiledConfig struct {
	urls         []URLSource
	dictionaries []compiledDictionary
	settings     resolvedSettings
}

type sourceResult struct {
	index    int
	channels []Channel
	err      error
}

var (
	rawCacheLock sync.RWMutex
	rawCache     = make(map[string]*cacheEntry)

	processedCacheLock sync.RWMutex
	processedCache     = make(map[string]*cacheEntry)

	processGroup singleflight.Group

	extinfAttrRegex = regexp.MustCompile(`([A-Za-z0-9_.:-]+)\s*=\s*("[^"]*"|'[^']*'|[^\s,]+)`)
	qualityRegex    = regexp.MustCompile(`(?i)\((2160p|1080p|720p|576p|480p|360p|240p|8k|4k)\)`)
	labelRegex      = regexp.MustCompile(`\[([^\]]+)\]`)
)

func defaultSettings() resolvedSettings {
	return resolvedSettings{
		Port:               defaultServerPort,
		SourceTimeout:      30 * time.Second,
		SourceCacheTTL:     time.Hour,
		ResultCacheTTL:     time.Minute,
		ServerReadTimeout:  10 * time.Second,
		ServerWriteTimeout: 60 * time.Second,
		ServerIdleTimeout:  120 * time.Second,
	}
}

func parseDurationSetting(value string, fallback time.Duration, name string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback, nil
	}

	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid server.%s %q: %w", name, value, err)
	}
	return duration, nil
}

func resolveSettings(settings Settings) (resolvedSettings, error) {
	defaults := defaultSettings()
	var err error
	if settings.Port != 0 {
		if settings.Port < 1 || settings.Port > 65535 {
			return defaults, fmt.Errorf("invalid server.port %d: must be between 1 and 65535", settings.Port)
		}
		defaults.Port = settings.Port
	}

	defaults.SourceTimeout, err = parseDurationSetting(settings.SourceTimeout, defaults.SourceTimeout, "source_timeout")
	if err != nil {
		return defaults, err
	}
	defaults.SourceCacheTTL, err = parseDurationSetting(settings.SourceCacheTTL, defaults.SourceCacheTTL, "source_cache_ttl")
	if err != nil {
		return defaults, err
	}
	defaults.ResultCacheTTL, err = parseDurationSetting(settings.ResultCacheTTL, defaults.ResultCacheTTL, "result_cache_ttl")
	if err != nil {
		return defaults, err
	}
	defaults.ServerReadTimeout, err = parseDurationSetting(settings.ServerReadTimeout, defaults.ServerReadTimeout, "server_read_timeout")
	if err != nil {
		return defaults, err
	}
	defaults.ServerWriteTimeout, err = parseDurationSetting(settings.ServerWriteTimeout, defaults.ServerWriteTimeout, "server_write_timeout")
	if err != nil {
		return defaults, err
	}
	defaults.ServerIdleTimeout, err = parseDurationSetting(settings.ServerIdleTimeout, defaults.ServerIdleTimeout, "server_idle_timeout")
	if err != nil {
		return defaults, err
	}

	return defaults, nil
}

func loadConfig(configPath string) (*compiledConfig, error) {
	configData, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}

	var config Config
	if err := yaml.Unmarshal(configData, &config); err != nil {
		return nil, err
	}

	settings, err := resolveSettings(config.Server)
	if err != nil {
		return nil, err
	}
	sources := compileURLSources(config.Urls)
	if len(sources) == 0 {
		return nil, fmt.Errorf("no urls configured")
	}
	if len(config.ChannelGroups) == 0 {
		return nil, fmt.Errorf("no channel groups configured")
	}

	dictionaries, err := compileDictionaries(config.ChannelGroups)
	if err != nil {
		return nil, err
	}

	return &compiledConfig{
		urls:         sources,
		dictionaries: dictionaries,
		settings:     settings,
	}, nil
}

func compileURLSources(urls []string) []URLSource {
	sources := make([]URLSource, 0, len(urls))
	for _, url := range urls {
		url = strings.TrimSpace(url)
		if url == "" {
			continue
		}
		sources = append(sources, URLSource{URL: url})
	}
	return sources
}

func loadRuntimeSettings(configPath string) resolvedSettings {
	configData, err := os.ReadFile(configPath)
	if err != nil {
		fmt.Println("Error loading runtime settings, using defaults:", err)
		return defaultSettings()
	}

	var config Config
	if err := yaml.Unmarshal(configData, &config); err != nil {
		fmt.Println("Error loading runtime settings, using defaults:", err)
		return defaultSettings()
	}

	settings, err := resolveSettings(config.Server)
	if err != nil {
		fmt.Println("Error parsing runtime settings, using defaults:", err)
		return defaultSettings()
	}
	return settings
}

func compileDictionaries(dictionaries []ChannelDictionary) ([]compiledDictionary, error) {
	compiled := make([]compiledDictionary, 0, len(dictionaries))

	for dictionaryIndex, dictionary := range dictionaries {
		if strings.TrimSpace(dictionary.Name) == "" {
			return nil, fmt.Errorf("channel-groups[%d].name is empty", dictionaryIndex)
		}

		compiledDictionary := compiledDictionary{
			name:  dictionary.Name,
			index: dictionaryIndex,
		}

		channelOrderByName := configuredChannelOrder(dictionary)
		channelNames := orderedChannelNames(dictionary, channelOrderByName)

		for _, channelName := range channelNames {
			rules, err := compileRules(dictionary.Channels[channelName], dictionary.Name, channelName)
			if err != nil {
				return nil, err
			}
			compiledDictionary.channels = append(compiledDictionary.channels, compiledChannel{
				name:  channelName,
				order: channelOrder(channelName, channelOrderByName),
				rules: rules,
			})
		}

		if fallbackRules, exists := dictionary.Channels["*"]; exists {
			rules, err := compileRules(fallbackRules, dictionary.Name, "*")
			if err != nil {
				return nil, err
			}
			compiledDictionary.fallback = rules
		}

		compiled = append(compiled, compiledDictionary)
	}

	return compiled, nil
}

func configuredChannelOrder(dictionary ChannelDictionary) map[string]int {
	orderByName := make(map[string]int, len(dictionary.ChannelOrder))
	for index, name := range dictionary.ChannelOrder {
		orderByName[name] = index
	}
	return orderByName
}

func orderedChannelNames(dictionary ChannelDictionary, orderByName map[string]int) []string {
	channelNames := make([]string, 0, len(dictionary.Channels))
	seen := make(map[string]struct{}, len(dictionary.Channels))

	for _, name := range dictionary.ChannelOrder {
		if name == "*" {
			continue
		}
		if _, exists := dictionary.Channels[name]; !exists {
			continue
		}
		channelNames = append(channelNames, name)
		seen[name] = struct{}{}
	}

	var unorderedNames []string
	for name := range dictionary.Channels {
		if name == "*" {
			continue
		}
		if _, exists := seen[name]; exists {
			continue
		}
		unorderedNames = append(unorderedNames, name)
	}
	sort.Strings(unorderedNames)
	for _, name := range unorderedNames {
		orderByName[name] = len(orderByName)
		channelNames = append(channelNames, name)
	}

	return channelNames
}

func compileRules(rules []MatchRule, dictionaryName, channelName string) ([]compiledRule, error) {
	compiled := make([]compiledRule, 0, len(rules))
	for ruleIndex, rule := range rules {
		compiledRule := compiledRule{index: ruleIndex}
		for fieldName, pattern := range rule {
			fieldName = canonicalFieldName(fieldName)
			re, err := regexp.Compile(pattern)
			if err != nil {
				return nil, fmt.Errorf("invalid regex for %s/%s rule %d field %s: %w", dictionaryName, channelName, ruleIndex, fieldName, err)
			}
			compiledRule.matchers = append(compiledRule.matchers, fieldMatcher{name: fieldName, re: re})
		}
		if len(compiledRule.matchers) == 0 {
			return nil, fmt.Errorf("empty rule for %s/%s at index %d", dictionaryName, channelName, ruleIndex)
		}
		compiled = append(compiled, compiledRule)
	}
	return compiled, nil
}

func channelOrder(name string, orderByName map[string]int) int {
	if order, exists := orderByName[name]; exists {
		return order
	}
	return unknownOrder
}

func fetchM3U(source URLSource, settings resolvedSettings, bypassCache bool) (string, error) {
	cacheKey := source.URL + "\x00" + source.UserAgent
	if !bypassCache && settings.SourceCacheTTL > 0 {
		rawCacheLock.RLock()
		entry, exists := rawCache[cacheKey]
		if exists && time.Now().Before(entry.expiryTime) {
			rawCacheLock.RUnlock()
			return entry.data, nil
		}
		rawCacheLock.RUnlock()
	}

	var (
		data string
		err  error
	)
	if isHTTPURL(source.URL) {
		data, err = fetchM3UFromURL(source.URL, source.UserAgent, settings.SourceTimeout)
	} else {
		data, err = fetchM3UFromFile(source.URL)
	}
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(data) == "" {
		return "", fmt.Errorf("empty M3U source %s", source.URL)
	}

	if settings.SourceCacheTTL > 0 {
		rawCacheLock.Lock()
		rawCache[cacheKey] = &cacheEntry{
			data:       data,
			expiryTime: time.Now().Add(settings.SourceCacheTTL),
		}
		rawCacheLock.Unlock()
	}

	return data, nil
}

func fetchM3UFromURL(url, userAgent string, timeout time.Duration) (string, error) {
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}
	if userAgent == "" {
		userAgent = defaultUserAgent
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetching URL %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("fetching URL %s: unexpected status %s", url, resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading URL %s: %w", url, err)
	}
	return string(body), nil
}

func fetchM3UFromFile(filePath string) (string, error) {
	body, err := os.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("reading file %s: %w", filePath, err)
	}
	return string(body), nil
}

func parseSources(config *compiledConfig, bypassCache bool) ([]Channel, error) {
	var wg sync.WaitGroup
	results := make(chan sourceResult, len(config.urls))

	for sourceIndex, source := range config.urls {
		wg.Add(1)
		go func(sourceIndex int, source URLSource) {
			defer wg.Done()

			data, err := fetchM3U(source, config.settings, bypassCache)
			if err != nil {
				results <- sourceResult{index: sourceIndex, err: err}
				return
			}

			channels := parseM3UData(data, sourceIndex)
			if len(channels) == 0 {
				results <- sourceResult{index: sourceIndex, err: fmt.Errorf("source %s contains no channels", source.URL)}
				return
			}
			results <- sourceResult{index: sourceIndex, channels: channels}
		}(sourceIndex, source)
	}

	wg.Wait()
	close(results)

	orderedResults := make([]sourceResult, len(config.urls))
	for result := range results {
		orderedResults[result.index] = result
	}

	var allChannels []Channel
	var sourceErrors []string
	for _, result := range orderedResults {
		if result.err != nil {
			sourceErrors = append(sourceErrors, result.err.Error())
			continue
		}
		allChannels = append(allChannels, result.channels...)
	}

	if len(allChannels) == 0 {
		return nil, fmt.Errorf("all sources failed: %s", strings.Join(sourceErrors, "; "))
	}
	for _, sourceError := range sourceErrors {
		fmt.Println("Source skipped:", sourceError)
	}

	return allChannels, nil
}

func parseM3UData(m3uData string, sourceIndex int) []Channel {
	var channels []Channel
	lines := strings.Split(m3uData, "\n")
	var current Channel
	hasCurrent := false

	for _, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "#EXTINF:") {
			current = parseEXTINF(line)
			current.SourceIndex = sourceIndex
			hasCurrent = true
			continue
		}

		if !hasCurrent {
			continue
		}

		if strings.HasPrefix(line, "#EXTGRP:") {
			setChannelField(&current, "group", strings.TrimSpace(strings.TrimPrefix(line, "#EXTGRP:")))
			continue
		}

		if strings.HasPrefix(line, "#EXTVLCOPT:") {
			parseVLCOpt(&current, line)
			continue
		}

		if strings.HasPrefix(line, "#") {
			continue
		}

		current.URL = line
		current.SourceChannelIndex = len(channels)
		setChannelField(&current, "url", line)
		finalizeChannelFields(&current)

		if current.Name != "" && current.URL != "" {
			channels = append(channels, current)
		}
		current = Channel{}
		hasCurrent = false
	}

	return channels
}

func parseEXTINF(line string) Channel {
	channel := Channel{Fields: make(map[string]string)}
	setChannelField(&channel, "raw_extinf", line)

	content := strings.TrimSpace(strings.TrimPrefix(line, "#EXTINF:"))
	commaIndex := lastCommaOutsideQuotes(content)
	metadata := content
	if commaIndex >= 0 {
		metadata = strings.TrimSpace(content[:commaIndex])
		setChannelField(&channel, "name", strings.TrimSpace(content[commaIndex+1:]))
	}

	duration, attrsText := splitDurationAndAttributes(metadata)
	if duration != "" {
		channel.Duration = duration
		setChannelField(&channel, "duration", duration)
	}

	for key, value := range parseAttributes(attrsText) {
		setChannelField(&channel, key, value)
	}

	if matches := qualityRegex.FindStringSubmatch(channel.Name); len(matches) == 2 {
		setChannelField(&channel, "quality", matches[1])
	}
	if matches := labelRegex.FindStringSubmatch(channel.Name); len(matches) == 2 {
		setChannelField(&channel, "label", matches[1])
	}

	return channel
}

func splitDurationAndAttributes(metadata string) (string, string) {
	metadata = strings.TrimSpace(metadata)
	if metadata == "" {
		return "", ""
	}

	index := 0
	for index < len(metadata) && metadata[index] != ' ' && metadata[index] != '\t' && metadata[index] != ',' {
		index++
	}

	duration := strings.TrimSpace(metadata[:index])
	attrsText := strings.TrimLeft(metadata[index:], " \t,")
	return duration, attrsText
}

func parseAttributes(text string) map[string]string {
	attrs := make(map[string]string)
	for _, match := range extinfAttrRegex.FindAllStringSubmatch(text, -1) {
		if len(match) != 3 {
			continue
		}
		attrs[match[1]] = unquoteAttributeValue(match[2])
	}
	return attrs
}

func parseVLCOpt(channel *Channel, line string) {
	content := strings.TrimSpace(strings.TrimPrefix(line, "#EXTVLCOPT:"))
	key, value, exists := strings.Cut(content, "=")
	if !exists {
		return
	}
	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)

	switch canonicalFieldName(key) {
	case "http_user_agent":
		setChannelField(channel, "http_user_agent", value)
	case "http_referrer":
		setChannelField(channel, "http_referrer", value)
	default:
		setChannelField(channel, "vlcopt_"+key, value)
	}
}

func unquoteAttributeValue(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 {
		first := value[0]
		last := value[len(value)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			return value[1 : len(value)-1]
		}
	}
	return value
}

func lastCommaOutsideQuotes(text string) int {
	inQuote := false
	quoteChar := rune(0)
	lastComma := -1

	for index, char := range text {
		if char == '"' || char == '\'' {
			if inQuote && char == quoteChar {
				inQuote = false
			} else if !inQuote {
				inQuote = true
				quoteChar = char
			}
			continue
		}
		if char == ',' && !inQuote {
			lastComma = index
		}
	}

	return lastComma
}

func setChannelField(channel *Channel, fieldName, value string) {
	if channel.Fields == nil {
		channel.Fields = make(map[string]string)
	}
	fieldName = canonicalFieldName(fieldName)
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}

	channel.Fields[fieldName] = value
	switch fieldName {
	case "name":
		channel.Name = value
	case "group":
		channel.Group = value
	case "url":
		channel.URL = value
	case "duration":
		channel.Duration = value
	case "tvg_id":
		channel.Tvg.ID = value
	case "tvg_name":
		channel.Tvg.Name = value
	case "tvg_logo":
		channel.Tvg.Logo = value
	}
}

func finalizeChannelFields(channel *Channel) {
	setChannelField(channel, "name", channel.Name)
	setChannelField(channel, "group", channel.Group)
	setChannelField(channel, "url", channel.URL)
	setChannelField(channel, "duration", channel.Duration)
	setChannelField(channel, "tvg_id", channel.Tvg.ID)
	setChannelField(channel, "tvg_name", channel.Tvg.Name)
	setChannelField(channel, "tvg_logo", channel.Tvg.Logo)

	values := make([]string, 0, len(channel.Fields))
	for key, value := range channel.Fields {
		if key == "raw" {
			continue
		}
		values = append(values, value)
	}
	sort.Strings(values)
	setChannelField(channel, "raw", strings.Join(values, " "))
}

func normalizeFieldName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	var builder strings.Builder
	previousUnderscore := false

	for _, char := range name {
		isAlphaNumeric := (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9')
		if isAlphaNumeric {
			builder.WriteRune(char)
			previousUnderscore = false
			continue
		}
		if !previousUnderscore {
			builder.WriteByte('_')
			previousUnderscore = true
		}
	}

	return strings.Trim(builder.String(), "_")
}

func canonicalFieldName(name string) string {
	switch normalized := normalizeFieldName(name); normalized {
	case "title", "channel", "display_name":
		return "name"
	case "group_title", "tvg_group", "group_name":
		return "group"
	case "id", "tvgid":
		return "tvg_id"
	case "logo", "tvglogo":
		return "tvg_logo"
	case "number", "chno", "channel_number", "tvgchno":
		return "tvg_chno"
	case "country", "tvgcountry":
		return "tvg_country"
	case "language", "lang", "tvglanguage":
		return "tvg_language"
	case "user_agent", "http_user_agent", "http_useragent":
		return "http_user_agent"
	case "referrer", "referer", "http_referrer", "http_referer":
		return "http_referrer"
	case "unique_id", "cuid":
		return "cuid"
	case "channelid", "channel_id":
		return "channel_id"
	default:
		return normalized
	}
}

func classifyChannels(channels []Channel, config *compiledConfig) []Channel {
	classified := make([]Channel, 0, len(channels))

	for _, channel := range channels {
		if output, matched := matchExplicitChannel(channel, config); matched {
			classified = append(classified, output)
			continue
		}
		if output, matched := matchFallbackChannel(channel, config); matched {
			classified = append(classified, output)
		}
	}

	sortChannels(classified)
	return dedupeChannels(classified)
}

func matchExplicitChannel(channel Channel, config *compiledConfig) (Channel, bool) {
	for _, dictionary := range config.dictionaries {
		for _, compiledChannel := range dictionary.channels {
			for _, rule := range compiledChannel.rules {
				if matchesRule(channel, rule) {
					return makeOutputChannel(channel, dictionary, compiledChannel.name, compiledChannel.order, rule.index, false), true
				}
			}
		}
	}
	return Channel{}, false
}

func matchFallbackChannel(channel Channel, config *compiledConfig) (Channel, bool) {
	for _, dictionary := range config.dictionaries {
		for _, rule := range dictionary.fallback {
			if matchesRule(channel, rule) {
				return makeOutputChannel(channel, dictionary, channel.Name, unknownOrder, rule.index, true), true
			}
		}
	}
	return Channel{}, false
}

func matchesRule(channel Channel, rule compiledRule) bool {
	for _, matcher := range rule.matchers {
		value, exists := channel.Fields[matcher.name]
		if !exists || value == "" {
			return false
		}
		if !matcher.re.MatchString(value) {
			return false
		}
	}
	return true
}

func makeOutputChannel(channel Channel, dictionary compiledDictionary, name string, order int, ruleIndex int, fallback bool) Channel {
	output := channel
	output.Fields = copyStringMap(channel.Fields)
	output.Name = name
	output.Group = dictionary.name
	output.DictionaryIndex = dictionary.index
	output.ChannelOrder = order
	output.RuleIndex = ruleIndex
	output.Fallback = fallback
	if output.Tvg.Name == "" {
		output.Tvg.Name = name
	}
	setChannelField(&output, "name", output.Name)
	setChannelField(&output, "group", output.Group)
	setChannelField(&output, "tvg_name", output.Tvg.Name)
	return output
}

func copyStringMap(input map[string]string) map[string]string {
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func sortChannels(channels []Channel) {
	sort.SliceStable(channels, func(i, j int) bool {
		left := channels[i]
		right := channels[j]

		if left.DictionaryIndex != right.DictionaryIndex {
			return left.DictionaryIndex < right.DictionaryIndex
		}
		if left.ChannelOrder != right.ChannelOrder {
			return left.ChannelOrder < right.ChannelOrder
		}
		if left.Name != right.Name {
			return left.Name < right.Name
		}
		if left.RuleIndex != right.RuleIndex {
			return left.RuleIndex < right.RuleIndex
		}
		if left.SourceIndex != right.SourceIndex {
			return left.SourceIndex < right.SourceIndex
		}
		return left.SourceChannelIndex < right.SourceChannelIndex
	})
}

func dedupeChannels(channels []Channel) []Channel {
	deduped := make([]Channel, 0, len(channels))
	seen := make(map[string]struct{}, len(channels))

	for _, channel := range channels {
		key := channel.Group + "\x00" + channel.Name + "\x00" + channel.URL
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		deduped = append(deduped, channel)
	}

	return deduped
}

func generateM3U(channels []Channel, mergeURLs bool) string {
	var output strings.Builder
	output.WriteString("#EXTM3U\n")

	if !mergeURLs {
		for _, channel := range channels {
			writeM3UChannel(&output, channel, []string{channel.URL})
		}
		return output.String()
	}

	for index := 0; index < len(channels); {
		channel := channels[index]
		urls := []string{channel.URL}
		index++

		for index < len(channels) && sameOutputChannel(channel, channels[index]) {
			urls = append(urls, channels[index].URL)
			index++
		}

		writeM3UChannel(&output, channel, urls)
	}

	return output.String()
}

func sameOutputChannel(left, right Channel) bool {
	return left.Group == right.Group && left.Name == right.Name
}

func writeM3UChannel(output *strings.Builder, channel Channel, urls []string) {
	duration := channel.Duration
	if duration == "" {
		duration = "-1"
	}

	attrs := make([]string, 0, 12)
	attrs = appendM3UAttr(attrs, "tvg-id", channel.Tvg.ID)
	attrs = appendM3UAttr(attrs, "tvg-name", firstNonEmpty(channel.Tvg.Name, channel.Name))
	attrs = appendM3UAttr(attrs, "tvg-logo", channel.Tvg.Logo)
	attrs = appendM3UAttr(attrs, "group-title", channel.Group)
	attrs = appendKnownOutputAttrs(attrs, channel.Fields)

	output.WriteString("#EXTINF:")
	output.WriteString(duration)
	if len(attrs) > 0 {
		output.WriteByte(' ')
		output.WriteString(strings.Join(attrs, " "))
	}
	output.WriteByte(',')
	output.WriteString(channel.Name)
	output.WriteByte('\n')

	if referrer := channel.Fields["http_referrer"]; referrer != "" {
		output.WriteString("#EXTVLCOPT:http-referrer=")
		output.WriteString(referrer)
		output.WriteByte('\n')
	}
	if userAgent := channel.Fields["http_user_agent"]; userAgent != "" {
		output.WriteString("#EXTVLCOPT:http-user-agent=")
		output.WriteString(userAgent)
		output.WriteByte('\n')
	}

	for _, url := range urls {
		output.WriteString(url)
		output.WriteByte('\n')
	}
}

func appendKnownOutputAttrs(attrs []string, fields map[string]string) []string {
	knownAttrs := []struct {
		field string
		attr  string
	}{
		{field: "tvg_chno", attr: "tvg-chno"},
		{field: "tvg_country", attr: "tvg-country"},
		{field: "tvg_language", attr: "tvg-language"},
		{field: "radio", attr: "radio"},
		{field: "catchup", attr: "catchup"},
		{field: "catchup_source", attr: "catchup-source"},
		{field: "catchup_days", attr: "catchup-days"},
		{field: "catchup_correction", attr: "catchup-correction"},
		{field: "timeshift", attr: "timeshift"},
		{field: "tvg_shift", attr: "tvg-shift"},
		{field: "cuid", attr: "CUID"},
		{field: "channel_id", attr: "channel-id"},
	}

	for _, knownAttr := range knownAttrs {
		attrs = appendM3UAttr(attrs, knownAttr.attr, fields[knownAttr.field])
	}
	return attrs
}

func appendM3UAttr(attrs []string, name, value string) []string {
	if value == "" {
		return attrs
	}
	return append(attrs, fmt.Sprintf(`%s="%s"`, name, escapeM3UAttr(value)))
}

func escapeM3UAttr(value string) string {
	return strings.ReplaceAll(value, `"`, `&quot;`)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func generateAllM3U(configPath string, bypassCache bool, mergeURLs bool) (string, time.Duration, error) {
	config, err := loadConfig(configPath)
	if err != nil {
		return "", 0, err
	}

	rawChannels, err := parseSources(config, bypassCache)
	if err != nil {
		return "", 0, err
	}

	processedChannels := classifyChannels(rawChannels, config)
	return generateM3U(processedChannels, mergeURLs), config.settings.ResultCacheTTL, nil
}

func generateAndCacheM3U(configPath string, bypassCache bool, mergeURLs bool) (string, error) {
	newM3U, resultCacheTTL, err := generateAllM3U(configPath, bypassCache, mergeURLs)
	if err != nil {
		return "", err
	}

	if resultCacheTTL > 0 {
		processedCacheLock.Lock()
		processedCache[processedCacheKey(mergeURLs)] = &cacheEntry{
			data:       newM3U,
			expiryTime: time.Now().Add(resultCacheTTL),
		}
		processedCacheLock.Unlock()
	}

	return newM3U, nil
}

func processedCacheKey(mergeURLs bool) string {
	if mergeURLs {
		return "merge=true"
	}
	return "merge=false"
}

func handler(configPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/all.m3u" {
			http.NotFound(w, r)
			return
		}

		bypassCache := r.URL.Query().Get("cache") == "false"
		mergeURLs := r.URL.Query().Get("merge") != "false"
		cacheKey := processedCacheKey(mergeURLs)
		if bypassCache {
			newM3U, err := generateAndCacheM3U(configPath, true, mergeURLs)
			if err != nil {
				http.Error(w, fmt.Sprintf("Error generating M3U: %v", err), http.StatusInternalServerError)
				return
			}
			writeM3U(w, newM3U)
			return
		}

		processedCacheLock.RLock()
		cached := processedCache[cacheKey]
		processedCacheLock.RUnlock()

		if cached != nil && time.Now().Before(cached.expiryTime) {
			writeM3U(w, cached.data)
			return
		}

		result, err, _ := processGroup.Do("all.m3u:"+cacheKey, func() (interface{}, error) {
			return generateAndCacheM3U(configPath, false, mergeURLs)
		})
		if err != nil {
			http.Error(w, fmt.Sprintf("Error generating M3U: %v", err), http.StatusInternalServerError)
			return
		}

		writeM3U(w, result.(string))
	}
}

func writeM3U(w http.ResponseWriter, data string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(data))
}

func isHTTPURL(value string) bool {
	value = strings.ToLower(value)
	return strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://")
}

func main() {
	options, err := parseCLIArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		printUsage()
		os.Exit(2)
	}
	if options.ShowHelp {
		printUsage()
		return
	}
	if options.ShowVersion {
		printVersion()
		return
	}

	settings := loadRuntimeSettings(options.ConfigPath)
	serverAddr := fmt.Sprintf(":%d", settings.Port)
	http.HandleFunc("/", handler(options.ConfigPath))

	server := &http.Server{
		Addr:         serverAddr,
		ReadTimeout:  settings.ServerReadTimeout,
		WriteTimeout: settings.ServerWriteTimeout,
		IdleTimeout:  settings.ServerIdleTimeout,
	}

	fmt.Printf("Starting server on %s with config %s...\n", serverAddr, options.ConfigPath)
	if err := server.ListenAndServe(); err != nil {
		fmt.Println("Error starting server:", err)
	}
}
