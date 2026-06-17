package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"raydio/internal/audio"
	"raydio/internal/paths"
	"raydio/internal/radio"
	"raydio/internal/store"
)

func TestValidateConfigRejectsNonPositiveScheduleInterval(t *testing.T) {
	cfg := config{
		ScheduleInterval: 0,
		GapFrames:        1,
	}
	if err := validateConfig(cfg); err == nil {
		t.Fatal("expected non-positive schedule interval to fail validation")
	}
}

func TestReadConfigLoadsServerSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
data_dir: /srv/raydio
gap_frames: 7
server:
  addr: ":18080"
  schedule_interval: 250ms
worker:
  rescan_interval: 2s
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := readConfig([]string{"-config", path})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != ":18080" {
		t.Fatalf("Addr = %q", cfg.Addr)
	}
	if cfg.DataDir != "/srv/raydio" {
		t.Fatalf("DataDir = %q", cfg.DataDir)
	}
	if cfg.CacheDir != "/srv/raydio/cache" {
		t.Fatalf("CacheDir = %q", cfg.CacheDir)
	}
	if cfg.DBPath != "/srv/raydio/raydio.sqlite" {
		t.Fatalf("DBPath = %q", cfg.DBPath)
	}
	if cfg.ScheduleInterval != 250*time.Millisecond {
		t.Fatalf("ScheduleInterval = %s", cfg.ScheduleInterval)
	}
	if cfg.GapFrames != 7 {
		t.Fatalf("GapFrames = %d", cfg.GapFrames)
	}
}

func TestRunRejectsMissingWorkerPreparedCache(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	layout := paths.New(dir, "")
	err := run(ctx, config{
		Addr:             "127.0.0.1:0",
		DataDir:          dir,
		CacheDir:         layout.CacheDir,
		DBPath:           layout.DBPath,
		ScheduleInterval: time.Second,
		GapFrames:        5,
	})
	if err == nil {
		t.Fatal("expected missing worker-prepared cache to fail startup")
	}
	if !strings.Contains(err.Error(), "run raydio-worker") {
		t.Fatalf("error = %q, want raydio-worker guidance", err)
	}
}

func TestRunStartsWithWorkerPreparedSilence(t *testing.T) {
	requireFFmpeg(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dir := t.TempDir()
	layout := paths.New(dir, "")
	for _, dir := range append([]string{layout.CacheDir}, paths.CacheDirs(layout.CacheDir)...) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := audio.EnsureSilence(ctx, paths.SilencePath(layout.CacheDir, 5), 5); err != nil {
		t.Fatal(err)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- run(ctx, config{
			Addr:             "127.0.0.1:0",
			DataDir:          dir,
			CacheDir:         layout.CacheDir,
			DBPath:           layout.DBPath,
			ScheduleInterval: 5 * time.Millisecond,
			GapFrames:        5,
		})
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not stop after context cancellation")
	}
}

func TestScheduleLoopMaintainsFutureSlots(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "raydio.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	a := &app{
		cfg:       config{ScheduleInterval: 5 * time.Millisecond},
		scheduler: radio.NewScheduler(st, "/cache/silence.mp3", 5),
	}
	go a.scheduleLoop(ctx)

	waitFor(t, 500*time.Millisecond, func() bool {
		slots, err := st.SlotsEndingAfter(ctx, time.Now().UnixMilli())
		return err == nil && len(slots) > 0
	})
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
