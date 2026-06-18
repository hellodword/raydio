package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"raydio/internal/paths"
)

const testStationUUID = "00000000-0000-0000-0000-000000000001"

func TestValidateConfigRejectsInvalidValues(t *testing.T) {
	if err := validateConfig(config{RescanInterval: 0, GapFrames: 1}); err == nil {
		t.Fatal("expected non-positive rescan interval to fail validation")
	}
	if err := validateConfig(config{RescanInterval: time.Second, GapFrames: 0}); err == nil {
		t.Fatal("expected non-positive gap frames to fail validation")
	}
}

func TestReadConfigLoadsWorkerSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
data_dir: /srv/raydio
gap_frames: 7
log_level: INFO
server:
  addr: ":18080"
  schedule_interval: 250ms
worker:
  inbox_dir: /srv/inbox
  rescan_interval: 2s
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
	if cfg.CacheDir != "/srv/raydio/cache" {
		t.Fatalf("CacheDir = %q", cfg.CacheDir)
	}
	if cfg.DBPath != "/srv/raydio/raydio.sqlite" {
		t.Fatalf("DBPath = %q", cfg.DBPath)
	}
	if cfg.RescanInterval != 2*time.Second {
		t.Fatalf("RescanInterval = %s", cfg.RescanInterval)
	}
	if cfg.GapFrames != 7 {
		t.Fatalf("GapFrames = %d", cfg.GapFrames)
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

func TestRunCreatesSilenceAndDatabase(t *testing.T) {
	requireFFmpeg(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dir := t.TempDir()
	layout := paths.New(dir, "")
	errCh := make(chan error, 1)
	go func() {
		errCh <- run(ctx, config{
			DataDir:        dir,
			InboxDir:       layout.InboxDir,
			CacheDir:       layout.CacheDir,
			DBPath:         layout.DBPath,
			Radios:         []radioConfig{{Alias: "monthly", UUID: testStationUUID, InboxDir: filepath.Join(layout.InboxDir, testStationUUID)}},
			RescanInterval: 50 * time.Millisecond,
			GapFrames:      5,
		})
	}()

	waitFor(t, 5*time.Second, func() bool {
		if _, err := os.Stat(paths.SilencePath(layout.CacheDir, 5)); err != nil {
			return false
		}
		if _, err := os.Stat(layout.DBPath); err != nil {
			return false
		}
		return true
	})
	cancel()
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not stop after context cancellation")
	}
}

func TestRunCreatesSharedSilenceForMultipleRadios(t *testing.T) {
	requireFFmpeg(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dir := t.TempDir()
	layout := paths.New(dir, "")
	radios := []radioConfig{
		{Alias: "monthly", UUID: testStationUUID, InboxDir: filepath.Join(layout.InboxDir, testStationUUID)},
		{Alias: "daily", UUID: "00000000-0000-0000-0000-000000000002", InboxDir: filepath.Join(layout.InboxDir, "00000000-0000-0000-0000-000000000002")},
		{Alias: "weekly", UUID: "00000000-0000-0000-0000-000000000003", InboxDir: filepath.Join(layout.InboxDir, "00000000-0000-0000-0000-000000000003")},
		{Alias: "hourly", UUID: "00000000-0000-0000-0000-000000000004", InboxDir: filepath.Join(layout.InboxDir, "00000000-0000-0000-0000-000000000004")},
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- run(ctx, config{
			DataDir:        dir,
			InboxDir:       layout.InboxDir,
			CacheDir:       layout.CacheDir,
			DBPath:         layout.DBPath,
			Radios:         radios,
			RescanInterval: 50 * time.Millisecond,
			GapFrames:      5,
		})
	}()

	waitFor(t, 5*time.Second, func() bool {
		if _, err := os.Stat(paths.SilencePath(layout.CacheDir, 5)); err != nil {
			return false
		}
		if _, err := os.Stat(layout.DBPath); err != nil {
			return false
		}
		return true
	})
	cancel()
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not stop after context cancellation")
	}
}

func TestWorkerScansMP3IntoCache(t *testing.T) {
	requireFFmpeg(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dir := t.TempDir()
	layout := paths.New(dir, "")
	stationInbox := filepath.Join(layout.InboxDir, testStationUUID)
	if err := os.MkdirAll(stationInbox, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(stationInbox, "song.mp3")
	if err := exec.CommandContext(ctx, "ffmpeg",
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "sine=frequency=330:duration=1",
		"-c:a", "libmp3lame", "-q:a", "4",
		"-f", "mp3", src,
	).Run(); err != nil {
		t.Fatal(err)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- run(ctx, config{
			DataDir:        dir,
			InboxDir:       layout.InboxDir,
			CacheDir:       layout.CacheDir,
			DBPath:         layout.DBPath,
			Radios:         []radioConfig{{Alias: "monthly", UUID: testStationUUID, InboxDir: stationInbox}},
			RescanInterval: 50 * time.Millisecond,
			GapFrames:      5,
		})
	}()

	waitFor(t, 5*time.Second, func() bool {
		matches, err := filepath.Glob(filepath.Join(layout.TracksDir, "*.mp3"))
		return err == nil && len(matches) == 1
	})
	cancel()
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not stop after context cancellation")
	}
}

func TestRunContinuesAfterInitialCatalogScanFailure(t *testing.T) {
	oldScanAllFunc := scanAllFunc
	defer func() { scanAllFunc = oldScanAllFunc }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	layout := paths.New(dir, "")
	initialErr := errors.New("initial catalog scan failed")
	var calls atomic.Int64
	scanAllFunc = func(context.Context, []catalogRuntime) error {
		calls.Add(1)
		return initialErr
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- run(ctx, config{
			DataDir:        dir,
			InboxDir:       layout.InboxDir,
			CacheDir:       layout.CacheDir,
			DBPath:         layout.DBPath,
			Radios:         []radioConfig{{Alias: "monthly", UUID: testStationUUID, InboxDir: filepath.Join(layout.InboxDir, testStationUUID)}},
			RescanInterval: time.Hour,
			GapFrames:      5,
		})
	}()

	select {
	case err := <-errCh:
		t.Fatalf("worker exited after initial scan failure: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if calls.Load() != 1 {
		t.Fatalf("initial scan calls = %d, want 1", calls.Load())
	}
	cancel()
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not stop after context cancellation")
	}
}

func TestWatchAndScanDebouncesEventsAndWatchesNewDirectories(t *testing.T) {
	oldDebounce := watchDebounce
	watchDebounce = 25 * time.Millisecond
	defer func() { watchDebounce = oldDebounce }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	inbox := filepath.Join(dir, testStationUUID)
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatal(err)
	}
	var scans atomic.Int64
	errCh := make(chan error, 1)
	go func() {
		errCh <- watchAndScan(ctx, time.Hour, []catalogRuntime{{
			alias: "monthly",
			uuid:  testStationUUID,
			inbox: inbox,
		}}, func(context.Context, catalogRuntime) error {
			scans.Add(1)
			return nil
		})
	}()
	time.Sleep(50 * time.Millisecond)

	subdir := filepath.Join(inbox, "nested")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool {
		return scans.Load() >= 1
	})

	before := scans.Load()
	if err := os.WriteFile(filepath.Join(subdir, "song.mp3"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool {
		return scans.Load() > before
	})

	cancel()
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("watch loop did not stop")
	}
}

func TestWatchLoopReturnsErrorWhenEventsChannelCloses(t *testing.T) {
	events := make(chan fsnotify.Event)
	close(events)
	errs := make(chan error)
	err := watchLoop(context.Background(), time.Hour, nil, func(context.Context, catalogRuntime) error {
		return nil
	}, noopWatcher{}, events, errs, map[string]struct{}{})
	if err == nil || !strings.Contains(err.Error(), "events channel closed") {
		t.Fatalf("error = %v, want events channel closed", err)
	}
}

func TestWatchLoopReturnsErrorWhenErrorsChannelCloses(t *testing.T) {
	events := make(chan fsnotify.Event)
	errs := make(chan error)
	close(errs)
	err := watchLoop(context.Background(), time.Hour, nil, func(context.Context, catalogRuntime) error {
		return nil
	}, noopWatcher{}, events, errs, map[string]struct{}{})
	if err == nil || !strings.Contains(err.Error(), "errors channel closed") {
		t.Fatalf("error = %v, want errors channel closed", err)
	}
}

func TestIgnoredPathFiltersHiddenTempAndPartialFiles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "inbox")
	if ignoredPath(root, filepath.Join(root, "song.mp3")) {
		t.Fatal("regular mp3 should not be ignored")
	}
	for _, path := range []string{
		filepath.Join(root, ".hidden", "song.mp3"),
		filepath.Join(root, "song.tmp"),
		filepath.Join(root, "song.part"),
	} {
		if !ignoredPath(root, path) {
			t.Fatalf("%s should be ignored", path)
		}
	}
}

func TestCleanupStaleTempFiles(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	oldTmp := filepath.Join(root, "old.tmp")
	newTmp := filepath.Join(root, "new.tmp")
	regular := filepath.Join(root, "keep.mp3")
	for _, path := range []string{oldTmp, newTmp, regular} {
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	oldTime := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(oldTmp, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	if err := cleanupStaleTempFiles(ctx, root, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldTmp); !os.IsNotExist(err) {
		t.Fatalf("old tmp exists err=%v", err)
	}
	for _, path := range []string{newTmp, regular} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s missing: %v", path, err)
		}
	}
}

type noopWatcher struct{}

func (noopWatcher) Add(string) error {
	return nil
}

func waitFor(t *testing.T, timeout time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}

func requireFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg unavailable")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe unavailable")
	}
}
