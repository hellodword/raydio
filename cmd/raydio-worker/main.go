package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"raydio/internal/catalog"
	"raydio/internal/paths"
	"raydio/internal/settings"
	"raydio/internal/store"
)

type config struct {
	ConfigPath     string
	DataDir        string
	InboxDir       string
	CacheDir       string
	DBPath         string
	Radios         []radioConfig
	RescanInterval time.Duration
	GapFrames      int64
	LogLevel       slog.Level
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
		slog.Error("raydio-worker stopped", "error", err)
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
	fs := flag.NewFlagSet("raydio-worker", flag.ContinueOnError)
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
		ConfigPath:     configPath,
		DataDir:        layout.DataDir,
		InboxDir:       layout.InboxDir,
		CacheDir:       layout.CacheDir,
		DBPath:         layout.DBPath,
		Radios:         radios,
		RescanInterval: fileCfg.Worker.RescanInterval,
		GapFrames:      fileCfg.GapFrames,
		LogLevel:       fileCfg.LogLevel,
	}, nil
}

func run(ctx context.Context, cfg config) error {
	if err := validateConfig(cfg); err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return err
	}
	slog.Info("starting raydio-worker", "data_dir", cfg.DataDir, "inbox_dir", cfg.InboxDir, "cache_dir", cfg.CacheDir, "db_path", cfg.DBPath, "radios", len(cfg.Radios), "rescan_interval", cfg.RescanInterval, "gap_frames", cfg.GapFrames)
	st, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	cats := make([]catalogRuntime, 0, len(cfg.Radios))
	for _, r := range cfg.Radios {
		if err := st.UpsertStation(ctx, store.Station{UUID: r.UUID, Alias: r.Alias, Enabled: true}); err != nil {
			return err
		}
		cats = append(cats, catalogRuntime{
			alias: r.Alias,
			uuid:  r.UUID,
			cat: catalog.New(catalog.Config{
				StationUUID:   r.UUID,
				InboxDir:      r.InboxDir,
				CacheDir:      cfg.CacheDir,
				SilenceFrames: cfg.GapFrames,
			}, st),
		})
	}
	if err := scanAll(ctx, cats); err != nil {
		return err
	}
	return scanLoop(ctx, cfg.RescanInterval, cats)
}

func validateConfig(cfg config) error {
	if cfg.RescanInterval <= 0 {
		return fmt.Errorf("rescan interval must be positive")
	}
	if cfg.GapFrames <= 0 {
		return fmt.Errorf("gap frame count must be positive")
	}
	if len(cfg.Radios) == 0 {
		return fmt.Errorf("at least one radio is required")
	}
	return nil
}

type catalogRuntime struct {
	alias string
	uuid  string
	cat   *catalog.Service
}

func scanLoop(ctx context.Context, interval time.Duration, cats []catalogRuntime) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			slog.Debug("catalog scan tick")
			if err := scanAll(ctx, cats); err != nil {
				if ctx.Err() != nil || errors.Is(err, context.Canceled) {
					return nil
				}
				slog.Error("catalog scan failed", "error", err)
			}
		}
	}
}

func scanAll(ctx context.Context, cats []catalogRuntime) error {
	for _, cat := range cats {
		if err := scan(ctx, cat); err != nil {
			return err
		}
	}
	return nil
}

func scan(ctx context.Context, cat catalogRuntime) error {
	start := time.Now()
	slog.Debug("catalog scan starting", "radio", cat.alias, "uuid", cat.uuid)
	result, err := cat.cat.Scan(ctx)
	if err != nil {
		return err
	}
	attrs := []any{
		"radio", cat.alias,
		"uuid", cat.uuid,
		"seen", result.Seen,
		"processed", result.Processed,
		"skipped", result.Skipped,
		"errors", result.Errors,
		"changed", result.Changed,
		"duration", time.Since(start),
	}
	if result.Changed || result.Errors > 0 {
		slog.Info("catalog scan finished", attrs...)
	} else {
		slog.Debug("catalog scan finished", attrs...)
	}
	return nil
}
