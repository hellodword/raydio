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
	"strings"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"golang.org/x/sync/errgroup"

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
			inbox: r.InboxDir,
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
	return watchAndScan(ctx, cfg.RescanInterval, cats, scan)
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
	inbox string
	cat   *catalog.Service
}

type scannerFunc func(context.Context, catalogRuntime) error

var watchDebounce = 500 * time.Millisecond

func watchAndScan(ctx context.Context, interval time.Duration, cats []catalogRuntime, scanFn scannerFunc) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()

	watched := map[string]struct{}{}
	for _, cat := range cats {
		if err := watchTree(watcher, watched, cat.inbox); err != nil {
			return err
		}
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	scanReq := make(chan string, maxInt(4, len(cats)*4))
	timers := map[string]*time.Timer{}
	catsByUUID := map[string]catalogRuntime{}
	for _, cat := range cats {
		catsByUUID[cat.uuid] = cat
	}
	schedule := func(uuid string) {
		if timer := timers[uuid]; timer != nil {
			timer.Reset(watchDebounce)
			return
		}
		timers[uuid] = time.AfterFunc(watchDebounce, func() {
			select {
			case scanReq <- uuid:
			default:
			}
		})
	}
	defer func() {
		for _, timer := range timers {
			timer.Stop()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			cat, ok := stationForPath(cats, event.Name)
			if !ok || ignoredPath(cat.inbox, event.Name) {
				continue
			}
			if event.Has(fsnotify.Create) || event.Has(fsnotify.Rename) {
				if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
					if err := watchTree(watcher, watched, event.Name); err != nil {
						slog.Warn("watch new directory failed", "path", event.Name, "error", err)
					}
				}
			}
			if event.Has(fsnotify.Create) || event.Has(fsnotify.Write) || event.Has(fsnotify.Rename) || event.Has(fsnotify.Remove) {
				schedule(cat.uuid)
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			slog.Warn("catalog watcher error", "error", err)
		case <-ticker.C:
			slog.Debug("catalog scan tick")
			if err := scanAll(ctx, cats); err != nil {
				if ctx.Err() != nil || errors.Is(err, context.Canceled) {
					return nil
				}
				slog.Error("catalog scan failed", "error", err)
			}
		case uuid := <-scanReq:
			cat, ok := catsByUUID[uuid]
			if !ok {
				continue
			}
			if err := scanFn(ctx, cat); err != nil {
				if ctx.Err() != nil || errors.Is(err, context.Canceled) {
					return nil
				}
				slog.Error("catalog scan failed", "radio", cat.alias, "uuid", cat.uuid, "error", err)
			}
		}
	}
}

func scanAll(ctx context.Context, cats []catalogRuntime) error {
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(maxInt(1, minInt(len(cats), 4)))
	for _, cat := range cats {
		cat := cat
		g.Go(func() error {
			return scan(ctx, cat)
		})
	}
	return g.Wait()
}

func watchTree(watcher *fsnotify.Watcher, watched map[string]struct{}, root string) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	return filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if path != root && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		clean := filepath.Clean(path)
		if _, ok := watched[clean]; ok {
			return nil
		}
		if err := watcher.Add(clean); err != nil {
			return err
		}
		watched[clean] = struct{}{}
		return nil
	})
}

func stationForPath(cats []catalogRuntime, path string) (catalogRuntime, bool) {
	clean := filepath.Clean(path)
	for _, cat := range cats {
		root := filepath.Clean(cat.inbox)
		if clean == root {
			return cat, true
		}
		rel, err := filepath.Rel(root, clean)
		if err == nil && rel != "." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".." {
			return cat, true
		}
	}
	return catalogRuntime{}, false
}

func ignoredPath(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil {
		return true
	}
	if rel == "." {
		return false
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if strings.HasPrefix(part, ".") {
			return true
		}
	}
	lower := strings.ToLower(filepath.Base(path))
	return strings.HasSuffix(lower, ".tmp") || strings.HasSuffix(lower, ".part")
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
