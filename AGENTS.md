# Agent Notes

## Repo Shape
- Single Go command in `main.go`; `go.mod` declares module `iptv-merge` with Go `1.24.1`.
- There are no subpackages, test files, README, CI workflows, lint/typecheck configs, or repo-local OpenCode config; this file is the only agent instruction file found.

## Commands
- `go test ./...` is the verified check; currently it reports `[no test files]` and mainly compiles the command.
- `go test .` is the focused check for the only package.
- `go run . -h` shows the only CLI flags: `-c <path>`, `-c=<path>`, and `-h`.
- `go run . -c config.example.yaml` starts the server with sample config; `go run .` expects ignored `config.yaml`.
- `make all` cross-builds only `linux_armv7` and `darwin_arm64` with `CGO_ENABLED=0` into ignored `build/`; use `make linux_armv7`, `make darwin_arm64`, or `make clean` for one target/cleanup.

## Runtime And Config
- Default port is `7777`, and the only served route is `/all.m3u`.
- Query flags change behavior: `cache=false` bypasses source/result caches, and `merge=false` prevents grouping multiple URLs under the same output channel.
- Startup only reads server settings; full config validation and source loading happen when `/all.m3u` is requested.
- `config.yaml`, root `*.m3u`, `build/`, and `go.sum` are ignored; treat local playlists/configs as runtime data, not source fixtures or commit targets, unless the user asks.
- `urls` may be HTTP(S) URLs or local file paths; `config.example.yaml` references ignored root playlists `tv.m3u`, `sdyd.m3u`, and `ipv6.m3u`.
- `server.*_timeout` and cache TTL values are Go duration strings parsed by `time.ParseDuration`.
- M3U parsing keeps `#EXTGRP`, `#EXTVLCOPT:http-referrer`, and `#EXTVLCOPT:http-user-agent`; output re-emits those VLC options.

## Channel Matching
- `channel-groups[].channels` YAML mapping order is preserved and controls output order; keep intended channel order in YAML instead of sorting keys.
- Channel rule lists are OR; fields within one rule map are AND; every field value is a Go regexp, and invalid or empty rules fail config load.
- The `"*"` channel key is fallback-only after explicit channel matches fail; it is not part of explicit channel ordering.
- Rule field names are canonicalized before matching, so aliases such as `title`, `channel`, and `display_name` map to `name`, while `group-title` and `group_name` map to `group`.
