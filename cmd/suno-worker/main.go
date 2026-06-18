package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"raydio/internal/paths"
	"raydio/internal/settings"
	"raydio/internal/suno"
)

type config struct {
	ConfigPath    string
	DataDir       string
	InboxDir      string
	Radios        []radioConfig
	SyncInterval  time.Duration
	HTTPTimeout   time.Duration
	MaxAudioBytes int64
	MaxCoverBytes int64
	LogLevel      slog.Level
}

type radioConfig struct {
	Alias    string
	UUID     string
	InboxDir string
}

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
		slog.Error("suno-worker stopped", "error", err)
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
	fs := flag.NewFlagSet("suno-worker", flag.ContinueOnError)
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
	layout := paths.New(fileCfg.DataDir, fileCfg.Worker.InboxDir)
	radios := make([]radioConfig, 0, len(fileCfg.Radios))
	for _, r := range fileCfg.Radios {
		radios = append(radios, radioConfig{
			Alias:    r.Alias,
			UUID:     r.UUID,
			InboxDir: filepath.Join(layout.InboxDir, r.UUID),
		})
	}
	return config{
		ConfigPath:    configPath,
		DataDir:       layout.DataDir,
		InboxDir:      layout.InboxDir,
		Radios:        radios,
		SyncInterval:  fileCfg.Suno.SyncInterval,
		HTTPTimeout:   fileCfg.Suno.HTTPTimeout,
		MaxAudioBytes: fileCfg.Suno.MaxAudioBytes,
		MaxCoverBytes: fileCfg.Suno.MaxCoverBytes,
		LogLevel:      fileCfg.LogLevel,
	}, nil
}

func run(ctx context.Context, cfg config) error {
	if err := validateConfig(cfg); err != nil {
		return err
	}
	client := suno.NewClient(suno.DefaultBaseURL, &http.Client{Timeout: cfg.HTTPTimeout})
	syncer := suno.NewSyncer(client, slog.Default())
	syncer.SetDownloadLimits(cfg.MaxAudioBytes, cfg.MaxCoverBytes)
	slog.Info("starting suno-worker", "data_dir", cfg.DataDir, "inbox_dir", cfg.InboxDir, "radios", len(cfg.Radios), "sync_interval", cfg.SyncInterval, "http_timeout", cfg.HTTPTimeout, "max_audio_bytes", cfg.MaxAudioBytes, "max_cover_bytes", cfg.MaxCoverBytes)
	if err := syncAllFunc(ctx, syncer, cfg.Radios); err != nil {
		if isShutdownErr(ctx, err) {
			return nil
		}
		slog.Error("initial suno sync failed", "error", err)
	}
	ticker := time.NewTicker(cfg.SyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := syncAllFunc(ctx, syncer, cfg.Radios); err != nil {
				if isShutdownErr(ctx, err) {
					return nil
				}
				slog.Error("suno sync failed", "error", err)
			}
		}
	}
}

func validateConfig(cfg config) error {
	if cfg.SyncInterval <= 0 {
		return fmt.Errorf("suno sync interval must be positive")
	}
	if cfg.HTTPTimeout <= 0 {
		return fmt.Errorf("suno http timeout must be positive")
	}
	if cfg.MaxAudioBytes <= 0 {
		return fmt.Errorf("suno max audio bytes must be positive")
	}
	if cfg.MaxCoverBytes <= 0 {
		return fmt.Errorf("suno max cover bytes must be positive")
	}
	if len(cfg.Radios) == 0 {
		return fmt.Errorf("at least one radio is required")
	}
	return nil
}

var syncAllFunc = syncAll

func isShutdownErr(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, context.Canceled)
}

func syncAll(ctx context.Context, syncer *suno.Syncer, radios []radioConfig) error {
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(maxInt(1, minInt(len(radios), 4)))
	for _, r := range radios {
		r := r
		g.Go(func() error {
			start := time.Now()
			slog.Debug("suno sync starting", "radio", r.Alias, "uuid", r.UUID)
			result, err := syncer.SyncRadio(ctx, suno.Radio{Alias: r.Alias, UUID: r.UUID, InboxDir: r.InboxDir})
			attrs := []any{
				"radio", r.Alias,
				"uuid", r.UUID,
				"seen", result.Seen,
				"complete", result.Complete,
				"downloaded", result.Downloaded,
				"skipped", result.Skipped,
				"deleted", result.Deleted,
				"errors", result.Errors,
				"duration", time.Since(start),
			}
			if err != nil {
				slog.Error("suno sync failed", append(attrs, "error", err)...)
				return err
			}
			if result.Downloaded > 0 || result.Deleted > 0 || result.Errors > 0 {
				slog.Info("suno sync finished", attrs...)
			} else {
				slog.Debug("suno sync finished", attrs...)
			}
			return nil
		})
	}
	return g.Wait()
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
