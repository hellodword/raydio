package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
		ScheduleInterval:   0,
		StreamChunkWindow:  time.Millisecond,
		StreamBufferWindow: time.Second,
		StreamWriteTimeout: time.Second,
		GapFrames:          1,
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
log_level: ERROR
server:
  addr: ":18080"
  schedule_interval: 250ms
  stream_chunk_window: 240ms
  stream_buffer_window: 2s
  stream_write_timeout: 5s
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
	if cfg.StreamChunkWindow != 240*time.Millisecond {
		t.Fatalf("StreamChunkWindow = %s", cfg.StreamChunkWindow)
	}
	if cfg.StreamBufferWindow != 2*time.Second {
		t.Fatalf("StreamBufferWindow = %s", cfg.StreamBufferWindow)
	}
	if cfg.StreamWriteTimeout != 5*time.Second {
		t.Fatalf("StreamWriteTimeout = %s", cfg.StreamWriteTimeout)
	}
	if cfg.GapFrames != 7 {
		t.Fatalf("GapFrames = %d", cfg.GapFrames)
	}
	if cfg.LogLevel != slog.LevelError {
		t.Fatalf("LogLevel = %s", cfg.LogLevel)
	}
}

func TestRunRejectsMissingWorkerPreparedCache(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	layout := paths.New(dir, "")
	err := run(ctx, config{
		Addr:               "127.0.0.1:0",
		DataDir:            dir,
		CacheDir:           layout.CacheDir,
		DBPath:             layout.DBPath,
		ScheduleInterval:   time.Second,
		StreamChunkWindow:  minStreamChunkWindow,
		StreamBufferWindow: time.Second,
		StreamWriteTimeout: time.Second,
		GapFrames:          5,
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
			Addr:               "127.0.0.1:0",
			DataDir:            dir,
			CacheDir:           layout.CacheDir,
			DBPath:             layout.DBPath,
			ScheduleInterval:   5 * time.Millisecond,
			StreamChunkWindow:  minStreamChunkWindow,
			StreamBufferWindow: time.Second,
			StreamWriteTimeout: time.Second,
			GapFrames:          5,
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

func TestEngineStartMaintainsFutureSlots(t *testing.T) {
	requireFFmpeg(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "raydio.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	silencePath := filepath.Join(t.TempDir(), "silence.mp3")
	if err := audio.EnsureSilence(ctx, silencePath, 5); err != nil {
		t.Fatal(err)
	}
	scheduler := radio.NewScheduler(st, silencePath, 5)
	engine, err := radio.NewEngine(radio.EngineConfig{
		Scheduler:          scheduler,
		Store:              st,
		SilencePath:        silencePath,
		RefreshInterval:    5 * time.Millisecond,
		StreamChunkWindow:  24 * time.Millisecond,
		StreamBufferWindow: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Start(ctx); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 500*time.Millisecond, func() bool {
		slots, err := st.SlotsEndingAfter(ctx, time.Now().UnixMilli())
		return err == nil && len(slots) > 0
	})
}

func TestHandleCatalogPaginatesAndUsesETag(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "raydio.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	for _, track := range []store.Track{
		{ID: "aaa", SourcePath: "/music/a.mp3", CachePath: "/cache/a.mp3", Title: "Alpha", Artist: "Artist", DurationMs: 1000, FrameCount: 42, FrameSize: 576, Bitrate: 192000, SampleRate: 48000, Channels: 2, Status: store.TrackStatusActive, SourceModUnix: 1},
		{ID: "bbb", SourcePath: "/music/b.mp3", CachePath: "/cache/b.mp3", Title: "Bravo", Artist: "Artist", DurationMs: 2000, FrameCount: 84, FrameSize: 576, Bitrate: 192000, SampleRate: 48000, Channels: 2, Status: store.TrackStatusActive, SourceModUnix: 1},
		{ID: "ccc", SourcePath: "/music/c.mp3", CachePath: "/cache/c.mp3", Title: "Charlie", Artist: "Artist", DurationMs: 3000, FrameCount: 125, FrameSize: 576, Bitrate: 192000, SampleRate: 48000, Channels: 2, Status: store.TrackStatusActive, SourceModUnix: 1},
	} {
		if err := st.UpsertTrack(ctx, track); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.UpsertAsset(ctx, store.Asset{TrackID: "bbb", Kind: "cover", Path: "/cache/b.jpg", MIME: "image/jpeg"}); err != nil {
		t.Fatal(err)
	}

	a := &app{store: st}
	req := httptest.NewRequest(http.MethodGet, "/api/catalog?limit=2", nil)
	rr := httptest.NewRecorder()
	a.handleCatalog(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "cachePath") || strings.Contains(rr.Body.String(), "sourcePath") {
		t.Fatalf("catalog exposed internal paths: %s", rr.Body.String())
	}
	var page struct {
		Revision   int64                `json:"revision"`
		Tracks     []store.CatalogTrack `json:"tracks"`
		NextCursor string               `json:"nextCursor"`
		HasMore    bool                 `json:"hasMore"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Revision == 0 || len(page.Tracks) != 2 || !page.HasMore || page.NextCursor == "" {
		t.Fatalf("page = %+v", page)
	}
	if page.Tracks[1].CoverURL != "/covers/bbb" {
		t.Fatalf("cover URL = %q", page.Tracks[1].CoverURL)
	}

	etag := rr.Header().Get("ETag")
	req = httptest.NewRequest(http.MethodGet, "/api/catalog?limit=2", nil)
	req.Header.Set("If-None-Match", etag)
	rr = httptest.NewRecorder()
	a.handleCatalog(rr, req)
	if rr.Code != http.StatusNotModified {
		t.Fatalf("etag status = %d", rr.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/catalog?limit=2&cursor="+page.NextCursor, nil)
	rr = httptest.NewRecorder()
	a.handleCatalog(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("next status = %d body = %s", rr.Code, rr.Body.String())
	}
	var next struct {
		Tracks  []store.CatalogTrack `json:"tracks"`
		HasMore bool                 `json:"hasMore"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &next); err != nil {
		t.Fatal(err)
	}
	if len(next.Tracks) != 1 || next.Tracks[0].ID != "ccc" || next.HasMore {
		t.Fatalf("next page = %+v", next)
	}
}

func TestServeAssetUsesETag(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cover.jpg")
	if err := os.WriteFile(path, []byte("cover"), 0o644); err != nil {
		t.Fatal(err)
	}
	asset := store.Asset{TrackID: "aaa", Kind: "cover", Path: path, MIME: "image/jpeg"}

	req := httptest.NewRequest(http.MethodGet, "/covers/aaa", nil)
	rr := httptest.NewRecorder()
	serveAsset(rr, req, asset)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
	}
	etag := rr.Header().Get("ETag")
	if etag == "" {
		t.Fatal("expected ETag")
	}

	req = httptest.NewRequest(http.MethodGet, "/covers/aaa", nil)
	req.Header.Set("If-None-Match", etag)
	rr = httptest.NewRecorder()
	serveAsset(rr, req, asset)
	if rr.Code != http.StatusNotModified {
		t.Fatalf("etag status = %d body = %s", rr.Code, rr.Body.String())
	}
	if rr.Body.Len() != 0 {
		t.Fatalf("304 body = %q", rr.Body.String())
	}
}

func TestServeWebFileUsesPreloadedBytesAndETag(t *testing.T) {
	files, err := loadWebFiles()
	if err != nil {
		t.Fatal(err)
	}
	a := &app{webFiles: files}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	a.serveWebFile(rr, req, "index.html", "text/html; charset=utf-8")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if rr.Header().Get("ETag") == "" {
		t.Fatal("missing ETag")
	}
	if !strings.Contains(rr.Body.String(), "<!doctype html>") {
		t.Fatalf("unexpected body prefix: %.40q", rr.Body.String())
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
