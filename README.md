# Raydio

Raydio is a multi-station web radio system written in Go. It turns configured
MP3 directories into shared, always-moving radio timelines. Every listener that
opens the same `/radio/<uuid-or-alias>` path hears the same track at the same
position, based on server time.

Raydio runs as three processes:

- `raydio-worker` scans the inbox, performs ffmpeg preprocessing, and uses
  ffprobe for normalized cache MP3 validation.
- `suno-worker` optionally syncs configured Suno playlists into each station
  inbox.
- `raydio` serves HTTP, APIs, and cached MP3 frame ranges.

The HTTP server never runs ffmpeg or ffprobe and does not decode audio while
streaming. Source files are normalized once by the worker into clean
constant-bitrate MP3 files, then the HTTP stream sends MP3 frame ranges directly
from disk.

## Features

- Per-station live radio timeline shared by listeners of that station.
- Built-in `/radio/all` station that randomly plays from all configured stations.
- Background schedule maintenance, even when nobody is listening.
- Separate media worker for scanner, ffmpeg, cache validation, and explicit
  cover asset copying.
- Infinite `audio/mpeg` streams at `GET /radio/<uuid-or-alias>`.
- Browser player at `GET /` with play, pause, volume, current track, cover, and
  status display.
- SQLite catalog and persistent schedule.
- Worker-owned ffmpeg preprocessing into clean MP3:
  - 48 kHz
  - stereo
  - 192 kbps CBR
  - no ID3 header
  - no Xing header
  - fixed 576-byte MP3 frames
- Event-driven worker directory scanner with periodic full-scan fallback.
- Optional Suno playlist sync worker.
- Sidecar JSON metadata support for track title and artist.
- Sidecar cover support for `.jpg`, `.jpeg`, `.png`, and `.webp` files.
- Silence fallback when the catalog is empty.

## Requirements

- Go 1.26 or newer.
- `ffmpeg` and `ffprobe` available on `PATH` for `raydio-worker`.
- CGO enabled for SQLite.
- A C compiler usable by Go. In the provided devcontainer, `go env CC` points to
  `zig cc -target x86_64-linux-gnu`.

Third-party Go dependencies:

```text
github.com/mattn/go-sqlite3 v1.14.44
github.com/fsnotify/fsnotify v1.10.1
golang.org/x/sync v0.21.0
golang.org/x/time v0.15.0
go.yaml.in/yaml/v4 v4.0.0-rc.5
```

## Quick Start

Create a local config file:

```bash
cp config.example.yaml config.yaml
```

Start the worker in one terminal:

```bash
CGO_ENABLED=1 go run ./cmd/raydio-worker
```

Optionally start the Suno sync worker in another terminal:

```bash
CGO_ENABLED=1 go run ./cmd/suno-worker
```

Start the HTTP server in another terminal:

```bash
CGO_ENABLED=1 go run ./cmd/raydio
```

Open:

```text
http://localhost:8080/
```

Add MP3 files to:

```text
./data/inbox/<radio-uuid>
```

`raydio-worker` scans on startup, watches inbox directories for filesystem
events, and still performs a full rescan every 30 seconds by default.
`raydio` maintains the future radio schedule every minute by default, even with
zero listeners. Converted audio, covers, the silence file, and the SQLite
database are stored under `./data`.

`raydio` expects the worker to have prepared the cache and silence MP3. If the
cache is missing, the server fails startup with guidance to run `raydio-worker`
against the same data directory.

## Docker Compose

The Compose setup builds three local images and runs every container as
UID/GID `65532`, with no root runtime user. `raydio` and `suno-worker` use
distroless runtime images. `raydio-worker` uses Debian slim because it must run
`ffmpeg` and `ffprobe`.

Start the stack:

```bash
docker compose up --build
```

`cloudflare/cloudflared` starts a quick tunnel to `http://raydio:8080`. Read the
public `trycloudflare.com` URL from:

```bash
docker compose logs -f cloudflared
```

The Docker config lives in `config.docker.yaml`. Its `server.addr` is `:8080`
so the server listens inside the Compose network, and
`server.trusted_proxy_cidrs` trusts the Docker bridge subnet so Raydio can use
`CF-Connecting-IP`/`X-Forwarded-For` from cloudflared. The HTTP server is not
published on a host port by default; public access goes through the quick
tunnel.

Runtime data is stored in the named volume `raydio-data`. The worker inbox is
`/srv/raydio/data/inbox/<radio-uuid>` inside that volume.

## Command-Line Flags

All binaries intentionally expose only the config path and help flags:

| Flag | Default | Description |
| --- | --- | --- |
| `-config` | `config.yaml` | Configuration file path. |
| `-h`, `-help` | none | Print help. |

All operational settings live in the config file.

Example:

```bash
CGO_ENABLED=1 go run ./cmd/raydio-worker -config /srv/raydio/config.yaml
CGO_ENABLED=1 go run ./cmd/suno-worker -config /srv/raydio/config.yaml
CGO_ENABLED=1 go run ./cmd/raydio -config /srv/raydio/config.yaml
```

## Configuration File

`config.yaml` is ignored by Git because it is the local runtime configuration.
Start from the commented example:

```bash
cp config.example.yaml config.yaml
```

Supported keys:

| Key | Default | Description |
| --- | --- | --- |
| `data_dir` | `./data` | Root data directory. |
| `gap_frames` | `209` | Silence frames inserted between tracks. |
| `log_level` | `DEBUG` | Minimum structured log level for all binaries. |
| `radios[].alias` | none | Human-readable radio path alias. Lowercase letters, numbers, and hyphens only. `all` is reserved for the built-in aggregate station. |
| `radios[].uuid` | none | Canonical radio UUID. Worker scans `<worker.inbox_dir>/<uuid>`. |
| `server.addr` | `:8080` | HTTP listen address for `raydio`. |
| `server.rate_limit_rps` | `10` | Per-client-IP HTTP request rate limit. `/healthz` is exempt. |
| `server.rate_limit_burst` | `30` | Per-client-IP burst size for HTTP rate limiting. |
| `server.max_streams_per_ip` | `4` | Maximum concurrent MP3 streams per client IP. |
| `server.max_events_per_ip` | `8` | Maximum concurrent SSE event streams per client IP. |
| `server.trusted_proxy_cidrs` | `[]` | Reverse proxy CIDRs or exact IPs whose client-IP headers are trusted. |
| `server.client_ip_headers` | `["CF-Connecting-IP", "X-Forwarded-For", "X-Real-IP"]` | Header order used only for trusted proxies. |
| `server.schedule_interval` | `1m` | Background schedule maintenance interval. |
| `server.stream_chunk_window` | `480ms` | Shared audio chunk size produced once and fanned out to listeners. |
| `server.stream_buffer_window` | `2s` | Live catch-up window for slow listeners. |
| `server.stream_write_timeout` | `5s` | Maximum blocking time for a listener write. |
| `suno.sync_interval` | `30m` | How often `suno-worker` syncs configured Suno playlists. |
| `suno.http_timeout` | `30s` | HTTP timeout for Suno playlist and media downloads. |
| `suno.max_audio_bytes` | `134217728` | Maximum bytes accepted for one downloaded Suno MP3. |
| `suno.max_cover_bytes` | `16777216` | Maximum bytes accepted for one downloaded Suno cover image. |
| `worker.inbox_dir` | empty | Base source MP3 directory. Empty means `<data_dir>/inbox`. |
| `worker.rescan_interval` | `30s` | Directory rescan interval. |

At 48 kHz MP3, one frame is 24 ms. The default `209`-frame gap is about
5.016 seconds.

Duration values use Go duration syntax, such as `500ms`, `15s`, `1m`, or `1h`.
Relative `data_dir` and `worker.inbox_dir` paths are resolved from the directory
that contains the config file. `radios` must contain at least one entry.
Log level values are `DEBUG`, `INFO`, `WARN`, or `ERROR`.

## Logging

Raydio uses Go `slog` structured text logs on stderr. All binaries use the same
`log_level` value from the config file. The example config defaults to `DEBUG`
so development runs include startup settings, schedule ticks, stream lifecycle
events, and worker scan summaries.

## Input Files and Metadata

`raydio-worker` processes stable `.mp3` files under each configured
`<worker.inbox_dir>/<uuid>` directory. It watches the station inbox and
non-hidden subdirectories with `fsnotify`, debounces changes per station, and
keeps `worker.rescan_interval` as a full-scan fallback for missed events or
filesystems without reliable notifications. Hidden paths, `.tmp` files, and
`.part` files are ignored.

Raydio does not read source MP3 tags, embedded cover art, or lyric files.
Imported track metadata comes from same-name JSON sidecars:

- `title`: sidecar `title`, falling back to source filename without extension.
- `artist`: sidecar `artist`, falling back to `Unknown artist`.
- `album`: empty.

Supported sidecar files:

```text
song.mp3
song.json
song.jpg
song.jpeg
song.png
song.webp
```

JSON sidecars are imported as track metadata. Image sidecars are imported as
cover assets.

### Suno Sync

`suno-worker` treats each configured `radios[].uuid` as a Suno playlist UUID and
requests:

```text
https://studio-api-prod.suno.com/api/playlist/<uuid>
```

Only clips with `clip.status == "complete"` are downloaded. For each clip,
`suno-worker` writes the MP3, cover image, and JSON metadata sidecar into
`<worker.inbox_dir>/<uuid>`. It keeps a `.suno-manifest.json` file for internal
bookkeeping and removes only stale files it previously managed; manual inbox
files are not deleted.
`raydio-worker` then imports those files through the normal scanner.

## HTTP Endpoints

| Endpoint | Description |
| --- | --- |
| `GET /` | Browser player. |
| `GET /api/stations` | Configured station aliases and UUIDs, plus the built-in `all` aggregate station. |
| `GET /radio/{uuid-or-alias}` | Infinite MP3 stream for one station. |
| `GET /radio/{uuid-or-alias}/api/now` | Current server time, slot, track, elapsed time, and duration. |
| `GET /radio/{uuid-or-alias}/api/events` | Server-Sent Events stream for track changes. |
| `GET /radio/{uuid-or-alias}/api/catalog` | Paginated current station catalog state. |
| `GET /radio/{uuid-or-alias}/covers/{trackID}` | Cover asset for a track, when present. |
| `GET /healthz` | Plain `ok` health check. |

The browser player lists `all` first and selects it by default. Use
`/?raydio=<alias-or-uuid>` to open a specific station; a missing or unknown
`raydio` value is normalized to `/?raydio=all` while preserving other query
parameters.

`/radio/{uuid-or-alias}/api/catalog` is paginated. Use `limit` to request up to
500 tracks and pass the returned `nextCursor` as `cursor` to read the next page.
Responses include `revision`, `tracks`, `nextCursor`, and `hasMore`, and support
`ETag` revalidation.

`/radio/{uuid-or-alias}` sends:

```http
Content-Type: audio/mpeg
Cache-Control: no-store, no-transform
X-Accel-Buffering: no
Accept-Ranges: none
```

It intentionally has no `Content-Length` and does not support seeking.

## Design

### Audio preprocessing

Source MP3 files can be VBR, can contain metadata headers, and can have variable
frame sizes. That makes direct time-to-byte seeking unreliable. `raydio-worker`
therefore normalizes every source file once with ffmpeg:

```bash
ffmpeg -nostdin -hide_banner -loglevel error \
  -i "$INPUT" \
  -map 0:a:0 -vn -sn -dn \
  -ac 2 -ar 48000 \
  -c:a libmp3lame -b:a 192k -reservoir 0 \
  -map_metadata -1 \
  -id3v2_version 0 -write_xing 0 \
  -f mp3 "$OUTPUT"
```

The converted file is accepted only if validation proves:

- 48 kHz sample rate.
- 2 channels.
- 192000 bit/s.
- No leading `ID3` tag.
- Every audio frame is 576 bytes.

This makes byte seeking simple:

```text
byteOffset = frameIndex * 576
```

### Scheduler

Raydio stores schedule slots in SQLite. Slots are scoped by station UUID. A slot
is either a track or silence:

```text
track A -> silence gap -> track B -> silence gap -> ...
```

Each station scheduler fills future slots ahead of time. The main service radio
engine keeps these schedules extended while the process is running, even if
there are no active listeners. Track order uses a shuffle bag. When more than
one track exists, Raydio avoids choosing the same track for adjacent track slots.
The built-in `all` station has its own shared schedule and picks uniformly among
non-empty configured stations before shuffling within the chosen station.

The radio engine loads schedule and catalog snapshots into memory on a fixed
background interval. Request handlers read those snapshots instead of querying
SQLite per listener. If the current slot is unexpectedly missing, only the
engine performs a fallback refill and refresh; individual listeners never
trigger schedule work.

When a source file is removed:

- The track is marked missing.
- It is not scheduled again.
- Future schedule slots are refreshed.
- Already-playing cached audio can finish.

When a station catalog is empty, that scheduler emits silence slots and
`/radio/<uuid-or-alias>` continues streaming valid MP3 audio.

When all listeners disconnect, no audio is played in the background. Raydio only
continues maintaining schedule rows. The next listener joins the current
wall-clock slot and frame.

### Worker and Server Split

`raydio-worker` owns every expensive or externally executed media task:

- fsnotify inbox watching, full scans, and file stability checks
- source hashing
- ffmpeg transcode
- clean cache MP3 validation with ffprobe
- silence MP3 generation
- sidecar cover copy
- track and asset DB updates
- future schedule invalidation after catalog changes

`raydio` owns only:

- HTTP server
- one shared stream producer and listener fanout per station
- metadata APIs and static assets
- background schedule maintenance

Both processes use the same SQLite database and the same data/cache directory.
This lets deployments apply Docker CPU/memory/I/O limits to `raydio-worker`
without throttling active listeners.

### Streaming

The main service has one stream producer per station. Every
`server.stream_chunk_window`, each producer reads the current MP3 frame range
once from disk and publishes the immutable bytes into a shared in-memory ring
buffer. Each listener only tracks a sequence cursor into that station's ring.
Adding listeners does not add SQLite queries, schedule calculations, or disk
reads.

Slow listeners can fall behind by up to `server.stream_buffer_window`. After
that, they skip old chunks and rejoin the current live stream. A write blocked
longer than `server.stream_write_timeout` closes the connection.

## SQLite Storage

The database is stored at:

```text
<data>/raydio.sqlite
```

The store enables:

```text
WAL
busy_timeout=5000
foreign_keys=ON
synchronous=NORMAL
```

Main tables:

- `stations`
- `tracks`
- `track_assets`
- `schedule_slots`
- `catalog_state`

Schema creation is fresh-only. Empty databases are initialized directly with the
current schema. Databases containing historical migration tables such as
`goose_db_version`, the removed `settings` table, or any unknown application
tables fail startup with an operator-facing error. Remove or recreate the
database to start with the current fresh schema.

## Playback From Terminal

```bash
curl -sN http://localhost:8080/radio/monthly | ffplay -hide_banner -nodisp -f mp3 -
```

## Reverse Proxy Notes

If `/radio/<uuid-or-alias>` is served behind a reverse proxy, response buffering
must be disabled for that route.

Rate limiting keys by client IP. By default Raydio ignores `CF-Connecting-IP`,
`X-Forwarded-For`, and `X-Real-IP` because clients can spoof those headers when
they can reach the origin directly. To use real client IPs behind Nginx,
cloudflared, Cloudflare, or another trusted proxy, configure the immediate
proxy source addresses:

```yaml
server:
  trusted_proxy_cidrs: ["127.0.0.1/32", "::1/128"]
  client_ip_headers: ["CF-Connecting-IP", "X-Forwarded-For", "X-Real-IP"]
```

For Docker or private reverse proxies, include the proxy subnet, such as
`172.16.0.0/12`. For Cloudflare direct-to-origin deployments, either configure
Cloudflare source CIDRs or firewall the origin so only Cloudflare can reach it.
When the immediate peer is not trusted, Raydio falls back to `RemoteAddr`.

Nginx example:

```nginx
location /radio/ {
    proxy_pass http://127.0.0.1:8080;
    proxy_http_version 1.1;
    proxy_buffering off;
    proxy_request_buffering off;
    gzip off;
    proxy_read_timeout 1h;
    proxy_send_timeout 1h;
    add_header Cache-Control "no-store, no-transform";
    add_header X-Accel-Buffering "no";
}
```

## Development

Run tests:

```bash
CGO_ENABLED=1 go test ./...
CGO_ENABLED=1 go test -race ./...
CGO_ENABLED=1 go vet ./...
```

Run with sample files:

```bash
mkdir -p /tmp/raydio-demo/data/inbox/00000000-0000-0000-0000-000000000001
cp tmp/origin/*.mp3 /tmp/raydio-demo/data/inbox/00000000-0000-0000-0000-000000000001/
cat >/tmp/raydio-demo/config.yaml <<'YAML'
data_dir: data
gap_frames: 209
log_level: DEBUG
radios:
  - alias: monthly
    uuid: "00000000-0000-0000-0000-000000000001"
server:
  addr: "127.0.0.1:18080"
  rate_limit_rps: 10
  rate_limit_burst: 30
  max_streams_per_ip: 4
  max_events_per_ip: 8
  trusted_proxy_cidrs: []
  client_ip_headers: ["CF-Connecting-IP", "X-Forwarded-For", "X-Real-IP"]
  schedule_interval: 1m
  stream_chunk_window: 480ms
  stream_buffer_window: 2s
  stream_write_timeout: 5s
suno:
  sync_interval: 30m
  http_timeout: 30s
  max_audio_bytes: 134217728
  max_cover_bytes: 16777216
worker:
  inbox_dir: ""
  rescan_interval: 30s
YAML
CGO_ENABLED=1 go run ./cmd/raydio-worker -config /tmp/raydio-demo/config.yaml
CGO_ENABLED=1 go run ./cmd/raydio -config /tmp/raydio-demo/config.yaml
```

Inspect catalog:

```bash
curl -s http://127.0.0.1:18080/radio/monthly/api/catalog
```

Capture a short stream sample:

```bash
curl --max-time 3 -sN http://127.0.0.1:18080/radio/monthly -o /tmp/raydio-sample.mp3
ffprobe -v error -show_format -show_streams /tmp/raydio-sample.mp3
```

## Limitations

- Single-instance only.
- Single catalog worker only.
- No admin UI.
- No write API for metadata.
- No HLS.
- No ICY metadata blocks in the MP3 stream.
- No seeking, previous-track, or next-track controls.
- No Vue, Vite, or Node-based frontend build chain.
