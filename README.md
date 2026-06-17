# Raydio

Raydio is a single-instance web radio server written in Go. It turns a directory
of MP3 files into one shared, always-moving radio timeline. Every listener that
opens `/radio` hears the same track at the same position, based on server time.

The server does not run ffmpeg per listener and does not decode audio while
streaming. Source files are normalized once into clean constant-bitrate MP3
files, then the HTTP stream sends MP3 frame ranges directly from disk.

## Features

- Global live radio timeline shared by all listeners.
- Background schedule maintenance, even when nobody is listening.
- Infinite `audio/mpeg` stream at `GET /radio`.
- Browser player at `GET /` with play, pause, volume, current track, cover, and
  lyric display.
- SQLite catalog and persistent schedule.
- Offline ffmpeg preprocessing into clean MP3:
  - 48 kHz
  - stereo
  - 192 kbps CBR
  - no ID3 header
  - no Xing header
  - fixed 576-byte MP3 frames
- Directory scanner for adding and removing tracks.
- Sidecar metadata support for title, artist, album, lyrics, and cover art.
- Silence fallback when the catalog is empty.

## Requirements

- Go 1.26 or newer.
- `ffmpeg` and `ffprobe` available on `PATH`.
- CGO enabled for SQLite.
- A C compiler usable by Go. In the provided devcontainer, `go env CC` points to
  `zig cc -target x86_64-linux-gnu`.

The only third-party Go dependency is:

```text
github.com/mattn/go-sqlite3 v1.14.44
```

## Quick Start

Build and run:

```bash
CGO_ENABLED=1 go run ./cmd/raydio
```

Open:

```text
http://localhost:8080/
```

Add MP3 files to:

```text
./data/inbox
```

Raydio scans on startup and then rescans every 30 seconds by default. It also
maintains the future radio schedule every minute by default, even with zero
listeners. Converted audio, covers, lyrics, the silence file, and the SQLite
database are stored under `./data`.

## Command-Line Flags

| Flag | Environment variable | Default | Description |
| --- | --- | --- | --- |
| `-addr` | `RAYDIO_ADDR` | `:8080` | HTTP listen address. |
| `-data` | `RAYDIO_DATA` | `./data` | Root data directory. |
| `-inbox` | `RAYDIO_INBOX` | `<data>/inbox` | Source MP3 directory. |
| `-rescan` | `RAYDIO_RESCAN` | `30s` | Directory rescan interval. |
| `-schedule` | `RAYDIO_SCHEDULE` | `1m` | Background schedule maintenance interval. |
| `-gap-frames` | `RAYDIO_GAP_FRAMES` | `209` | Silence frames inserted between tracks. |

At 48 kHz MP3, one frame is 24 ms. The default `209`-frame gap is about
5.016 seconds.

Example:

```bash
CGO_ENABLED=1 go run ./cmd/raydio \
  -addr 127.0.0.1:8080 \
  -data /srv/raydio \
  -rescan 15s \
  -schedule 1m
```

## Input Files and Metadata

Raydio processes stable `.mp3` files in the inbox directory. Hidden files,
`.tmp` files, and `.part` files are ignored.

Metadata priority:

1. Sidecar files next to the MP3.
2. Embedded tags and embedded cover art.
3. Fallback values from the filename and `Unknown artist`.

Sidecar JSON example:

```json
{
  "title": "Track Title",
  "artist": "Artist Name",
  "album": "Album Name",
  "comment": "Optional note"
}
```

Supported sidecar files:

```text
song.mp3
song.json
song.lrc
song.jpg
song.jpeg
song.png
song.webp
```

Lyrics use LRC timestamps, for example:

```text
[00:12.000]First lyric line
[00:18.500]Second lyric line
```

## HTTP Endpoints

| Endpoint | Description |
| --- | --- |
| `GET /` | Browser player. |
| `GET /radio` | Infinite MP3 stream. |
| `GET /api/now` | Current server time, slot, track, elapsed time, and duration. |
| `GET /api/events` | Server-Sent Events stream for track changes. |
| `GET /api/catalog` | Current catalog state. |
| `GET /covers/{trackID}` | Cover asset for a track, when present. |
| `GET /lyrics/{trackID}` | LRC lyric asset for a track, when present. |
| `GET /healthz` | Plain `ok` health check. |

`/radio` sends:

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
frame sizes. That makes direct time-to-byte seeking unreliable. Raydio therefore
normalizes every source file once with ffmpeg:

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

Raydio stores schedule slots in SQLite. A slot is either a track or silence:

```text
track A -> silence gap -> track B -> silence gap -> ...
```

The scheduler fills future slots ahead of time. A background ticker keeps this
schedule extended while the process is running, even if there are no active
listeners. Track order uses a shuffle bag. When more than one track exists,
Raydio avoids choosing the same track for adjacent track slots.

Request handlers normally read the current slot from SQLite. If the current slot
is unexpectedly missing, the handler performs one fallback refill and retries.
This keeps `/radio`, `/api/now`, and `/api/events` robust without reverting to a
lazy-only scheduling model.

When a source file is removed:

- The track is marked missing.
- It is not scheduled again.
- Future schedule slots are refreshed.
- Already-playing cached audio can finish.

When the catalog is empty, the scheduler emits silence slots and `/radio`
continues streaming valid MP3 audio.

When all listeners disconnect, no audio is played in the background. Raydio only
continues maintaining schedule rows. The next listener joins the current
wall-clock slot and frame.

### Streaming

The stream loop sends small windows of MP3 frames. Each tick is aligned to
server time, so a slow client does not drag the radio timeline backward. If a
write stalls, the next successful tick resumes from the current global frame.

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

- `tracks`
- `track_assets`
- `schedule_slots`
- `settings`
- `schema_migrations`

## Playback From Terminal

```bash
curl -sN http://localhost:8080/radio | ffplay -hide_banner -nodisp -f mp3 -
```

## Reverse Proxy Notes

If `/radio` is served behind a reverse proxy, response buffering must be
disabled for that route.

Nginx example:

```nginx
location /radio {
    proxy_pass http://127.0.0.1:8080/radio;
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
mkdir -p /tmp/raydio-demo/data/inbox
cp tmp/origin/*.mp3 /tmp/raydio-demo/data/inbox/
CGO_ENABLED=1 go run ./cmd/raydio -addr 127.0.0.1:18080 -data /tmp/raydio-demo/data
```

Inspect catalog:

```bash
curl -s http://127.0.0.1:18080/api/catalog
```

Capture a short stream sample:

```bash
curl --max-time 3 -sN http://127.0.0.1:18080/radio -o /tmp/raydio-sample.mp3
ffprobe -v error -show_format -show_streams /tmp/raydio-sample.mp3
```

## Limitations

- Single-instance only.
- No admin UI.
- No write API for metadata.
- No HLS.
- No ICY metadata blocks in the MP3 stream.
- No seeking, previous-track, or next-track controls.
- No Vue, Vite, or Node-based frontend build chain.
