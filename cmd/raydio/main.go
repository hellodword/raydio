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
	"strings"
	"syscall"
	"time"

	"raydio/internal/paths"
	"raydio/internal/radio"
	"raydio/internal/settings"
	"raydio/internal/store"
	"raydio/web"
)

type config struct {
	Addr             string
	DataDir          string
	CacheDir         string
	DBPath           string
	ScheduleInterval time.Duration
	GapFrames        int64
}

type app struct {
	cfg       config
	store     *store.Store
	scheduler *radio.Scheduler
}

func main() {
	cfg, err := readConfig(os.Args[1:])
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg); err != nil {
		log.Fatal(err)
	}
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
		Addr:             fileCfg.Server.Addr,
		DataDir:          layout.DataDir,
		CacheDir:         layout.CacheDir,
		DBPath:           layout.DBPath,
		ScheduleInterval: fileCfg.Server.ScheduleInterval,
		GapFrames:        fileCfg.GapFrames,
	}, nil
}

func run(ctx context.Context, cfg config) error {
	if err := validateConfig(cfg); err != nil {
		return err
	}
	if err := paths.RequireServerCache(cfg.CacheDir, cfg.GapFrames); err != nil {
		return err
	}
	st, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	scheduler := radio.NewScheduler(st, paths.SilencePath(cfg.CacheDir, cfg.GapFrames), cfg.GapFrames)
	if err := scheduler.Ensure(ctx, time.Now()); err != nil {
		return err
	}

	a := &app{cfg: cfg, store: st, scheduler: scheduler}
	go a.scheduleLoop(ctx)

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

func validateConfig(cfg config) error {
	if cfg.ScheduleInterval <= 0 {
		return fmt.Errorf("schedule interval must be positive")
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

func (a *app) scheduleLoop(ctx context.Context) {
	ticker := time.NewTicker(a.cfg.ScheduleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := a.scheduler.Ensure(ctx, time.Now()); err != nil {
				log.Printf("schedule failed: %v", err)
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
