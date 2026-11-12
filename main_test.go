package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type countingSource struct {
	*httptest.Server
	fetches  atomic.Int64
	delay    atomic.Int64
	bodyFunc atomic.Pointer[string]
}

func newCountingSource(t testing.TB, body string) *countingSource {
	t.Helper()

	source := &countingSource{}
	source.bodyFunc.Store(&body)
	source.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		source.fetches.Add(1)
		if delay := time.Duration(source.delay.Load()); delay > 0 {
			time.Sleep(delay)
		}
		_, _ = io.WriteString(w, *source.bodyFunc.Load())
	}))
	t.Cleanup(source.Server.Close)
	return source
}

func (source *countingSource) setBody(body string) {
	source.bodyFunc.Store(&body)
}

func playlistBody(name string) string {
	return fmt.Sprintf("#EXTM3U\n#EXTINF:-1 group-title=\"CCTV\",%s\nhttp://example.com/%s.m3u8\n", name, name)
}

func duplicatedPlaylistBody(name string) string {
	return fmt.Sprintf(
		"#EXTM3U\n#EXTINF:-1 group-title=\"CCTV\",%s\nhttp://example.com/%s-a.m3u8\n#EXTINF:-1 group-title=\"CCTV\",%s\nhttp://example.com/%s-b.m3u8\n",
		name, name, name, name,
	)
}

func writeTestConfig(t testing.TB, sourceURL, sourceCacheTTL, resultCacheTTL string) string {
	t.Helper()

	content := fmt.Sprintf(
		"urls:\n  - %q\nchannel-groups:\n  - name: test\n    channels:\n      \"*\":\n        - name: .*\nserver:\n  source_cache_ttl: %s\n  result_cache_ttl: %s\n",
		sourceURL, sourceCacheTTL, resultCacheTTL,
	)

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}
	return path
}

func resetState(t testing.TB) {
	t.Helper()

	rawCacheLock.Lock()
	rawCache = make(map[string]*cacheEntry)
	rawCacheLock.Unlock()

	processedCacheLock.Lock()
	processedCache = make(map[string]*renderedEntry)
	processedCacheLock.Unlock()

	refreshLock.Lock()
	refreshRunning = make(map[string]bool)
	refreshLock.Unlock()

	configCacheLock.Lock()
	configCache = nil
	configCacheLock.Unlock()

	// A background refresh still in flight would share the singleflight key with the next test
	waitForRefreshFlag(t, processedCacheKey(true), false)
	waitForRefreshFlag(t, processedCacheKey(false), false)
}

func fetchPlaylist(t testing.TB, server *httptest.Server, query, ifNoneMatch string) (int, string, http.Header) {
	t.Helper()

	url := server.URL + "/all.m3u"
	if query != "" {
		url += "?" + query
	}

	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if ifNoneMatch != "" {
		request.Header.Set("If-None-Match", ifNoneMatch)
	}

	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return response.StatusCode, string(body), response.Header
}

func waitFor(t testing.TB, timeout time.Duration, condition func() bool, message string) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, message)
}

func cachedEntry(key string) *renderedEntry {
	processedCacheLock.RLock()
	defer processedCacheLock.RUnlock()
	return processedCache[key]
}

func cachedETag(key string) string {
	entry := cachedEntry(key)
	if entry == nil {
		return ""
	}
	return entry.etag
}

func TestHandlerColdStartServesBodyWithHeaders(t *testing.T) {
	resetState(t)

	source := newCountingSource(t, playlistBody("CCTV-1"))
	server := httptest.NewServer(handler(writeTestConfig(t, source.URL, "0s", "1m")))
	defer server.Close()

	status, body, header := fetchPlaylist(t, server, "", "")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if !strings.HasPrefix(body, "#EXTM3U\n") {
		t.Fatalf("body = %q, want playlist header", body)
	}
	if got := header.Get("Content-Type"); got != contentTypeM3U {
		t.Fatalf("Content-Type = %q, want %q", got, contentTypeM3U)
	}
	if got := header.Get("Cache-Control"); got != cacheControlNoCache {
		t.Fatalf("Cache-Control = %q, want %q", got, cacheControlNoCache)
	}
	if got := header.Get("Transfer-Encoding"); got != "" {
		t.Fatalf("Transfer-Encoding = %q, want empty so chunked framing is avoided", got)
	}

	length, err := strconv.Atoi(header.Get("Content-Length"))
	if err != nil {
		t.Fatalf("Content-Length = %q: %v", header.Get("Content-Length"), err)
	}
	if length != len(body) {
		t.Fatalf("Content-Length = %d, body length = %d", length, len(body))
	}
	if got, want := header.Get("ETag"), etagFor([]byte(body)); got != want {
		t.Fatalf("ETag = %q, want %q", got, want)
	}
	if got := source.fetches.Load(); got != 1 {
		t.Fatalf("source fetches = %d, want 1", got)
	}
}

func TestHandlerFreshCacheHitDoesNotFetchSource(t *testing.T) {
	resetState(t)

	source := newCountingSource(t, playlistBody("CCTV-1"))
	server := httptest.NewServer(handler(writeTestConfig(t, source.URL, "0s", "1m")))
	defer server.Close()

	_, firstBody, firstHeader := fetchPlaylist(t, server, "", "")
	status, body, header := fetchPlaylist(t, server, "", "")

	if status != http.StatusOK {
		t.Fatalf("fresh hit status = %d, want %d", status, http.StatusOK)
	}
	if body != firstBody {
		t.Fatalf("fresh hit body = %q, want %q", body, firstBody)
	}
	if header.Get("ETag") != firstHeader.Get("ETag") {
		t.Fatalf("ETag changed on fresh hit: %q vs %q", header.Get("ETag"), firstHeader.Get("ETag"))
	}
	if got := source.fetches.Load(); got != 1 {
		t.Fatalf("source fetches = %d, want 1 (result cache hit must not touch sources)", got)
	}
}

func TestHandlerNotModifiedWhenETagMatches(t *testing.T) {
	resetState(t)

	source := newCountingSource(t, playlistBody("CCTV-1"))
	server := httptest.NewServer(handler(writeTestConfig(t, source.URL, "0s", "1m")))
	defer server.Close()

	_, _, header := fetchPlaylist(t, server, "", "")
	etag := header.Get("ETag")
	if etag == "" {
		t.Fatal("missing ETag from cold start response")
	}

	status, body, second := fetchPlaylist(t, server, "", etag)
	if status != http.StatusNotModified {
		t.Fatalf("status = %d, want %d", status, http.StatusNotModified)
	}
	if body != "" {
		t.Fatalf("304 body = %q, want empty", body)
	}
	if got := second.Get("ETag"); got != etag {
		t.Fatalf("304 ETag = %q, want %q", got, etag)
	}
	if got := second.Get("Content-Length"); got != "" {
		t.Fatalf("304 Content-Length = %q, want unset", got)
	}

	if gotStatus, gotBody, _ := fetchPlaylist(t, server, "", `"deadbeef"`); gotStatus != http.StatusOK || gotBody == "" {
		t.Fatalf("mismatched ETag status = %d, body length = %d, want 200 with body", gotStatus, len(gotBody))
	}
	if got := source.fetches.Load(); got != 1 {
		t.Fatalf("source fetches = %d, want 1", got)
	}
}

func TestHandlerStaleHitServesOldBodyThenRefreshes(t *testing.T) {
	resetState(t)

	source := newCountingSource(t, playlistBody("CCTV-1"))
	server := httptest.NewServer(handler(writeTestConfig(t, source.URL, "0s", "30ms")))
	defer server.Close()

	_, staleBody, staleHeader := fetchPlaylist(t, server, "", "")
	source.setBody(playlistBody("CCTV-2"))
	time.Sleep(50 * time.Millisecond)

	status, body, _ := fetchPlaylist(t, server, "", "")
	if status != http.StatusOK {
		t.Fatalf("stale hit status = %d, want %d", status, http.StatusOK)
	}
	if body != staleBody {
		t.Fatalf("stale hit served a refreshed body: %q vs %q", body, staleBody)
	}

	waitFor(t, 3*time.Second, func() bool {
		return cachedETag("merge=true") != "" && cachedETag("merge=true") != staleHeader.Get("ETag")
	}, "background refresh to update the result cache")

	if got := source.fetches.Load(); got != 2 {
		t.Fatalf("source fetches = %d, want 2 (one cold start plus one background refresh)", got)
	}
	waitForRefreshFlag(t, "merge=true", false)

	status, body, _ = fetchPlaylist(t, server, "", "")
	if status != http.StatusOK || !strings.Contains(body, "CCTV-2") {
		t.Fatalf("post-refresh status = %d, body = %q", status, body)
	}
	waitForRefreshFlag(t, "merge=true", false)
}

func TestStaleHitsShareOneBackgroundRefresh(t *testing.T) {
	resetState(t)

	source := newCountingSource(t, duplicatedPlaylistBody("CCTV-1"))
	server := httptest.NewServer(handler(writeTestConfig(t, source.URL, "0s", "30ms")))
	defer server.Close()

	if _, _, _ = fetchPlaylist(t, server, "", ""); source.fetches.Load() != 1 {
		t.Fatal("cold start did not fetch the source")
	}

	source.setBody(duplicatedPlaylistBody("CCTV-2"))
	source.delay.Store(int64(200 * time.Millisecond))
	time.Sleep(50 * time.Millisecond)

	coldETag := cachedETag("merge=true")
	for range 5 {
		if status, _, _ := fetchPlaylist(t, server, "", ""); status != http.StatusOK {
			t.Fatalf("stale hit status = %d, want %d", status, http.StatusOK)
		}
	}

	waitFor(t, 3*time.Second, func() bool {
		etag := cachedETag("merge=true")
		return etag != "" && etag != coldETag
	}, "the single background refresh to finish")
	if got := source.fetches.Load(); got != 2 {
		t.Fatalf("source fetches = %d, want 2 (five stale hits must share one refresh)", got)
	}

	waitForRefreshFlag(t, "merge=true", false)
	source.delay.Store(0)
	time.Sleep(50 * time.Millisecond)

	if _, _, _ = fetchPlaylist(t, server, "", ""); !refreshFlagSet("merge=true") {
		t.Fatal("a later stale hit must be able to schedule a new refresh")
	}
	waitForRefreshFlag(t, "merge=true", false)
}

func refreshFlagSet(key string) bool {
	refreshLock.Lock()
	defer refreshLock.Unlock()
	return refreshRunning[key]
}

func waitForRefreshFlag(t testing.TB, key string, want bool) {
	t.Helper()

	waitFor(t, 3*time.Second, func() bool {
		return refreshFlagSet(key) == want
	}, fmt.Sprintf("refresh flag for %s to become %t", key, want))
}

func TestBypassCacheIgnoresIfNoneMatch(t *testing.T) {
	resetState(t)

	source := newCountingSource(t, playlistBody("CCTV-1"))
	server := httptest.NewServer(handler(writeTestConfig(t, source.URL, "0s", "1m")))
	defer server.Close()

	coldStatus, coldBody, header := fetchPlaylist(t, server, "", "")
	if coldStatus != http.StatusOK || coldBody == "" {
		t.Fatalf("cold start status = %d, body length = %d, want 200 with body", coldStatus, len(coldBody))
	}
	if got := source.fetches.Load(); got != 1 {
		t.Fatalf("cold start source fetches = %d, want 1", got)
	}

	status, body, _ := fetchPlaylist(t, server, "cache=false", header.Get("ETag"))
	if status != http.StatusOK || body == "" {
		t.Fatalf("bypass status = %d, body length = %d, want 200 with body", status, len(body))
	}
	if got := source.fetches.Load(); got != 2 {
		t.Fatalf("source fetches = %d, want 2 (cache=false must reload sources)", got)
	}
}

func TestMergeFalseUsesSeparateCacheKey(t *testing.T) {
	resetState(t)

	source := newCountingSource(t, duplicatedPlaylistBody("CCTV-1"))
	server := httptest.NewServer(handler(writeTestConfig(t, source.URL, "0s", "1m")))
	defer server.Close()

	mergedStatus, mergedBody, mergedHeader := fetchPlaylist(t, server, "", "")
	if mergedStatus != http.StatusOK {
		t.Fatalf("merged status = %d, want %d", mergedStatus, http.StatusOK)
	}

	status, body, header := fetchPlaylist(t, server, "merge=false", mergedHeader.Get("ETag"))
	if status != http.StatusOK {
		t.Fatalf("merge=false status = %d, want %d", status, http.StatusOK)
	}
	if body == mergedBody || header.Get("ETag") == mergedHeader.Get("ETag") {
		t.Fatal("merge=false must render and cache its own playlist variant")
	}
	if cachedEntry("merge=true") == nil || cachedEntry("merge=false") == nil {
		t.Fatal("both merge cache keys should be populated")
	}
}

func TestEtagForIsContentBased(t *testing.T) {
	body := []byte("#EXTM3U\n")
	if etagFor(body) != etagFor([]byte("#EXTM3U\n")) {
		t.Fatal("etag must be stable for identical content")
	}
	if etagFor(body) == etagFor([]byte("#EXTM3U\n#EXTINF:-1,x\n")) {
		t.Fatal("etag must change when content changes")
	}
	if got := etagFor(body); !strings.HasPrefix(got, `"`) || !strings.HasSuffix(got, `"`) {
		t.Fatalf("etag = %s, want quoted form", got)
	}
}

func TestETagMatches(t *testing.T) {
	etag := `"abc123"`
	cases := []struct {
		header string
		want   bool
	}{
		{header: "", want: false},
		{header: "*", want: true},
		{header: etag, want: true},
		{header: "W/" + etag, want: true},
		{header: `"zzz", ` + etag, want: true},
		{header: `"zzz"`, want: false},
	}

	for _, testCase := range cases {
		if got := etagMatches(testCase.header, etag); got != testCase.want {
			t.Errorf("etagMatches(%q, %s) = %t, want %t", testCase.header, etag, got, testCase.want)
		}
	}
}

func TestChannelDefFormsResolveEPGIDs(t *testing.T) {
	resetState(t)

	dir := t.TempDir()
	playlist := filepath.Join(dir, "source.m3u")
	if err := os.WriteFile(playlist, []byte(
		"#EXTM3U\n"+
			"#EXTINF:-1 tvg-id=\"foreign.id\" tvg-name=\"CCTV 1\",CCTV-1 综合\nhttp://example.com/cctv1.m3u8\n"+
			"#EXTINF:-1,湖南卫视\nhttp://example.com/hunan.m3u8\n"+
			"#EXTINF:-1 tvg-id=\"src.hn\" tvg-name=\"HN源名\",湖南卫视\nhttp://example.com/hunan2.m3u8\n"+
			"#EXTINF:-1 tvg-id=\"keep.me\",CCTV-测试\nhttp://example.com/keep.m3u8\n",
	), 0o600); err != nil {
		t.Fatalf("write playlist: %v", err)
	}

	configPath := filepath.Join(dir, "config.yaml")
	content := fmt.Sprintf(
		"urls:\n  - %q\nchannel-groups:\n  - name: test\n    channels:\n      \"CCTV-1\": {tvg-id: CCTV1, regex: [{name: '^CCTV-1'}]}\n      \"湖南卫视\": [{name: '^湖南卫视$'}]\n      \"测试频道\": {tvg-name: 显式测试名, tvg-logo: \"http://example.com/logo.png\", regex: [{name: '^CCTV-测试$'}]}\n",
		playlist,
	)
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	output, _, err := generateAllM3U(configPath, true, true)
	if err != nil {
		t.Fatalf("generate m3u: %v", err)
	}

	for _, want := range []string{
		// tvg-id overridden by config; tvg-name keeps the source value (attributes are independent)
		`#EXTINF:-1 tvg-id="CCTV1" tvg-name="CCTV 1" group-title="test",CCTV-1`,
		// no explicit values: merged group takes the first non-empty source tvg-id/tvg-name top-down
		`#EXTINF:-1 tvg-id="src.hn" tvg-name="HN源名" group-title="test",湖南卫视`,
		// explicit tvg-name/tvg-logo override; unconfigured tvg-id passes through from the source
		`#EXTINF:-1 tvg-id="keep.me" tvg-name="显式测试名" tvg-logo="http://example.com/logo.png" group-title="test",测试频道`,
	} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing line:\n  want: %s\n  got:\n%s", want, output)
		}
	}

	// mapping form without regex must fail config load
	badPath := filepath.Join(dir, "bad.yaml")
	badContent := fmt.Sprintf("urls:\n  - %q\nchannel-groups:\n  - name: test\n    channels:\n      \"CCTV-1\": {tvg-id: CCTV1}\n", playlist)
	if err := os.WriteFile(badPath, []byte(badContent), 0o600); err != nil {
		t.Fatalf("write bad config: %v", err)
	}
	if _, err := loadConfig(badPath); err == nil {
		t.Fatal("channel definition without regex rules must fail config load")
	}

	// the shipped example must compile (regexes, dual channel forms)
	if _, err := loadConfig("config.example.yaml"); err != nil {
		t.Fatalf("load config.example.yaml: %v", err)
	}
}

func TestLoadConfigCachedReusesUntilFileChanges(t *testing.T) {
	resetState(t)

	source := newCountingSource(t, playlistBody("CCTV-1"))
	configPath := writeTestConfig(t, source.URL, "0s", "1m")

	first, err := loadConfigCached(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	second, err := loadConfigCached(configPath)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if first != second {
		t.Fatal("unchanged config file must reuse the compiled config")
	}

	otherPath := writeTestConfig(t, source.URL, "0s", "10m")
	other, err := loadConfigCached(otherPath)
	if err != nil {
		t.Fatalf("load other config: %v", err)
	}
	if other.settings.ResultCacheTTL != 10*time.Minute {
		t.Fatalf("other path ttl = %s, want 10m", other.settings.ResultCacheTTL)
	}
	reloadedOther, err := loadConfigCached(otherPath)
	if err != nil {
		t.Fatalf("reload other config: %v", err)
	}
	if reloadedOther != other {
		t.Fatal("unchanged config file must reuse the compiled config")
	}

	rewritten := "urls:\n  - \"" + source.URL + "\"\n  - \"http://example.com/extra.m3u\"\nchannel-groups:\n  - name: test\n    channels:\n      \"*\":\n        - name: .*\n"
	if err := os.WriteFile(otherPath, []byte(rewritten), 0o600); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}
	updated, err := loadConfigCached(otherPath)
	if err != nil {
		t.Fatalf("load rewritten config: %v", err)
	}
	if updated == other {
		t.Fatal("changed config file must invalidate the compiled config")
	}
	if len(updated.urls) != 2 {
		t.Fatalf("urls = %d, want 2", len(updated.urls))
	}

	if _, err := loadConfigCached(filepath.Join(filepath.Dir(otherPath), "missing.yaml")); err == nil {
		t.Fatal("missing config file must return an error")
	}
}

func TestGenerateAndCacheM3UStoresBytes(t *testing.T) {
	resetState(t)

	source := newCountingSource(t, playlistBody("CCTV-1"))
	entry, err := generateAndCacheM3U(writeTestConfig(t, source.URL, "0s", "1m"), false, true)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if entry.etag != etagFor(entry.body) {
		t.Fatalf("entry ETag = %q, want %q", entry.etag, etagFor(entry.body))
	}
	if cachedEntry("merge=true") != entry {
		t.Fatal("generated entry must be stored in the result cache")
	}
	if entry.expiryTime.Before(time.Now()) {
		t.Fatalf("cached expiry = %s, want a future time", entry.expiryTime)
	}
}

func TestAsyncLogWriterFlushesOnCloseAndFallsBackAfterwards(t *testing.T) {
	buffer := &syncBuffer{}
	writer := newAsyncLogWriterTo(buffer)
	line := []byte("iptv-merge buffered log line\n")

	if got, err := writer.Write(line); got != len(line) || err != nil {
		t.Fatalf("Write = %d, %v, want %d, nil", got, err, len(line))
	}
	writer.Close()
	if got := buffer.String(); !strings.Contains(got, "buffered log line") {
		t.Fatalf("buffered destination = %q, want the queued line", got)
	}

	if got, err := writer.Write(line); got != len(line) || err != nil {
		t.Fatalf("post-close Write = %d, %v, want %d, nil", got, err, len(line))
	}
	if count := strings.Count(buffer.String(), "buffered log line"); count != 2 {
		t.Fatalf("queued lines = %d, want 2", count)
	}
}

type syncBuffer struct {
	lock sync.Mutex
	buf  bytes.Buffer
}

func (buffer *syncBuffer) Write(p []byte) (int, error) {
	buffer.lock.Lock()
	defer buffer.lock.Unlock()
	return buffer.buf.Write(p)
}

func (buffer *syncBuffer) String() string {
	buffer.lock.Lock()
	defer buffer.lock.Unlock()
	return buffer.buf.String()
}

func newCacheHitBenchmark(b *testing.B) *httptest.Server {
	b.Helper()

	body := playlistBody("CCTV-1")
	for index := 0; index < 300; index++ {
		body += playlistBody(fmt.Sprintf("BENCH-%d", index))
	}

	source := newCountingSource(b, body)
	resetState(b)
	server := httptest.NewServer(handler(writeTestConfig(b, source.URL, "0s", "1m")))
	b.Cleanup(server.Close)

	if status, _, _ := fetchPlaylist(b, server, "", ""); status != http.StatusOK {
		b.Fatalf("cold start status = %d, want 200", status)
	}

	refreshLock.Lock()
	refreshRunning["merge=true"] = true
	refreshLock.Unlock()
	processedCacheLock.Lock()
	processedCache["merge=true"].expiryTime = time.Now().Add(-time.Second)
	processedCacheLock.Unlock()

	if len(cachedEntry("merge=true").body) < 1000 {
		b.Fatalf("cached body length = %d, want a multi-chunk playlist", len(cachedEntry("merge=true").body))
	}
	return server
}

func BenchmarkStaleCacheHit(b *testing.B) {
	server := newCacheHitBenchmark(b)

	client := server.Client()
	url := server.URL + "/all.m3u"
	b.SetBytes(int64(len(cachedEntry("merge=true").body)))
	b.ReportAllocs()
	b.ResetTimer()

	for index := 0; index < b.N; index++ {
		response, err := client.Get(url)
		if err != nil {
			b.Fatalf("get: %v", err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
	}
}

func BenchmarkStaleCacheHitNotModified(b *testing.B) {
	server := newCacheHitBenchmark(b)
	_, _, header := fetchPlaylist(b, server, "", "")

	client := server.Client()
	url := server.URL + "/all.m3u"
	b.ReportAllocs()
	b.ResetTimer()

	for index := 0; index < b.N; index++ {
		request, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			b.Fatalf("build request: %v", err)
		}
		request.Header.Set("If-None-Match", header.Get("ETag"))
		response, err := client.Do(request)
		if err != nil {
			b.Fatalf("do: %v", err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
	}
}
