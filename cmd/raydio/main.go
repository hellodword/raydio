package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"raydio/internal/paths"
	"raydio/internal/radio"
	"raydio/internal/settings"
	"raydio/internal/store"
	"raydio/web"
)

type config struct {
	ConfigPath         string
	Addr               string
	DataDir            string
	CacheDir           string
	DBPath             string
	ScheduleInterval   time.Duration
	StreamChunkWindow  time.Duration
	StreamBufferWindow time.Duration
	StreamWriteTimeout time.Duration
	GapFrames          int64
	LogLevel           slog.Level
}

type app struct {
	cfg      config
	store    *store.Store
	engine   *radio.Engine
	webFiles map[string]webFile
}

type webFile struct {
	Data        []byte
	ContentType string
	ETag        string
	ModTime     time.Time
}

const (
	defaultCatalogLimit   = 100
	maxCatalogLimit       = 500
	minStreamChunkWindow  = 120 * time.Millisecond
	maxStreamChunkWindow  = 2 * time.Second
	maxStreamBufferWindow = 30 * time.Second
)

func main() {
	cfg, err := readConfig(os.Args[1:])
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		slog.Error("load config failed", "error", err)
		os.Exit(1)
	}
	configureLogging(cfg.LogLevel)
	slog.Debug("config loaded", "path", cfg.ConfigPath, "log_level", cfg.LogLevel.String())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg); err != nil {
		slog.Error("raydio stopped", "error", err)
		os.Exit(1)
	}
}

func configureLogging(level slog.Level) {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: level,
	})))
}

func readConfig(args []string) (config, error) {
	var configPath string
	fs := flag.NewFlagSet("raydio", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&configPath, "config", "config.yaml", "configuration file path")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: %s [-config path]\n", fs.Name())
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if fs.NArg() != 0 {
		return config{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	fileCfg, err := settings.Load(configPath)
	if err != nil {
		return config{}, err
	}
	layout := paths.New(fileCfg.DataDir, "")
	return config{
		ConfigPath:         configPath,
		Addr:               fileCfg.Server.Addr,
		DataDir:            layout.DataDir,
		CacheDir:           layout.CacheDir,
		DBPath:             layout.DBPath,
		ScheduleInterval:   fileCfg.Server.ScheduleInterval,
		StreamChunkWindow:  fileCfg.Server.StreamChunkWindow,
		StreamBufferWindow: fileCfg.Server.StreamBufferWindow,
		StreamWriteTimeout: fileCfg.Server.StreamWriteTimeout,
		GapFrames:          fileCfg.GapFrames,
		LogLevel:           fileCfg.LogLevel,
	}, nil
}

func run(ctx context.Context, cfg config) error {
	if err := validateConfig(cfg); err != nil {
		return err
	}
	if err := paths.RequireServerCache(cfg.CacheDir, cfg.GapFrames); err != nil {
		return err
	}
	slog.Debug("worker-prepared media cache found", "cache_dir", cfg.CacheDir, "gap_frames", cfg.GapFrames)
	st, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	scheduler := radio.NewScheduler(st, paths.SilencePath(cfg.CacheDir, cfg.GapFrames), cfg.GapFrames)
	engine, err := radio.NewEngine(radio.EngineConfig{
		Scheduler:          scheduler,
		Store:              st,
		SilencePath:        paths.SilencePath(cfg.CacheDir, cfg.GapFrames),
		RefreshInterval:    cfg.ScheduleInterval,
		StreamChunkWindow:  cfg.StreamChunkWindow,
		StreamBufferWindow: cfg.StreamBufferWindow,
	})
	if err != nil {
		return err
	}
	if err := engine.Start(ctx); err != nil {
		return err
	}
	slog.Info("starting raydio", "addr", cfg.Addr, "data_dir", cfg.DataDir, "cache_dir", cfg.CacheDir, "db_path", cfg.DBPath, "schedule_interval", cfg.ScheduleInterval, "stream_chunk_window", cfg.StreamChunkWindow, "stream_buffer_window", cfg.StreamBufferWindow, "stream_write_timeout", cfg.StreamWriteTimeout, "gap_frames", cfg.GapFrames)

	webFiles, err := loadWebFiles()
	if err != nil {
		return err
	}
	a := &app{cfg: cfg, store: st, engine: engine, webFiles: webFiles}

	mux := http.NewServeMux()
	a.routes(mux)
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("raydio listening", "addr", cfg.Addr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func validateConfig(cfg config) error {
	if cfg.ScheduleInterval <= 0 {
		return fmt.Errorf("schedule interval must be positive")
	}
	if cfg.StreamChunkWindow <= 0 {
		return fmt.Errorf("stream chunk window must be positive")
	}
	if cfg.StreamChunkWindow < minStreamChunkWindow {
		return fmt.Errorf("stream chunk window must be at least %s", minStreamChunkWindow)
	}
	if cfg.StreamChunkWindow > maxStreamChunkWindow {
		return fmt.Errorf("stream chunk window must be at most %s", maxStreamChunkWindow)
	}
	if cfg.StreamBufferWindow <= 0 {
		return fmt.Errorf("stream buffer window must be positive")
	}
	if cfg.StreamBufferWindow < cfg.StreamChunkWindow {
		return fmt.Errorf("stream buffer window must be greater than or equal to stream chunk window")
	}
	if cfg.StreamBufferWindow > maxStreamBufferWindow {
		return fmt.Errorf("stream buffer window must be at most %s", maxStreamBufferWindow)
	}
	if cfg.StreamWriteTimeout <= 0 {
		return fmt.Errorf("stream write timeout must be positive")
	}
	if cfg.GapFrames <= 0 {
		return fmt.Errorf("gap frame count must be positive")
	}
	return nil
}

func (a *app) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /", a.handleIndex)
	mux.HandleFunc("GET /app.js", a.static("app.js", "application/javascript; charset=utf-8"))
	mux.HandleFunc("GET /styles.css", a.static("styles.css", "text/css; charset=utf-8"))
	mux.HandleFunc("GET /radio", a.handleRadio)
	mux.HandleFunc("GET /api/now", a.handleNow)
	mux.HandleFunc("GET /api/events", a.handleEvents)
	mux.HandleFunc("GET /api/catalog", a.handleCatalog)
	mux.HandleFunc("GET /covers/{id}", a.handleAsset("cover"))
	mux.HandleFunc("GET /lyrics/{id}", a.handleAsset("lyrics"))
	mux.HandleFunc("GET /healthz", a.handleHealthz)
}

func (a *app) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	a.serveWebFile(w, r, "index.html", "text/html; charset=utf-8")
}

func (a *app) static(name, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a.serveWebFile(w, r, name, contentType)
	}
}

func (a *app) serveWebFile(w http.ResponseWriter, r *http.Request, name, contentType string) {
	f, ok := a.webFiles[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if contentType != "" {
		f.ContentType = contentType
	}
	w.Header().Set("Content-Type", f.ContentType)
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Header().Set("ETag", f.ETag)
	http.ServeContent(w, r, name, f.ModTime, bytes.NewReader(f.Data))
}

func (a *app) handleRadio(w http.ResponseWriter, r *http.Request) {
	slog.Debug("radio stream connected", "remote", r.RemoteAddr)
	defer slog.Debug("radio stream disconnected", "remote", r.RemoteAddr)

	h := w.Header()
	h.Set("Content-Type", "audio/mpeg")
	h.Set("Cache-Control", "no-store, no-transform")
	h.Set("X-Accel-Buffering", "no")
	h.Set("Accept-Ranges", "none")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	ctx := r.Context()
	seq := a.engine.LiveSeq()
	deadlineSupported := true
	for {
		p, next, err := a.engine.WaitPacket(ctx, seq)
		if err != nil {
			return
		}
		seq = next
		if deadlineSupported {
			err := setWriteDeadline(w, a.cfg.StreamWriteTimeout)
			deadlineSupported = err == nil
			if err != nil {
				slog.Debug("radio write deadline unsupported", "remote", r.RemoteAddr, "error", err)
			}
		}
		if _, err := w.Write(p.Data); err != nil {
			if ctx.Err() == nil {
				slog.Debug("radio stream write stopped", "remote", r.RemoteAddr, "error", err)
			}
			return
		}
		flusher.Flush()
	}
}

func setWriteDeadline(w http.ResponseWriter, timeout time.Duration) error {
	return http.NewResponseController(w).SetWriteDeadline(time.Now().Add(timeout))
}

func (a *app) handleNow(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, a.engine.Now())
}

func (a *app) handleEvents(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store, no-transform")
	h.Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	ctx := r.Context()
	send := func(event string, payload any) bool {
		b, err := json.Marshal(payload)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	events, unsubscribe := a.engine.SubscribeEvents()
	defer unsubscribe()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	if !send("now", a.engine.Now()) {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case now, ok := <-events:
			if !ok {
				return
			}
			if !send("now", now) {
				return
			}
		case <-heartbeat.C:
			if _, err := io.WriteString(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (a *app) handleCatalog(w http.ResponseWriter, r *http.Request) {
	limit, err := parseCatalogLimit(r.URL.Query().Get("limit"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	afterTitle, afterID, err := decodeCatalogCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rev, err := a.store.CatalogRevision(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	etag := catalogETag(rev, limit, r.URL.Query().Get("cursor"))
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, no-cache")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	tracks, err := a.store.ListCatalogPage(r.Context(), afterTitle, afterID, limit+1)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	hasMore := len(tracks) > limit
	if hasMore {
		tracks = tracks[:limit]
	}
	nextCursor := ""
	if hasMore && len(tracks) > 0 {
		last := tracks[len(tracks)-1]
		nextCursor = encodeCatalogCursor(last.Title, last.ID)
	}
	writeJSON(w, map[string]any{
		"revision":   rev.Revision,
		"tracks":     tracks,
		"nextCursor": nextCursor,
		"hasMore":    hasMore,
	})
}

func (a *app) handleAsset(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if asset, ok := a.engine.Asset(id, kind); ok {
			serveAsset(w, r, asset)
			return
		}
		asset, err := a.store.Asset(r.Context(), id, kind)
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		a.engine.RequestRefresh()
		serveAsset(w, r, asset)
	}
}

func (a *app) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "ok\n")
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if w.Header().Get("Cache-Control") == "" {
		w.Header().Set("Cache-Control", "no-store")
	}
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}

func loadWebFiles() (map[string]webFile, error) {
	files := map[string]string{
		"index.html": "text/html; charset=utf-8",
		"app.js":     "application/javascript; charset=utf-8",
		"styles.css": "text/css; charset=utf-8",
	}
	out := make(map[string]webFile, len(files))
	for name, contentType := range files {
		data, err := web.FS.ReadFile(name)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		out[name] = webFile{
			Data:        data,
			ContentType: contentType,
			ETag:        `"` + base64.RawURLEncoding.EncodeToString(sum[:]) + `"`,
			ModTime:     time.Unix(0, 0).UTC(),
		}
	}
	return out, nil
}

func serveAsset(w http.ResponseWriter, r *http.Request, a store.Asset) {
	w.Header().Set("Content-Type", a.MIME)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeFile(w, r, a.Path)
}

func parseCatalogLimit(raw string) (int, error) {
	if raw == "" {
		return defaultCatalogLimit, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("limit must be a positive integer")
	}
	if n > maxCatalogLimit {
		return maxCatalogLimit, nil
	}
	return n, nil
}

func encodeCatalogCursor(title, id string) string {
	b, _ := json.Marshal(struct {
		Title string `json:"title"`
		ID    string `json:"id"`
	}{Title: title, ID: id})
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCatalogCursor(raw string) (string, string, error) {
	if raw == "" {
		return "", "", nil
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return "", "", fmt.Errorf("invalid cursor")
	}
	var cursor struct {
		Title string `json:"title"`
		ID    string `json:"id"`
	}
	if err := json.Unmarshal(b, &cursor); err != nil || cursor.ID == "" {
		return "", "", fmt.Errorf("invalid cursor")
	}
	return cursor.Title, cursor.ID, nil
}

func catalogETag(rev store.CatalogRevision, limit int, cursor string) string {
	sum := sha256.Sum256([]byte(cursor))
	return fmt.Sprintf(`"catalog-%d-%d-%x"`, rev.Revision, limit, sum[:6])
}
