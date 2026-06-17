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
	"hash/fnv"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"raydio/internal/paths"
	"raydio/internal/radio"
	"raydio/internal/settings"
	"raydio/internal/store"
	"raydio/web"

	"golang.org/x/time/rate"
)

type config struct {
	ConfigPath         string
	Addr               string
	DataDir            string
	CacheDir           string
	DBPath             string
	Radios             []radioConfig
	RateLimitRPS       float64
	RateLimitBurst     int
	MaxStreamsPerIP    int
	MaxEventsPerIP     int
	TrustedProxyCIDRs  []string
	ClientIPHeaders    []string
	ScheduleInterval   time.Duration
	StreamChunkWindow  time.Duration
	StreamBufferWindow time.Duration
	StreamWriteTimeout time.Duration
	GapFrames          int64
	LogLevel           slog.Level
}

type app struct {
	cfg        config
	store      *store.Store
	stations   map[string]*stationRuntime
	stationIDs []string
	webFiles   map[string]webFile
	streams    *ipConnectionLimiter
	events     *ipConnectionLimiter
}

type radioConfig struct {
	Alias string `json:"alias"`
	UUID  string `json:"uuid"`
}

type stationRuntime struct {
	station radioConfig
	engine  *radio.Engine
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
	radios := make([]radioConfig, 0, len(fileCfg.Radios))
	for _, r := range fileCfg.Radios {
		radios = append(radios, radioConfig{Alias: r.Alias, UUID: r.UUID})
	}
	return config{
		ConfigPath:         configPath,
		Addr:               fileCfg.Server.Addr,
		DataDir:            layout.DataDir,
		CacheDir:           layout.CacheDir,
		DBPath:             layout.DBPath,
		Radios:             radios,
		RateLimitRPS:       fileCfg.Server.RateLimitRPS,
		RateLimitBurst:     fileCfg.Server.RateLimitBurst,
		MaxStreamsPerIP:    fileCfg.Server.MaxStreamsPerIP,
		MaxEventsPerIP:     fileCfg.Server.MaxEventsPerIP,
		TrustedProxyCIDRs:  append([]string(nil), fileCfg.Server.TrustedProxyCIDRs...),
		ClientIPHeaders:    append([]string(nil), fileCfg.Server.ClientIPHeaders...),
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
	clientIPs, err := newClientIPResolver(cfg.TrustedProxyCIDRs, cfg.ClientIPHeaders)
	if err != nil {
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

	stations := map[string]*stationRuntime{}
	stationIDs := make([]string, 0, len(cfg.Radios))
	for _, r := range cfg.Radios {
		if err := st.UpsertStation(ctx, store.Station{UUID: r.UUID, Alias: r.Alias, Enabled: true}); err != nil {
			return err
		}
		scheduler := radio.NewScheduler(st, r.UUID, paths.SilencePath(cfg.CacheDir, cfg.GapFrames), cfg.GapFrames)
		engine, err := radio.NewEngine(radio.EngineConfig{
			StationUUID:        r.UUID,
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
		rt := &stationRuntime{station: r, engine: engine}
		stations[r.UUID] = rt
		stations[r.Alias] = rt
		stationIDs = append(stationIDs, r.UUID)
	}
	slog.Info("starting raydio", "addr", cfg.Addr, "data_dir", cfg.DataDir, "cache_dir", cfg.CacheDir, "db_path", cfg.DBPath, "radios", len(cfg.Radios), "rate_limit_rps", cfg.RateLimitRPS, "rate_limit_burst", cfg.RateLimitBurst, "max_streams_per_ip", cfg.MaxStreamsPerIP, "max_events_per_ip", cfg.MaxEventsPerIP, "trusted_proxy_cidrs", cfg.TrustedProxyCIDRs, "client_ip_headers", cfg.ClientIPHeaders, "schedule_interval", cfg.ScheduleInterval, "stream_chunk_window", cfg.StreamChunkWindow, "stream_buffer_window", cfg.StreamBufferWindow, "stream_write_timeout", cfg.StreamWriteTimeout, "gap_frames", cfg.GapFrames)

	webFiles, err := loadWebFiles()
	if err != nil {
		return err
	}
	a := &app{
		cfg:        cfg,
		store:      st,
		stations:   stations,
		stationIDs: stationIDs,
		webFiles:   webFiles,
		streams:    newIPConnectionLimiter(cfg.MaxStreamsPerIP, clientIPs),
		events:     newIPConnectionLimiter(cfg.MaxEventsPerIP, clientIPs),
	}

	mux := http.NewServeMux()
	a.routes(mux)
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           newSecurityHeadersMiddleware(newRateLimitMiddleware(cfg.RateLimitRPS, cfg.RateLimitBurst, clientIPs, mux)),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
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
	if cfg.RateLimitRPS <= 0 {
		return fmt.Errorf("rate limit rps must be positive")
	}
	if cfg.RateLimitBurst <= 0 {
		return fmt.Errorf("rate limit burst must be positive")
	}
	if cfg.MaxStreamsPerIP <= 0 {
		return fmt.Errorf("max streams per ip must be positive")
	}
	if cfg.MaxEventsPerIP <= 0 {
		return fmt.Errorf("max events per ip must be positive")
	}
	if _, err := newClientIPResolver(cfg.TrustedProxyCIDRs, cfg.ClientIPHeaders); err != nil {
		return err
	}
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
	if len(cfg.Radios) == 0 {
		return fmt.Errorf("at least one radio is required")
	}
	return nil
}

func (a *app) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /", a.handleIndex)
	mux.HandleFunc("GET /app.js", a.static("app.js", "application/javascript; charset=utf-8"))
	mux.HandleFunc("GET /styles.css", a.static("styles.css", "text/css; charset=utf-8"))
	mux.HandleFunc("GET /api/stations", a.handleStations)
	mux.HandleFunc("GET /radio/{station}", a.handleRadio)
	mux.HandleFunc("GET /radio/{station}/api/now", a.handleNow)
	mux.HandleFunc("GET /radio/{station}/api/events", a.handleEvents)
	mux.HandleFunc("GET /radio/{station}/api/catalog", a.handleCatalog)
	mux.HandleFunc("GET /radio/{station}/covers/{id}", a.handleAsset("cover"))
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

func (a *app) station(w http.ResponseWriter, r *http.Request) (*stationRuntime, bool) {
	id := r.PathValue("station")
	rt, ok := a.stations[id]
	if !ok {
		http.NotFound(w, r)
		return nil, false
	}
	return rt, true
}

func (a *app) handleStations(w http.ResponseWriter, r *http.Request) {
	out := make([]radioConfig, 0, len(a.stationIDs))
	for _, id := range a.stationIDs {
		rt := a.stations[id]
		if rt == nil {
			continue
		}
		out = append(out, rt.station)
	}
	writeJSON(w, map[string]any{"stations": out})
}

func (a *app) handleRadio(w http.ResponseWriter, r *http.Request) {
	rt, ok := a.station(w, r)
	if !ok {
		return
	}
	release, ok := a.streams.acquire(r)
	if !ok {
		http.Error(w, "too many streams", http.StatusTooManyRequests)
		return
	}
	defer release()
	slog.Debug("radio stream connected", "remote", r.RemoteAddr)
	defer slog.Debug("radio stream disconnected", "remote", r.RemoteAddr)

	h := w.Header()
	h.Set("Content-Type", "audio/mpeg")
	h.Set("Cache-Control", "no-store, no-transform")
	h.Set("X-Accel-Buffering", "no")
	h.Set("Accept-Ranges", "none")

	flusher, ok := w.(http.Flusher)
	if !ok {
		internalServerError(w, r, "radio streaming unsupported", nil)
		return
	}

	ctx := r.Context()
	seq := rt.engine.LiveSeq()
	deadlineSupported := true
	for {
		p, next, err := rt.engine.WaitPacket(ctx, seq)
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
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return http.NewResponseController(w).SetWriteDeadline(time.Now().Add(timeout))
}

func newSecurityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'; img-src 'self' data:; media-src 'self' blob:; script-src 'self'; style-src 'self'; base-uri 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

type clientLimiter struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

type rateLimitShard struct {
	mu        sync.Mutex
	clients   map[string]*clientLimiter
	lastPrune time.Time
}

type rateLimitMiddleware struct {
	next     http.Handler
	rps      float64
	burst    int
	resolver clientIPResolver
	shards   []rateLimitShard
	now      func() time.Time
}

func newRateLimitMiddleware(rps float64, burst int, resolver clientIPResolver, next http.Handler) http.Handler {
	m := &rateLimitMiddleware{
		next:     next,
		rps:      rps,
		burst:    burst,
		resolver: resolver,
		shards:   make([]rateLimitShard, 32),
		now:      time.Now,
	}
	for i := range m.shards {
		m.shards[i].clients = map[string]*clientLimiter{}
	}
	return m
}

func (m *rateLimitMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		m.next.ServeHTTP(w, r)
		return
	}
	if !m.allow(m.resolver.clientIP(r)) {
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	m.next.ServeHTTP(w, r)
}

func (m *rateLimitMiddleware) allow(ip string) bool {
	now := m.now()
	shard := &m.shards[clientShard(ip, len(m.shards))]
	shard.mu.Lock()
	defer shard.mu.Unlock()
	m.pruneLocked(shard, now)
	entry := shard.clients[ip]
	if entry == nil {
		entry = &clientLimiter{limiter: rate.NewLimiter(rate.Limit(m.rps), m.burst)}
		shard.clients[ip] = entry
	}
	entry.lastSeen = now
	return entry.limiter.AllowN(now, 1)
}

func (m *rateLimitMiddleware) pruneLocked(shard *rateLimitShard, now time.Time) {
	if !shard.lastPrune.IsZero() && now.Sub(shard.lastPrune) < time.Minute {
		return
	}
	shard.lastPrune = now
	for ip, entry := range shard.clients {
		if now.Sub(entry.lastSeen) > 5*time.Minute {
			delete(shard.clients, ip)
		}
	}
}

func clientShard(ip string, n int) int {
	if n <= 1 {
		return 0
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(ip))
	return int(h.Sum32() % uint32(n))
}

type ipConnectionLimiter struct {
	limit    int
	resolver clientIPResolver
	mu       sync.Mutex
	clients  map[string]int
}

func newIPConnectionLimiter(limit int, resolver clientIPResolver) *ipConnectionLimiter {
	return &ipConnectionLimiter{limit: limit, resolver: resolver, clients: map[string]int{}}
}

func (l *ipConnectionLimiter) acquire(r *http.Request) (func(), bool) {
	if l == nil || l.limit <= 0 {
		return func() {}, true
	}
	ip := l.resolver.clientIP(r)
	l.mu.Lock()
	if l.clients[ip] >= l.limit {
		l.mu.Unlock()
		return nil, false
	}
	l.clients[ip]++
	l.mu.Unlock()
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.clients[ip] <= 1 {
			delete(l.clients, ip)
			return
		}
		l.clients[ip]--
	}, true
}

type clientIPResolver struct {
	trustedProxies []netip.Prefix
	headers        []string
}

var defaultClientIPHeaders = []string{"CF-Connecting-IP", "X-Forwarded-For", "X-Real-IP"}

func newClientIPResolver(trustedCIDRs, headers []string) (clientIPResolver, error) {
	if headers == nil {
		headers = defaultClientIPHeaders
	}
	resolver := clientIPResolver{
		headers: make([]string, 0, len(headers)),
	}
	for _, raw := range headers {
		header, ok := normalizeClientIPHeader(raw)
		if strings.TrimSpace(raw) == "" {
			return clientIPResolver{}, fmt.Errorf("server.client_ip_headers must not contain empty names")
		}
		if !ok {
			return clientIPResolver{}, fmt.Errorf("server.client_ip_headers contains unsupported header %q", raw)
		}
		resolver.headers = append(resolver.headers, header)
	}
	for _, raw := range trustedCIDRs {
		prefix, err := parseTrustedProxyPrefix(raw)
		if err != nil {
			return clientIPResolver{}, err
		}
		resolver.trustedProxies = append(resolver.trustedProxies, prefix)
	}
	return resolver, nil
}

func normalizeClientIPHeader(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "cf-connecting-ip":
		return "CF-Connecting-IP", true
	case "x-forwarded-for":
		return "X-Forwarded-For", true
	case "x-real-ip":
		return "X-Real-IP", true
	default:
		return "", false
	}
}

func parseTrustedProxyPrefix(raw string) (netip.Prefix, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return netip.Prefix{}, fmt.Errorf("server.trusted_proxy_cidrs must not contain empty values")
	}
	if prefix, err := netip.ParsePrefix(value); err == nil {
		return prefix.Masked(), nil
	}
	addr, err := netip.ParseAddr(value)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("server.trusted_proxy_cidrs contains invalid value %q", raw)
	}
	addr = addr.Unmap()
	return netip.PrefixFrom(addr, addr.BitLen()), nil
}

func (r clientIPResolver) clientIP(req *http.Request) string {
	peer, ok, fallback := remoteAddrIP(req.RemoteAddr)
	if !ok {
		return fallback
	}
	if !r.trusts(peer) {
		return peer.String()
	}
	for _, header := range r.headers {
		switch header {
		case "CF-Connecting-IP", "X-Real-IP":
			if addr, ok := parseHeaderIP(req.Header.Get(header)); ok {
				return addr.String()
			}
		case "X-Forwarded-For":
			raw := req.Header.Get(header)
			if raw == "" {
				continue
			}
			if addr, ok := parseXForwardedFor(raw); ok {
				return addr.String()
			}
			return peer.String()
		}
	}
	return peer.String()
}

func (r clientIPResolver) trusts(peer netip.Addr) bool {
	for _, prefix := range r.trustedProxies {
		if prefix.Contains(peer) {
			return true
		}
	}
	return false
}

func remoteAddrIP(remoteAddr string) (netip.Addr, bool, string) {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	host = strings.TrimSpace(host)
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false, host
	}
	addr = addr.Unmap()
	return addr, true, addr.String()
}

func parseHeaderIP(raw string) (netip.Addr, bool) {
	addr, err := netip.ParseAddr(strings.TrimSpace(raw))
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}

func parseXForwardedFor(raw string) (netip.Addr, bool) {
	first, _, _ := strings.Cut(raw, ",")
	return parseHeaderIP(first)
}

func (a *app) handleNow(w http.ResponseWriter, r *http.Request) {
	rt, ok := a.station(w, r)
	if !ok {
		return
	}
	writeJSON(w, rt.engine.Now())
}

func (a *app) handleEvents(w http.ResponseWriter, r *http.Request) {
	rt, ok := a.station(w, r)
	if !ok {
		return
	}
	release, ok := a.events.acquire(r)
	if !ok {
		http.Error(w, "too many event streams", http.StatusTooManyRequests)
		return
	}
	defer release()
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store, no-transform")
	h.Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		internalServerError(w, r, "event streaming unsupported", nil)
		return
	}

	ctx := r.Context()
	deadlineSupported := true
	setDeadline := func() {
		if !deadlineSupported {
			return
		}
		err := setWriteDeadline(w, a.cfg.StreamWriteTimeout)
		deadlineSupported = err == nil
		if err != nil {
			slog.Debug("event write deadline unsupported", "remote", r.RemoteAddr, "error", err)
		}
	}
	send := func(event string, payload any) bool {
		b, err := json.Marshal(payload)
		if err != nil {
			return false
		}
		setDeadline()
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	events, unsubscribe := rt.engine.SubscribeEvents()
	defer unsubscribe()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	if !send("now", rt.engine.Now()) {
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
			setDeadline()
			if _, err := io.WriteString(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (a *app) handleCatalog(w http.ResponseWriter, r *http.Request) {
	rt, ok := a.station(w, r)
	if !ok {
		return
	}
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
	rev, err := a.store.CatalogRevision(r.Context(), rt.station.UUID)
	if err != nil {
		internalServerError(w, r, "catalog revision failed", err)
		return
	}
	etag := catalogETag(rev, limit, r.URL.Query().Get("cursor"))
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, no-cache")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	tracks, err := a.store.ListCatalogPage(r.Context(), rt.station.UUID, afterTitle, afterID, limit+1)
	if err != nil {
		internalServerError(w, r, "catalog page failed", err)
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
		rt, ok := a.station(w, r)
		if !ok {
			return
		}
		id := r.PathValue("id")
		if asset, ok := rt.engine.Asset(id, kind); ok {
			a.serveAsset(w, r, asset)
			return
		}
		asset, err := a.store.Asset(r.Context(), rt.station.UUID, id, kind)
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			internalServerError(w, r, "asset lookup failed", err)
			return
		}
		rt.engine.RequestRefresh()
		a.serveAsset(w, r, asset)
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

func internalServerError(w http.ResponseWriter, r *http.Request, msg string, err error) {
	attrs := []any{"method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr}
	if err != nil {
		attrs = append(attrs, "error", err)
	}
	slog.Error(msg, attrs...)
	http.Error(w, "internal server error", http.StatusInternalServerError)
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

func (a *app) serveAsset(w http.ResponseWriter, r *http.Request, asset store.Asset) {
	serveAsset(w, r, asset, filepath.Join(a.cfg.CacheDir, "covers"))
}

func serveAsset(w http.ResponseWriter, r *http.Request, a store.Asset, coverRoot string) {
	if err := validateCoverAssetPath(a, coverRoot); err != nil {
		slog.Warn("rejecting cover asset", "path", a.Path, "mime", a.MIME, "error", err)
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(a.Path)
	if errors.Is(err, os.ErrNotExist) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		internalServerError(w, r, "asset open failed", err)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		internalServerError(w, r, "asset stat failed", err)
		return
	}
	etag := assetETag(a, info)
	w.Header().Set("Content-Type", a.MIME)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	http.ServeContent(w, r, filepath.Base(a.Path), info.ModTime(), f)
}

func validateCoverAssetPath(a store.Asset, coverRoot string) error {
	if a.Kind != "cover" {
		return fmt.Errorf("unsupported asset kind %q", a.Kind)
	}
	ext := strings.ToLower(filepath.Ext(a.Path))
	if expected := coverMIMEByExt(ext); expected == "" || a.MIME != expected {
		return fmt.Errorf("cover extension %q does not match mime %q", ext, a.MIME)
	}
	rootAbs, err := filepath.Abs(coverRoot)
	if err != nil {
		return err
	}
	pathAbs, err := filepath.Abs(a.Path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(rootAbs, pathAbs)
	if err != nil {
		return err
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("cover path escapes cover root")
	}
	return nil
}

func coverMIMEByExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	default:
		return ""
	}
}

func assetETag(a store.Asset, info os.FileInfo) string {
	key := fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%d", a.TrackID, a.Kind, a.Path, info.Size(), info.ModTime().UnixNano())
	sum := sha256.Sum256([]byte(key))
	return `"` + base64.RawURLEncoding.EncodeToString(sum[:12]) + `"`
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
