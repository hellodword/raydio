package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const testStationUUID = "00000000-0000-0000-0000-000000000001"

func TestValidateConfigRejectsInvalidValues(t *testing.T) {
	if err := validateConfig(config{SyncInterval: 0, HTTPTimeout: time.Second, MaxAudioBytes: 1, MaxCoverBytes: 1, Radios: []radioConfig{{UUID: testStationUUID}}}); err == nil {
		t.Fatal("expected non-positive sync interval to fail validation")
	}
	if err := validateConfig(config{SyncInterval: time.Second, HTTPTimeout: 0, MaxAudioBytes: 1, MaxCoverBytes: 1, Radios: []radioConfig{{UUID: testStationUUID}}}); err == nil {
		t.Fatal("expected non-positive http timeout to fail validation")
	}
	if err := validateConfig(config{SyncInterval: time.Second, HTTPTimeout: time.Second, MaxAudioBytes: 0, MaxCoverBytes: 1, Radios: []radioConfig{{UUID: testStationUUID}}}); err == nil {
		t.Fatal("expected non-positive max audio bytes to fail validation")
	}
	if err := validateConfig(config{SyncInterval: time.Second, HTTPTimeout: time.Second, MaxAudioBytes: 1, MaxCoverBytes: 0, Radios: []radioConfig{{UUID: testStationUUID}}}); err == nil {
		t.Fatal("expected non-positive max cover bytes to fail validation")
	}
	if err := validateConfig(config{SyncInterval: time.Second, HTTPTimeout: time.Second, MaxAudioBytes: 1, MaxCoverBytes: 1}); err == nil {
		t.Fatal("expected empty radios to fail validation")
	}
}

func TestReadConfigLoadsSunoWorkerSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
data_dir: /srv/raydio
log_level: INFO
worker:
  inbox_dir: /srv/inbox
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

	cfg, err := readConfig([]string{"-config", path})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir != "/srv/raydio" {
		t.Fatalf("DataDir = %q", cfg.DataDir)
	}
	if cfg.InboxDir != "/srv/inbox" {
		t.Fatalf("InboxDir = %q", cfg.InboxDir)
	}
	if cfg.SyncInterval != 45*time.Minute {
		t.Fatalf("SyncInterval = %s", cfg.SyncInterval)
	}
	if cfg.HTTPTimeout != 12*time.Second {
		t.Fatalf("HTTPTimeout = %s", cfg.HTTPTimeout)
	}
	if cfg.MaxAudioBytes != 12345 {
		t.Fatalf("MaxAudioBytes = %d", cfg.MaxAudioBytes)
	}
	if cfg.MaxCoverBytes != 2345 {
		t.Fatalf("MaxCoverBytes = %d", cfg.MaxCoverBytes)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Fatalf("LogLevel = %s", cfg.LogLevel)
	}
	if len(cfg.Radios) != 1 || cfg.Radios[0].Alias != "monthly" || cfg.Radios[0].UUID != testStationUUID {
		t.Fatalf("Radios = %+v", cfg.Radios)
	}
	if cfg.Radios[0].InboxDir != filepath.Join("/srv/inbox", testStationUUID) {
		t.Fatalf("Radio inbox = %q", cfg.Radios[0].InboxDir)
	}
}
