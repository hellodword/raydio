package settings

import (
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestLoadParsesConfigWithComments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
# Shared settings.
data_dir: /srv/raydio # root data directory
gap_frames: 5
log_level: WARN

server:
  addr: ":18080"
  rate_limit_rps: 7.5
  rate_limit_burst: 11
  max_streams_per_ip: 3
  max_events_per_ip: 6
  trusted_proxy_cidrs:
    - 127.0.0.1
    - 10.0.0.0/8
  client_ip_headers:
    - x-forwarded-for
    - cf-connecting-ip
  schedule_interval: 250ms
  stream_chunk_window: 240ms
  stream_buffer_window: 2s
  stream_write_timeout: 5s

worker:
  inbox_dir: '/music inbox'
  rescan_interval: 2s

suno:
  sync_interval: 45m
  http_timeout: 12s
  max_audio_bytes: 12345
  max_cover_bytes: 2345

radios:
  - alias: monthly
    uuid: "00000000-0000-0000-0000-000000000001"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir != "/srv/raydio" {
		t.Fatalf("DataDir = %q", cfg.DataDir)
	}
	if cfg.GapFrames != 5 {
		t.Fatalf("GapFrames = %d", cfg.GapFrames)
	}
	if cfg.LogLevel != slog.LevelWarn {
		t.Fatalf("LogLevel = %s", cfg.LogLevel)
	}
	if cfg.Server.Addr != ":18080" {
		t.Fatalf("Server.Addr = %q", cfg.Server.Addr)
	}
	if cfg.Server.RateLimitRPS != 7.5 {
		t.Fatalf("Server.RateLimitRPS = %v", cfg.Server.RateLimitRPS)
	}
	if cfg.Server.RateLimitBurst != 11 {
		t.Fatalf("Server.RateLimitBurst = %d", cfg.Server.RateLimitBurst)
	}
	if cfg.Server.MaxStreamsPerIP != 3 {
		t.Fatalf("Server.MaxStreamsPerIP = %d", cfg.Server.MaxStreamsPerIP)
	}
	if cfg.Server.MaxEventsPerIP != 6 {
		t.Fatalf("Server.MaxEventsPerIP = %d", cfg.Server.MaxEventsPerIP)
	}
	if strings.Join(cfg.Server.TrustedProxyCIDRs, ",") != "127.0.0.1/32,10.0.0.0/8" {
		t.Fatalf("Server.TrustedProxyCIDRs = %+v", cfg.Server.TrustedProxyCIDRs)
	}
	if strings.Join(cfg.Server.ClientIPHeaders, ",") != "X-Forwarded-For,CF-Connecting-IP" {
		t.Fatalf("Server.ClientIPHeaders = %+v", cfg.Server.ClientIPHeaders)
	}
	if cfg.Server.ScheduleInterval != 250*time.Millisecond {
		t.Fatalf("Server.ScheduleInterval = %s", cfg.Server.ScheduleInterval)
	}
	if cfg.Server.StreamChunkWindow != 240*time.Millisecond {
		t.Fatalf("Server.StreamChunkWindow = %s", cfg.Server.StreamChunkWindow)
	}
	if cfg.Server.StreamBufferWindow != 2*time.Second {
		t.Fatalf("Server.StreamBufferWindow = %s", cfg.Server.StreamBufferWindow)
	}
	if cfg.Server.StreamWriteTimeout != 5*time.Second {
		t.Fatalf("Server.StreamWriteTimeout = %s", cfg.Server.StreamWriteTimeout)
	}
	if cfg.Worker.InboxDir != "/music inbox" {
		t.Fatalf("Worker.InboxDir = %q", cfg.Worker.InboxDir)
	}
	if cfg.Worker.RescanInterval != 2*time.Second {
		t.Fatalf("Worker.RescanInterval = %s", cfg.Worker.RescanInterval)
	}
	if cfg.Suno.SyncInterval != 45*time.Minute {
		t.Fatalf("Suno.SyncInterval = %s", cfg.Suno.SyncInterval)
	}
	if cfg.Suno.HTTPTimeout != 12*time.Second {
		t.Fatalf("Suno.HTTPTimeout = %s", cfg.Suno.HTTPTimeout)
	}
	if cfg.Suno.MaxAudioBytes != 12345 {
		t.Fatalf("Suno.MaxAudioBytes = %d", cfg.Suno.MaxAudioBytes)
	}
	if cfg.Suno.MaxCoverBytes != 2345 {
		t.Fatalf("Suno.MaxCoverBytes = %d", cfg.Suno.MaxCoverBytes)
	}
	if len(cfg.Radios) != 1 || cfg.Radios[0].Alias != "monthly" || cfg.Radios[0].UUID != "00000000-0000-0000-0000-000000000001" {
		t.Fatalf("Radios = %+v", cfg.Radios)
	}
}

func TestLoadKeepsDefaultsForOmittedValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
data_dir: /srv/raydio
server:
  addr: ":18080"
radios:
  - alias: monthly
    uuid: "00000000-0000-0000-0000-000000000001"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GapFrames != 209 {
		t.Fatalf("GapFrames = %d", cfg.GapFrames)
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Fatalf("LogLevel = %s", cfg.LogLevel)
	}
	if cfg.Server.ScheduleInterval != time.Minute {
		t.Fatalf("Server.ScheduleInterval = %s", cfg.Server.ScheduleInterval)
	}
	if cfg.Server.RateLimitRPS != 10 {
		t.Fatalf("Server.RateLimitRPS = %v", cfg.Server.RateLimitRPS)
	}
	if cfg.Server.RateLimitBurst != 30 {
		t.Fatalf("Server.RateLimitBurst = %d", cfg.Server.RateLimitBurst)
	}
	if cfg.Server.MaxStreamsPerIP != 4 {
		t.Fatalf("Server.MaxStreamsPerIP = %d", cfg.Server.MaxStreamsPerIP)
	}
	if cfg.Server.MaxEventsPerIP != 8 {
		t.Fatalf("Server.MaxEventsPerIP = %d", cfg.Server.MaxEventsPerIP)
	}
	if len(cfg.Server.TrustedProxyCIDRs) != 0 {
		t.Fatalf("Server.TrustedProxyCIDRs = %+v", cfg.Server.TrustedProxyCIDRs)
	}
	if strings.Join(cfg.Server.ClientIPHeaders, ",") != "CF-Connecting-IP,X-Forwarded-For,X-Real-IP" {
		t.Fatalf("Server.ClientIPHeaders = %+v", cfg.Server.ClientIPHeaders)
	}
	if cfg.Server.StreamChunkWindow != 480*time.Millisecond {
		t.Fatalf("Server.StreamChunkWindow = %s", cfg.Server.StreamChunkWindow)
	}
	if cfg.Server.StreamBufferWindow != 2*time.Second {
		t.Fatalf("Server.StreamBufferWindow = %s", cfg.Server.StreamBufferWindow)
	}
	if cfg.Server.StreamWriteTimeout != 5*time.Second {
		t.Fatalf("Server.StreamWriteTimeout = %s", cfg.Server.StreamWriteTimeout)
	}
	if cfg.Worker.RescanInterval != 30*time.Second {
		t.Fatalf("Worker.RescanInterval = %s", cfg.Worker.RescanInterval)
	}
	if cfg.Suno.SyncInterval != 30*time.Minute {
		t.Fatalf("Suno.SyncInterval = %s", cfg.Suno.SyncInterval)
	}
	if cfg.Suno.HTTPTimeout != 30*time.Second {
		t.Fatalf("Suno.HTTPTimeout = %s", cfg.Suno.HTTPTimeout)
	}
	if cfg.Suno.MaxAudioBytes != 128*1024*1024 {
		t.Fatalf("Suno.MaxAudioBytes = %d", cfg.Suno.MaxAudioBytes)
	}
	if cfg.Suno.MaxCoverBytes != 16*1024*1024 {
		t.Fatalf("Suno.MaxCoverBytes = %d", cfg.Suno.MaxCoverBytes)
	}
}

func TestLoadDockerConfig(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	path := filepath.Join(filepath.Dir(file), "..", "..", "config.docker.yaml")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir != "/srv/raydio/data" {
		t.Fatalf("DataDir = %q", cfg.DataDir)
	}
	if cfg.Server.Addr != ":8080" {
		t.Fatalf("Server.Addr = %q", cfg.Server.Addr)
	}
	if strings.Join(cfg.Server.TrustedProxyCIDRs, ",") != "172.16.0.0/12" {
		t.Fatalf("Server.TrustedProxyCIDRs = %+v", cfg.Server.TrustedProxyCIDRs)
	}
	if strings.Join(cfg.Server.ClientIPHeaders, ",") != "CF-Connecting-IP,X-Forwarded-For,X-Real-IP" {
		t.Fatalf("Server.ClientIPHeaders = %+v", cfg.Server.ClientIPHeaders)
	}
	if len(cfg.Radios) == 0 {
		t.Fatal("Radios is empty")
	}
}

func TestLoadResolvesRelativePathsFromConfigDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(`
data_dir: data
worker:
  inbox_dir: inbox
radios:
  - alias: monthly
    uuid: "00000000-0000-0000-0000-000000000001"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir != filepath.Join(dir, "data") {
		t.Fatalf("DataDir = %q", cfg.DataDir)
	}
	if cfg.Worker.InboxDir != filepath.Join(dir, "inbox") {
		t.Fatalf("Worker.InboxDir = %q", cfg.Worker.InboxDir)
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("unknown: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected unknown key to fail")
	}
}

func TestLoadRejectsUnknownNestedKeys(t *testing.T) {
	tests := []string{
		`server:
  typo: true
radios:
  - alias: monthly
    uuid: "00000000-0000-0000-0000-000000000001"
`,
		`radios:
  - alias: monthly
    uuid: "00000000-0000-0000-0000-000000000001"
    typo: true
`,
	}
	for _, body := range tests {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("expected unknown nested key to fail: %s", body)
		}
	}
}

func TestLoadRejectsDuplicateKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
data_dir: /srv/one
data_dir: /srv/two
radios:
  - alias: monthly
    uuid: "00000000-0000-0000-0000-000000000001"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected duplicate key to fail")
	}
}

func TestLoadRejectsInvalidLogLevel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("log_level: trace\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected invalid log level to fail")
	}
}

func TestLoadRejectsInvalidDuration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
server:
  schedule_interval: soon
radios:
  - alias: monthly
    uuid: "00000000-0000-0000-0000-000000000001"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected invalid duration to fail")
	}
	if !strings.Contains(err.Error(), "server.schedule_interval must be a Go duration") {
		t.Fatalf("error = %q", err)
	}
}

func TestLoadRejectsInvalidClientIPSettings(t *testing.T) {
	tests := []string{
		`server:
  trusted_proxy_cidrs: ["not-a-cidr"]
radios:
  - alias: monthly
    uuid: "00000000-0000-0000-0000-000000000001"
`,
		`server:
  client_ip_headers: ["Forwarded"]
radios:
  - alias: monthly
    uuid: "00000000-0000-0000-0000-000000000001"
`,
		`server:
  trusted_proxy_cidrs: [""]
radios:
  - alias: monthly
    uuid: "00000000-0000-0000-0000-000000000001"
`,
	}
	for _, body := range tests {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("expected invalid client IP settings to fail: %s", body)
		}
	}
}

func TestLoadParsesStandardYAMLFlowStyle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
data_dir: /srv/raydio
server: {addr: ":18080", schedule_interval: 250ms}
radios: [{alias: monthly, uuid: "00000000-0000-0000-0000-000000000001"}]
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Addr != ":18080" {
		t.Fatalf("Server.Addr = %q", cfg.Server.Addr)
	}
	if cfg.Server.ScheduleInterval != 250*time.Millisecond {
		t.Fatalf("Server.ScheduleInterval = %s", cfg.Server.ScheduleInterval)
	}
	if len(cfg.Radios) != 1 || cfg.Radios[0].Alias != "monthly" {
		t.Fatalf("Radios = %+v", cfg.Radios)
	}
}

func TestLoadRejectsInvalidRadios(t *testing.T) {
	tests := []string{
		`radios:
  - alias: Monthly
    uuid: "00000000-0000-0000-0000-000000000001"
`,
		`radios:
  - alias: monthly
    uuid: "not-a-uuid"
`,
		`radios:
  - alias: monthly
    uuid: "00000000-0000-0000-0000-000000000001"
  - alias: monthly
    uuid: "00000000-0000-0000-0000-000000000002"
`,
	}
	for _, body := range tests {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("expected invalid radios to fail: %s", body)
		}
	}
}
