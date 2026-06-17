package settings

import (
	"log/slog"
	"os"
	"path/filepath"
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
  schedule_interval: 250ms
  stream_chunk_window: 240ms
  stream_buffer_window: 2s
  stream_write_timeout: 5s

worker:
  inbox_dir: '/music inbox'
  rescan_interval: 2s
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
}

func TestLoadKeepsDefaultsForOmittedValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
data_dir: /srv/raydio
server:
  addr: ":18080"
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
}

func TestLoadResolvesRelativePathsFromConfigDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(`
data_dir: data
worker:
  inbox_dir: inbox
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

func TestLoadRejectsInvalidLogLevel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("log_level: trace\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected invalid log level to fail")
	}
}
