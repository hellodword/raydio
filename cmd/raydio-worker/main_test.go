package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"raydio/internal/paths"
)

func TestValidateConfigRejectsInvalidValues(t *testing.T) {
	if err := validateConfig(config{RescanInterval: 0, GapFrames: 1}); err == nil {
		t.Fatal("expected non-positive rescan interval to fail validation")
	}
	if err := validateConfig(config{RescanInterval: time.Second, GapFrames: 0}); err == nil {
		t.Fatal("expected non-positive gap frames to fail validation")
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
			RescanInterval: 50 * time.Millisecond,
			GapFrames:      5,
		})
	}()

	waitFor(t, time.Second, func() bool {
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
	if err := os.MkdirAll(layout.InboxDir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(layout.InboxDir, "song.mp3")
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
			RescanInterval: 50 * time.Millisecond,
			GapFrames:      5,
		})
	}()

	waitFor(t, 2*time.Second, func() bool {
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
