package catalog

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"raydio/internal/audio"
	"raydio/internal/paths"
	"raydio/internal/store"

	"golang.org/x/sync/errgroup"
)

type Config struct {
	StationUUID   string
	InboxDir      string
	CacheDir      string
	SilenceFrames int64
	StableDelay   time.Duration
	ImportLimit   int
}

type Service struct {
	cfg   Config
	store *store.Store
}

type ScanResult struct {
	Seen      int  `json:"seen"`
	Processed int  `json:"processed"`
	Skipped   int  `json:"skipped"`
	Errors    int  `json:"errors"`
	Changed   bool `json:"changed"`
}

type scanCandidate struct {
	path string
	info os.FileInfo
}

func New(cfg Config, st *store.Store) *Service {
	if cfg.StableDelay == 0 {
		cfg.StableDelay = 250 * time.Millisecond
	}
	if cfg.SilenceFrames == 0 {
		cfg.SilenceFrames = 209
	}
	if cfg.ImportLimit <= 0 {
		cfg.ImportLimit = 2
	}
	return &Service{cfg: cfg, store: st}
}

func (s *Service) SilencePath() string {
	return paths.SilencePath(s.cfg.CacheDir, s.cfg.SilenceFrames)
}

func (s *Service) EnsureDirs(ctx context.Context) error {
	dirs := append([]string{s.cfg.InboxDir}, paths.CacheDirs(s.cfg.CacheDir)...)
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return audio.EnsureSilence(ctx, s.SilencePath(), s.cfg.SilenceFrames)
}

func (s *Service) Scan(ctx context.Context) (ScanResult, error) {
	if err := s.EnsureDirs(ctx); err != nil {
		return ScanResult{}, err
	}

	var result ScanResult
	seen := map[string]struct{}{}
	var candidates []scanCandidate
	err := filepath.WalkDir(s.cfg.InboxDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			result.Errors++
			return nil
		}
		if d.IsDir() {
			if path != s.cfg.InboxDir && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !isCandidate(path, d.Name()) {
			result.Skipped++
			return nil
		}
		result.Seen++
		stable, info, err := stableFile(path, s.cfg.StableDelay)
		if err != nil || !stable {
			result.Skipped++
			return nil
		}
		seen[path] = struct{}{}
		candidates = append(candidates, scanCandidate{path: path, info: info})
		return nil
	})
	if err != nil {
		return result, err
	}
	var mu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(s.cfg.ImportLimit)
	for _, candidate := range candidates {
		candidate := candidate
		g.Go(func() error {
			if err := gctx.Err(); err != nil {
				return err
			}
			changed, err := s.processFile(gctx, candidate.path, candidate.info)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				result.Errors++
				_ = s.markFileError(context.Background(), candidate.path, err)
				return nil
			}
			if changed {
				result.Changed = true
			}
			result.Processed++
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return result, err
	}
	missing, err := s.store.MarkMissingExcept(ctx, s.cfg.StationUUID, seen)
	if err != nil {
		return result, err
	}
	if len(missing) > 0 {
		result.Changed = true
	}
	if result.Changed {
		if err := s.store.DeleteFutureSlots(ctx, s.cfg.StationUUID, time.Now().UnixMilli()); err != nil {
			return result, err
		}
	}
	return result, nil
}

func (s *Service) processFile(ctx context.Context, path string, info os.FileInfo) (bool, error) {
	sum, err := fileSHA256(path)
	if err != nil {
		return false, err
	}
	contentHash := hex.EncodeToString(sum[:])
	id := stationTrackID(s.cfg.StationUUID, contentHash)
	cachePath := filepath.Join(s.cfg.CacheDir, "tracks", contentHash+".mp3")
	changed := false
	if _, err := os.Stat(cachePath); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		if err := audio.TranscodeCleanMP3(ctx, path, cachePath); err != nil {
			return false, err
		}
		changed = true
	} else if _, err := audio.ValidateCleanMP3(ctx, cachePath); err != nil {
		if err := audio.TranscodeCleanMP3(ctx, path, cachePath); err != nil {
			return false, err
		}
		changed = true
	}
	v, err := audio.ValidateCleanMP3(ctx, cachePath)
	if err != nil {
		return false, err
	}

	t := store.Track{
		ID:            id,
		StationUUID:   s.cfg.StationUUID,
		ContentHash:   contentHash,
		SourcePath:    path,
		SourceSize:    info.Size(),
		SourceModUnix: info.ModTime().Unix(),
		CachePath:     cachePath,
		Title:         strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)),
		Artist:        "Unknown artist",
		Album:         "",
		DurationMs:    v.DurationMs,
		FrameCount:    v.FrameCount,
		FrameSize:     v.FrameSize,
		Bitrate:       v.Bitrate,
		SampleRate:    v.SampleRate,
		Channels:      v.Channels,
		Status:        store.TrackStatusActive,
	}
	if old, err := s.store.Track(ctx, id); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return false, err
		}
		changed = true
	} else if trackChanged(old, t) {
		changed = true
	}
	if err := s.store.UpsertTrack(ctx, t); err != nil {
		return false, err
	}
	if err := s.syncAssets(ctx, id, path); err != nil {
		return false, err
	}
	return changed, nil
}

func trackChanged(old, next store.Track) bool {
	return old.StationUUID != next.StationUUID ||
		old.ContentHash != next.ContentHash ||
		old.SourcePath != next.SourcePath ||
		old.SourceSize != next.SourceSize ||
		old.SourceModUnix != next.SourceModUnix ||
		old.CachePath != next.CachePath ||
		old.Title != next.Title ||
		old.Artist != next.Artist ||
		old.Album != next.Album ||
		old.DurationMs != next.DurationMs ||
		old.FrameCount != next.FrameCount ||
		old.FrameSize != next.FrameSize ||
		old.Bitrate != next.Bitrate ||
		old.SampleRate != next.SampleRate ||
		old.Channels != next.Channels ||
		old.Status != store.TrackStatusActive
}

func (s *Service) markFileError(ctx context.Context, path string, err error) error {
	sum, sumErr := fileSHA256(path)
	if sumErr != nil {
		return sumErr
	}
	contentHash := hex.EncodeToString(sum[:])
	id := stationTrackID(s.cfg.StationUUID, contentHash)
	return s.store.SetTrackStatus(ctx, id, store.TrackStatusError, sql.NullString{String: err.Error(), Valid: true})
}

func stationTrackID(stationUUID, contentHash string) string {
	sum := sha256.Sum256([]byte(stationUUID + "\x00" + contentHash))
	return hex.EncodeToString(sum[:])
}

func (s *Service) syncAssets(ctx context.Context, trackID, sourcePath string) error {
	cover := firstExisting(
		replaceExt(sourcePath, ".jpg"),
		replaceExt(sourcePath, ".jpeg"),
		replaceExt(sourcePath, ".png"),
		replaceExt(sourcePath, ".webp"),
	)
	if cover != "" {
		ext := strings.ToLower(filepath.Ext(cover))
		dst := filepath.Join(s.cfg.CacheDir, "covers", trackID+ext)
		if err := copyFile(cover, dst); err != nil {
			return err
		}
		if err := s.store.UpsertAsset(ctx, store.Asset{TrackID: trackID, Kind: "cover", Path: dst, MIME: mimeByExt(ext)}); err != nil {
			return err
		}
		return nil
	}
	return s.store.DeleteAsset(ctx, trackID, "cover")
}

func isCandidate(path, name string) bool {
	lower := strings.ToLower(name)
	if strings.HasPrefix(name, ".") {
		return false
	}
	if strings.HasSuffix(lower, ".tmp") || strings.HasSuffix(lower, ".part") {
		return false
	}
	return strings.EqualFold(filepath.Ext(path), ".mp3")
}

func stableFile(path string, delay time.Duration) (bool, os.FileInfo, error) {
	info1, err := os.Stat(path)
	if err != nil {
		return false, nil, err
	}
	time.Sleep(delay)
	info2, err := os.Stat(path)
	if err != nil {
		return false, nil, err
	}
	if info1.Size() != info2.Size() || !info1.ModTime().Equal(info2.ModTime()) {
		return false, info2, nil
	}
	return true, info2, nil
}

func fileSHA256(path string) ([32]byte, error) {
	var zero [32]byte
	f, err := os.Open(path)
	if err != nil {
		return zero, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return zero, err
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out, nil
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return closeErr
	}
	return os.Rename(tmp, dst)
}

func replaceExt(path, ext string) string {
	return strings.TrimSuffix(path, filepath.Ext(path)) + ext
}

func firstExisting(paths ...string) string {
	for _, path := range paths {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return ""
}

func mimeByExt(ext string) string {
	if m := mime.TypeByExtension(ext); m != "" {
		return m
	}
	switch strings.ToLower(ext) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	default:
		return "application/octet-stream"
	}
}
