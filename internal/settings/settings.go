package settings

import (
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.yaml.in/yaml/v4"
)

type File struct {
	DataDir   string
	GapFrames int64
	LogLevel  slog.Level
	Radios    []Radio
	Server    Server
	Suno      Suno
	Worker    Worker
}

type Radio struct {
	Alias string
	UUID  string
}

type Server struct {
	Addr               string
	RateLimitRPS       float64
	RateLimitBurst     int
	TrustedProxyCIDRs  []string
	ClientIPHeaders    []string
	ScheduleInterval   time.Duration
	StreamChunkWindow  time.Duration
	StreamBufferWindow time.Duration
	StreamWriteTimeout time.Duration
}

type Suno struct {
	SyncInterval time.Duration
	HTTPTimeout  time.Duration
}

type Worker struct {
	InboxDir       string
	RescanInterval time.Duration
}

type rawFile struct {
	DataDir   *string    `yaml:"data_dir"`
	GapFrames *int64     `yaml:"gap_frames"`
	LogLevel  *string    `yaml:"log_level"`
	Radios    []rawRadio `yaml:"radios"`
	Server    rawServer  `yaml:"server"`
	Suno      rawSuno    `yaml:"suno"`
	Worker    rawWorker  `yaml:"worker"`
}

type rawRadio struct {
	Alias string `yaml:"alias"`
	UUID  string `yaml:"uuid"`
}

type rawServer struct {
	Addr               *string  `yaml:"addr"`
	RateLimitRPS       *float64 `yaml:"rate_limit_rps"`
	RateLimitBurst     *int     `yaml:"rate_limit_burst"`
	TrustedProxyCIDRs  []string `yaml:"trusted_proxy_cidrs"`
	ClientIPHeaders    []string `yaml:"client_ip_headers"`
	ScheduleInterval   *string  `yaml:"schedule_interval"`
	StreamChunkWindow  *string  `yaml:"stream_chunk_window"`
	StreamBufferWindow *string  `yaml:"stream_buffer_window"`
	StreamWriteTimeout *string  `yaml:"stream_write_timeout"`
}

type rawSuno struct {
	SyncInterval *string `yaml:"sync_interval"`
	HTTPTimeout  *string `yaml:"http_timeout"`
}

type rawWorker struct {
	InboxDir       *string `yaml:"inbox_dir"`
	RescanInterval *string `yaml:"rescan_interval"`
}

func Defaults() File {
	return File{
		DataDir:   "./data",
		GapFrames: 209,
		LogLevel:  slog.LevelDebug,
		Server: Server{
			Addr:               ":8080",
			RateLimitRPS:       10,
			RateLimitBurst:     30,
			ClientIPHeaders:    defaultClientIPHeaders(),
			ScheduleInterval:   time.Minute,
			StreamChunkWindow:  480 * time.Millisecond,
			StreamBufferWindow: 2 * time.Second,
			StreamWriteTimeout: 5 * time.Second,
		},
		Suno: Suno{
			SyncInterval: 30 * time.Minute,
			HTTPTimeout:  30 * time.Second,
		},
		Worker: Worker{
			RescanInterval: 30 * time.Second,
		},
	}
}

func Load(path string) (File, error) {
	cfg := Defaults()
	b, err := os.ReadFile(path)
	if err != nil {
		return File{}, fmt.Errorf("read config %s: %w", path, err)
	}
	if err := parseYAML(b, &cfg); err != nil {
		return File{}, fmt.Errorf("parse config %s: %w", path, err)
	}
	resolvePaths(filepath.Dir(path), &cfg)
	if err := validateRadios(&cfg); err != nil {
		return File{}, fmt.Errorf("parse config %s: %w", path, err)
	}
	return cfg, nil
}

func resolvePaths(base string, cfg *File) {
	cfg.DataDir = resolvePath(base, cfg.DataDir)
	cfg.Worker.InboxDir = resolvePath(base, cfg.Worker.InboxDir)
}

func resolvePath(base, path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Clean(filepath.Join(base, path))
}

func parseYAML(b []byte, cfg *File) error {
	var raw rawFile
	if err := yaml.Load(b, &raw, yaml.WithKnownFields()); err != nil {
		return err
	}
	if raw.DataDir != nil {
		cfg.DataDir = *raw.DataDir
	}
	if raw.GapFrames != nil {
		cfg.GapFrames = *raw.GapFrames
	}
	if raw.LogLevel != nil {
		level, err := parseLogLevel(*raw.LogLevel)
		if err != nil {
			return err
		}
		cfg.LogLevel = level
	}
	if raw.Server.Addr != nil {
		cfg.Server.Addr = *raw.Server.Addr
	}
	if raw.Server.RateLimitRPS != nil {
		cfg.Server.RateLimitRPS = *raw.Server.RateLimitRPS
	}
	if raw.Server.RateLimitBurst != nil {
		cfg.Server.RateLimitBurst = *raw.Server.RateLimitBurst
	}
	if raw.Server.TrustedProxyCIDRs != nil {
		cfg.Server.TrustedProxyCIDRs = append([]string(nil), raw.Server.TrustedProxyCIDRs...)
	}
	if raw.Server.ClientIPHeaders != nil {
		cfg.Server.ClientIPHeaders = append([]string(nil), raw.Server.ClientIPHeaders...)
	}
	if err := parseOptionalDuration(raw.Server.ScheduleInterval, "server.schedule_interval", &cfg.Server.ScheduleInterval); err != nil {
		return err
	}
	if err := parseOptionalDuration(raw.Server.StreamChunkWindow, "server.stream_chunk_window", &cfg.Server.StreamChunkWindow); err != nil {
		return err
	}
	if err := parseOptionalDuration(raw.Server.StreamBufferWindow, "server.stream_buffer_window", &cfg.Server.StreamBufferWindow); err != nil {
		return err
	}
	if err := parseOptionalDuration(raw.Server.StreamWriteTimeout, "server.stream_write_timeout", &cfg.Server.StreamWriteTimeout); err != nil {
		return err
	}
	if err := parseOptionalDuration(raw.Suno.SyncInterval, "suno.sync_interval", &cfg.Suno.SyncInterval); err != nil {
		return err
	}
	if err := parseOptionalDuration(raw.Suno.HTTPTimeout, "suno.http_timeout", &cfg.Suno.HTTPTimeout); err != nil {
		return err
	}
	if raw.Worker.InboxDir != nil {
		cfg.Worker.InboxDir = *raw.Worker.InboxDir
	}
	if err := parseOptionalDuration(raw.Worker.RescanInterval, "worker.rescan_interval", &cfg.Worker.RescanInterval); err != nil {
		return err
	}
	if raw.Radios != nil {
		cfg.Radios = make([]Radio, 0, len(raw.Radios))
		for _, r := range raw.Radios {
			cfg.Radios = append(cfg.Radios, Radio{Alias: r.Alias, UUID: r.UUID})
		}
	}
	if err := validateServer(cfg); err != nil {
		return err
	}
	return nil
}

func parseOptionalDuration(raw *string, name string, out *time.Duration) error {
	if raw == nil {
		return nil
	}
	d, err := time.ParseDuration(*raw)
	if err != nil {
		return fmt.Errorf("%s must be a Go duration", name)
	}
	*out = d
	return nil
}

var (
	uuidPattern  = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	aliasPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)
)

func defaultClientIPHeaders() []string {
	return []string{"CF-Connecting-IP", "X-Forwarded-For", "X-Real-IP"}
}

func validateServer(cfg *File) error {
	normalizedHeaders := make([]string, 0, len(cfg.Server.ClientIPHeaders))
	for _, raw := range cfg.Server.ClientIPHeaders {
		header, ok := normalizeClientIPHeader(raw)
		if strings.TrimSpace(raw) == "" {
			return fmt.Errorf("server.client_ip_headers must not contain empty names")
		}
		if !ok {
			return fmt.Errorf("server.client_ip_headers contains unsupported header %q", raw)
		}
		normalizedHeaders = append(normalizedHeaders, header)
	}
	cfg.Server.ClientIPHeaders = normalizedHeaders

	normalizedCIDRs := make([]string, 0, len(cfg.Server.TrustedProxyCIDRs))
	for _, raw := range cfg.Server.TrustedProxyCIDRs {
		value := strings.TrimSpace(raw)
		if value == "" {
			return fmt.Errorf("server.trusted_proxy_cidrs must not contain empty values")
		}
		prefix, err := parseTrustedProxyPrefix(value)
		if err != nil {
			return fmt.Errorf("server.trusted_proxy_cidrs contains invalid value %q", raw)
		}
		normalizedCIDRs = append(normalizedCIDRs, prefix.String())
	}
	cfg.Server.TrustedProxyCIDRs = normalizedCIDRs
	return nil
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

func parseTrustedProxyPrefix(value string) (netip.Prefix, error) {
	if prefix, err := netip.ParsePrefix(value); err == nil {
		return prefix.Masked(), nil
	}
	addr, err := netip.ParseAddr(value)
	if err != nil {
		return netip.Prefix{}, err
	}
	addr = addr.Unmap()
	return netip.PrefixFrom(addr, addr.BitLen()), nil
}

func validateRadios(cfg *File) error {
	if len(cfg.Radios) == 0 {
		return fmt.Errorf("at least one radio is required")
	}
	aliases := map[string]struct{}{}
	uuids := map[string]struct{}{}
	for i := range cfg.Radios {
		r := &cfg.Radios[i]
		r.Alias = strings.TrimSpace(r.Alias)
		r.UUID = strings.ToLower(strings.TrimSpace(r.UUID))
		if r.Alias == "" {
			return fmt.Errorf("radio %d alias is required", i+1)
		}
		if !aliasPattern.MatchString(r.Alias) {
			return fmt.Errorf("radio %q alias must use lowercase letters, numbers, and hyphens", r.Alias)
		}
		if uuidPattern.MatchString(r.Alias) {
			return fmt.Errorf("radio alias %q must not look like a uuid", r.Alias)
		}
		if !uuidPattern.MatchString(r.UUID) {
			return fmt.Errorf("radio %q uuid must be a canonical uuid", r.Alias)
		}
		if _, ok := aliases[r.Alias]; ok {
			return fmt.Errorf("duplicate radio alias %q", r.Alias)
		}
		if _, ok := uuids[r.UUID]; ok {
			return fmt.Errorf("duplicate radio uuid %q", r.UUID)
		}
		aliases[r.Alias] = struct{}{}
		uuids[r.UUID] = struct{}{}
	}
	return nil
}

func parseLogLevel(value string) (slog.Level, error) {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "DEBUG":
		return slog.LevelDebug, nil
	case "INFO":
		return slog.LevelInfo, nil
	case "WARN", "WARNING":
		return slog.LevelWarn, nil
	case "ERROR":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("log_level must be DEBUG, INFO, WARN, or ERROR")
	}
}
