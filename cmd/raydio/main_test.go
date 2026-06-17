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

const testStationUUID = "00000000-0000-0000-0000-000000000001"

func TestValidateConfigRejectsNonPositiveScheduleInterval(t *testing.T) {
	cfg := config{
		RateLimitRPS:       10,
		RateLimitBurst:     30,
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

func TestValidateConfigRejectsInvalidRateLimit(t *testing.T) {
	cfg := config{
		RateLimitRPS:       0,
		RateLimitBurst:     30,
		ScheduleInterval:   time.Second,
		StreamChunkWindow:  minStreamChunkWindow,
		StreamBufferWindow: time.Second,
		StreamWriteTimeout: time.Second,
		GapFrames:          1,
		Radios:             []radioConfig{{Alias: "monthly", UUID: testStationUUID}},
	}
	if err := validateConfig(cfg); err == nil {
		t.Fatal("expected non-positive rate limit rps to fail validation")
	}
	cfg.RateLimitRPS = 10
	cfg.RateLimitBurst = 0
	if err := validateConfig(cfg); err == nil {
		t.Fatal("expected non-positive rate limit burst to fail validation")
	}
	cfg.RateLimitBurst = 30
	cfg.TrustedProxyCIDRs = []string{"not-a-cidr"}
	if err := validateConfig(cfg); err == nil {
		t.Fatal("expected invalid trusted proxy cidr to fail validation")
	}
	cfg.TrustedProxyCIDRs = nil
	cfg.ClientIPHeaders = []string{"Forwarded"}
	if err := validateConfig(cfg); err == nil {
		t.Fatal("expected unsupported client ip header to fail validation")
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
  rate_limit_rps: 5.5
  rate_limit_burst: 12
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
	if cfg.RateLimitRPS != 5.5 {
		t.Fatalf("RateLimitRPS = %v", cfg.RateLimitRPS)
	}
	if cfg.RateLimitBurst != 12 {
		t.Fatalf("RateLimitBurst = %d", cfg.RateLimitBurst)
	}
	if strings.Join(cfg.TrustedProxyCIDRs, ",") != "127.0.0.1/32,10.0.0.0/8" {
		t.Fatalf("TrustedProxyCIDRs = %+v", cfg.TrustedProxyCIDRs)
	}
	if strings.Join(cfg.ClientIPHeaders, ",") != "X-Forwarded-For,CF-Connecting-IP" {
		t.Fatalf("ClientIPHeaders = %+v", cfg.ClientIPHeaders)
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
	if len(cfg.Radios) != 1 || cfg.Radios[0].Alias != "monthly" || cfg.Radios[0].UUID != testStationUUID {
		t.Fatalf("Radios = %+v", cfg.Radios)
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
		RateLimitRPS:       10,
		RateLimitBurst:     30,
		ScheduleInterval:   time.Second,
		StreamChunkWindow:  minStreamChunkWindow,
		StreamBufferWindow: time.Second,
		StreamWriteTimeout: time.Second,
		GapFrames:          5,
		Radios:             []radioConfig{{Alias: "monthly", UUID: testStationUUID}},
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
			RateLimitRPS:       10,
			RateLimitBurst:     30,
			ScheduleInterval:   5 * time.Millisecond,
			StreamChunkWindow:  minStreamChunkWindow,
			StreamBufferWindow: time.Second,
			StreamWriteTimeout: time.Second,
			GapFrames:          5,
			Radios:             []radioConfig{{Alias: "monthly", UUID: testStationUUID}},
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
	mustUpsertStation(t, ctx, st)

	silencePath := filepath.Join(t.TempDir(), "silence.mp3")
	if err := audio.EnsureSilence(ctx, silencePath, 5); err != nil {
		t.Fatal(err)
	}
	scheduler := radio.NewScheduler(st, testStationUUID, silencePath, 5)
	engine, err := radio.NewEngine(radio.EngineConfig{
		StationUUID:        testStationUUID,
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
		slots, err := st.SlotsEndingAfter(ctx, testStationUUID, time.Now().UnixMilli())
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
	mustUpsertStation(t, ctx, st)

	for _, track := range []store.Track{
		{ID: "aaa", StationUUID: testStationUUID, SourcePath: "/music/a.mp3", CachePath: "/cache/a.mp3", Title: "Alpha", Artist: "Artist", DurationMs: 1000, FrameCount: 42, FrameSize: 576, Bitrate: 192000, SampleRate: 48000, Channels: 2, Status: store.TrackStatusActive, SourceModUnix: 1},
		{ID: "bbb", StationUUID: testStationUUID, SourcePath: "/music/b.mp3", CachePath: "/cache/b.mp3", Title: "Bravo", Artist: "Artist", DurationMs: 2000, FrameCount: 84, FrameSize: 576, Bitrate: 192000, SampleRate: 48000, Channels: 2, Status: store.TrackStatusActive, SourceModUnix: 1},
		{ID: "ccc", StationUUID: testStationUUID, SourcePath: "/music/c.mp3", CachePath: "/cache/c.mp3", Title: "Charlie", Artist: "Artist", DurationMs: 3000, FrameCount: 125, FrameSize: 576, Bitrate: 192000, SampleRate: 48000, Channels: 2, Status: store.TrackStatusActive, SourceModUnix: 1},
	} {
		if err := st.UpsertTrack(ctx, track); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.UpsertAsset(ctx, store.Asset{TrackID: "bbb", Kind: "cover", Path: "/cache/b.jpg", MIME: "image/jpeg"}); err != nil {
		t.Fatal(err)
	}

	a := testApp(st)
	req := httptest.NewRequest(http.MethodGet, "/radio/monthly/api/catalog?limit=2", nil)
	req.SetPathValue("station", "monthly")
	rr := httptest.NewRecorder()
	a.handleCatalog(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "cachePath") || strings.Contains(rr.Body.String(), "sourcePath") {
		t.Fatalf("catalog exposed internal paths: %s", rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "lyrics") {
		t.Fatalf("catalog exposed lyrics field: %s", rr.Body.String())
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
	if page.Tracks[1].CoverURL != "/radio/"+testStationUUID+"/covers/bbb" {
		t.Fatalf("cover URL = %q", page.Tracks[1].CoverURL)
	}

	etag := rr.Header().Get("ETag")
	req = httptest.NewRequest(http.MethodGet, "/radio/monthly/api/catalog?limit=2", nil)
	req.SetPathValue("station", "monthly")
	req.Header.Set("If-None-Match", etag)
	rr = httptest.NewRecorder()
	a.handleCatalog(rr, req)
	if rr.Code != http.StatusNotModified {
		t.Fatalf("etag status = %d", rr.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/radio/monthly/api/catalog?limit=2&cursor="+page.NextCursor, nil)
	req.SetPathValue("station", "monthly")
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

func TestRoutesRejectLegacyRadioAndAPIPaths(t *testing.T) {
	a := testApp(nil)
	mux := http.NewServeMux()
	a.routes(mux)
	for _, path := range []string{"/radio", "/api/now", "/api/events", "/api/catalog", "/radio/monthly/lyrics/aaa"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404", path, rr.Code)
		}
	}
}

func TestRateLimitMiddlewareLimitsAPIAndExemptsHealthz(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/stations", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	handler := newRateLimitMiddleware(1, 1, mustClientIPResolver(t, nil, nil), mux)

	req := httptest.NewRequest(http.MethodGet, "/api/stations", nil)
	req.RemoteAddr = "192.0.2.10:1234"
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("first status = %d", rr.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/stations", nil)
	req.RemoteAddr = "192.0.2.10:1234"
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d", rr.Code)
	}

	for i := 0; i < 3; i++ {
		req = httptest.NewRequest(http.MethodGet, "/healthz", nil)
		req.RemoteAddr = "192.0.2.10:1234"
		rr = httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("healthz status = %d", rr.Code)
		}
	}
}

func TestRateLimitMiddlewareSeparatesClientsBehindTrustedProxy(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/stations", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	handler := newRateLimitMiddleware(1, 1, mustClientIPResolver(t, []string{"10.0.0.0/8"}, nil), mux)

	req := httptest.NewRequest(http.MethodGet, "/api/stations", nil)
	req.RemoteAddr = "10.1.2.3:1234"
	req.Header.Set("CF-Connecting-IP", "198.51.100.10")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("first client status = %d", rr.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/stations", nil)
	req.RemoteAddr = "10.1.2.3:1234"
	req.Header.Set("CF-Connecting-IP", "198.51.100.11")
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("second client status = %d", rr.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/stations", nil)
	req.RemoteAddr = "10.1.2.3:1234"
	req.Header.Set("CF-Connecting-IP", "198.51.100.10")
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("first client repeat status = %d", rr.Code)
	}
}

func TestClientIPResolver(t *testing.T) {
	tests := []struct {
		name     string
		trusted  []string
		headers  []string
		remote   string
		reqHeads map[string]string
		want     string
	}{
		{
			name:   "direct remote ignores spoofed headers",
			remote: "192.0.2.10:1234",
			reqHeads: map[string]string{
				"CF-Connecting-IP": "198.51.100.10",
			},
			want: "192.0.2.10",
		},
		{
			name:    "trusted proxy uses cloudflare header",
			trusted: []string{"10.0.0.0/8"},
			remote:  "10.1.2.3:1234",
			reqHeads: map[string]string{
				"CF-Connecting-IP": "198.51.100.10",
			},
			want: "198.51.100.10",
		},
		{
			name:    "trusted proxy uses first valid forwarded for",
			trusted: []string{"10.0.0.0/8"},
			headers: []string{"X-Forwarded-For"},
			remote:  "10.1.2.3:1234",
			reqHeads: map[string]string{
				"X-Forwarded-For": "bad, 198.51.100.11, 198.51.100.12",
			},
			want: "198.51.100.11",
		},
		{
			name:    "invalid trusted headers fall back to peer",
			trusted: []string{"10.0.0.0/8"},
			remote:  "10.1.2.3:1234",
			reqHeads: map[string]string{
				"CF-Connecting-IP": "not-an-ip",
			},
			want: "10.1.2.3",
		},
		{
			name:    "trusted exact ipv6 proxy",
			trusted: []string{"::1"},
			remote:  "[::1]:1234",
			reqHeads: map[string]string{
				"X-Real-IP": "2001:db8::1",
			},
			want: "2001:db8::1",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resolver := mustClientIPResolver(t, tc.trusted, tc.headers)
			req := httptest.NewRequest(http.MethodGet, "/api/stations", nil)
			req.RemoteAddr = tc.remote
			for k, v := range tc.reqHeads {
				req.Header.Set(k, v)
			}
			if got := resolver.clientIP(req); got != tc.want {
				t.Fatalf("clientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHandleStationsReturnsConfiguredStations(t *testing.T) {
	a := testApp(nil)
	req := httptest.NewRequest(http.MethodGet, "/api/stations", nil)
	rr := httptest.NewRecorder()
	a.handleStations(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"alias":"monthly"`) || !strings.Contains(rr.Body.String(), testStationUUID) {
		t.Fatalf("stations body = %s", rr.Body.String())
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

func mustUpsertStation(t *testing.T, ctx context.Context, st *store.Store) {
	t.Helper()
	if err := st.UpsertStation(ctx, store.Station{UUID: testStationUUID, Alias: "monthly", Enabled: true}); err != nil {
		t.Fatal(err)
	}
}

func testApp(st *store.Store) *app {
	rt := &stationRuntime{station: radioConfig{Alias: "monthly", UUID: testStationUUID}}
	return &app{
		store:      st,
		stations:   map[string]*stationRuntime{"monthly": rt, testStationUUID: rt},
		stationIDs: []string{testStationUUID},
	}
}

func mustClientIPResolver(t *testing.T, trustedCIDRs, headers []string) clientIPResolver {
	t.Helper()
	resolver, err := newClientIPResolver(trustedCIDRs, headers)
	if err != nil {
		t.Fatal(err)
	}
	return resolver
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
