package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"raydio/internal/catalog"
	"raydio/internal/radio"
	"raydio/internal/store"
	"raydio/web"
)

type config struct {
	Addr           string
	DataDir        string
	InboxDir       string
	CacheDir       string
	DBPath         string
	RescanInterval time.Duration
	GapFrames      int64
}

type app struct {
	cfg       config
	store     *store.Store
	catalog   *catalog.Service
	scheduler *radio.Scheduler
}

func main() {
	cfg := readConfig()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg); err != nil {
		log.Fatal(err)
	}
}

func readConfig() config {
	var cfg config
	flag.StringVar(&cfg.Addr, "addr", env("RAYDIO_ADDR", ":8080"), "HTTP listen address")
	flag.StringVar(&cfg.DataDir, "data", env("RAYDIO_DATA", "./data"), "data directory")
	flag.StringVar(&cfg.InboxDir, "inbox", env("RAYDIO_INBOX", ""), "inbox directory")
	flag.DurationVar(&cfg.RescanInterval, "rescan", envDuration("RAYDIO_RESCAN", 30*time.Second), "rescan interval")
	flag.Int64Var(&cfg.GapFrames, "gap-frames", envInt64("RAYDIO_GAP_FRAMES", 209), "silence gap frame count")
	flag.Parse()
	if cfg.InboxDir == "" {
		cfg.InboxDir = filepath.Join(cfg.DataDir, "inbox")
	}
	cfg.CacheDir = filepath.Join(cfg.DataDir, "cache")
	cfg.DBPath = filepath.Join(cfg.DataDir, "raydio.sqlite")
	return cfg
}

func run(ctx context.Context, cfg config) error {
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return err
	}
	st, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	cat := catalog.New(catalog.Config{
		InboxDir:      cfg.InboxDir,
		CacheDir:      cfg.CacheDir,
		SilenceFrames: cfg.GapFrames,
	}, st)
	if _, err := cat.Scan(ctx); err != nil {
		return err
	}
	scheduler := radio.NewScheduler(st, cat.SilencePath(), cfg.GapFrames)
	if err := scheduler.Ensure(ctx, time.Now()); err != nil {
		return err
	}

	a := &app{cfg: cfg, store: st, catalog: cat, scheduler: scheduler}
	go a.scanLoop(ctx)

	mux := http.NewServeMux()
	a.routes(mux)
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("raydio listening on %s", cfg.Addr)
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

func (a *app) scanLoop(ctx context.Context) {
	ticker := time.NewTicker(a.cfg.RescanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if result, err := a.catalog.Scan(ctx); err != nil {
				log.Printf("scan failed: %v", err)
			} else if result.Changed || result.Errors > 0 {
				log.Printf("scan seen=%d processed=%d skipped=%d errors=%d changed=%t", result.Seen, result.Processed, result.Skipped, result.Errors, result.Changed)
			}
		}
	}
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
	b, err := web.FS.ReadFile(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "public, max-age=300")
	http.ServeContent(w, r, name, time.Now(), strings.NewReader(string(b)))
}

func (a *app) handleRadio(w http.ResponseWriter, r *http.Request) {
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
	ticker := time.NewTicker(240 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		chunks, err := a.scheduler.Chunks(ctx, time.Now(), 10)
		if err != nil {
			log.Printf("radio chunks: %v", err)
			return
		}
		for _, ch := range chunks {
			if err := writeChunk(ctx, w, ch); err != nil {
				return
			}
		}
		flusher.Flush()

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func writeChunk(ctx context.Context, w io.Writer, ch radio.Chunk) error {
	f, err := os.Open(ch.Path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Seek(ch.Offset, io.SeekStart); err != nil {
		return err
	}
	limited := io.LimitReader(f, ch.Length)
	buf := make([]byte, 32*1024)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		n, readErr := limited.Read(buf)
		if n > 0 {
			if _, err := w.Write(buf[:n]); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

func (a *app) handleNow(w http.ResponseWriter, r *http.Request) {
	now, err := a.scheduler.Now(r.Context(), time.Now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, now)
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
	var lastSlot string
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
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		now, err := a.scheduler.Now(ctx, time.Now())
		if err != nil {
			return
		}
		if now.SlotID != lastSlot {
			if !send("now", now) {
				return
			}
			lastSlot = now.SlotID
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-heartbeat.C:
			if _, err := io.WriteString(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (a *app) handleCatalog(w http.ResponseWriter, r *http.Request) {
	tracks, err := a.store.ListTracks(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"tracks": tracks})
}

func (a *app) handleAsset(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		a, err := a.store.Asset(r.Context(), id, kind)
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", a.MIME)
		w.Header().Set("Cache-Control", "public, max-age=86400")
		http.ServeFile(w, r, a.Path)
	}
}

func (a *app) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "ok\n")
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

func envInt64(key string, fallback int64) int64 {
	if v := os.Getenv(key); v != "" {
		var out int64
		if _, err := fmt.Sscan(v, &out); err == nil {
			return out
		}
	}
	return fallback
}
