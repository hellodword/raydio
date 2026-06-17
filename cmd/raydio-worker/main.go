package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"raydio/internal/catalog"
	"raydio/internal/paths"
	"raydio/internal/store"
)

type config struct {
	DataDir        string
	InboxDir       string
	CacheDir       string
	DBPath         string
	RescanInterval time.Duration
	GapFrames      int64
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
	flag.StringVar(&cfg.DataDir, "data", env("RAYDIO_DATA", "./data"), "data directory")
	flag.StringVar(&cfg.InboxDir, "inbox", env("RAYDIO_INBOX", ""), "inbox directory")
	flag.DurationVar(&cfg.RescanInterval, "rescan", envDuration("RAYDIO_RESCAN", 30*time.Second), "rescan interval")
	flag.Int64Var(&cfg.GapFrames, "gap-frames", envInt64("RAYDIO_GAP_FRAMES", 209), "silence gap frame count")
	flag.Parse()
	layout := paths.New(cfg.DataDir, cfg.InboxDir)
	cfg.InboxDir = layout.InboxDir
	cfg.CacheDir = layout.CacheDir
	cfg.DBPath = layout.DBPath
	return cfg
}

func run(ctx context.Context, cfg config) error {
	if err := validateConfig(cfg); err != nil {
		return err
	}
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
	if err := scan(ctx, cat); err != nil {
		return err
	}
	return scanLoop(ctx, cfg.RescanInterval, cat)
}

func validateConfig(cfg config) error {
	if cfg.RescanInterval <= 0 {
		return fmt.Errorf("rescan interval must be positive")
	}
	if cfg.GapFrames <= 0 {
		return fmt.Errorf("gap frame count must be positive")
	}
	return nil
}

func scanLoop(ctx context.Context, interval time.Duration, cat *catalog.Service) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := scan(ctx, cat); err != nil {
				if ctx.Err() != nil || errors.Is(err, context.Canceled) {
					return nil
				}
				log.Printf("scan failed: %v", err)
			}
		}
	}
}

func scan(ctx context.Context, cat *catalog.Service) error {
	result, err := cat.Scan(ctx)
	if err != nil {
		return err
	}
	if result.Changed || result.Errors > 0 {
		log.Printf("scan seen=%d processed=%d skipped=%d errors=%d changed=%t", result.Seen, result.Processed, result.Skipped, result.Errors, result.Changed)
	}
	return nil
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
