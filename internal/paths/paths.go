package paths

import (
	"fmt"
	"os"
	"path/filepath"
)

type Layout struct {
	DataDir    string
	InboxDir   string
	CacheDir   string
	TracksDir  string
	CoversDir  string
	LyricsDir  string
	SilenceDir string
	DBPath     string
}

func New(dataDir, inboxDir string) Layout {
	if inboxDir == "" {
		inboxDir = filepath.Join(dataDir, "inbox")
	}
	cacheDir := filepath.Join(dataDir, "cache")
	return Layout{
		DataDir:    dataDir,
		InboxDir:   inboxDir,
		CacheDir:   cacheDir,
		TracksDir:  filepath.Join(cacheDir, "tracks"),
		CoversDir:  filepath.Join(cacheDir, "covers"),
		LyricsDir:  filepath.Join(cacheDir, "lyrics"),
		SilenceDir: filepath.Join(cacheDir, "silence"),
		DBPath:     filepath.Join(dataDir, "raydio.sqlite"),
	}
}

func CacheDirs(cacheDir string) []string {
	return []string{
		filepath.Join(cacheDir, "tracks"),
		filepath.Join(cacheDir, "covers"),
		filepath.Join(cacheDir, "lyrics"),
		filepath.Join(cacheDir, "silence"),
	}
}

func SilencePath(cacheDir string, gapFrames int64) string {
	return filepath.Join(cacheDir, "silence", fmt.Sprintf("silence-%dframes.mp3", gapFrames))
}

func RequireServerCache(cacheDir string, gapFrames int64) error {
	for _, dir := range append([]string{cacheDir}, CacheDirs(cacheDir)...) {
		info, err := os.Stat(dir)
		if err != nil {
			return fmt.Errorf("media cache unavailable: %s missing; run raydio-worker with the shared data directory first", dir)
		}
		if !info.IsDir() {
			return fmt.Errorf("media cache unavailable: %s is not a directory", dir)
		}
	}
	silencePath := SilencePath(cacheDir, gapFrames)
	info, err := os.Stat(silencePath)
	if err != nil {
		return fmt.Errorf("media cache unavailable: %s missing; run raydio-worker with the shared data directory first", silencePath)
	}
	if info.IsDir() {
		return fmt.Errorf("media cache unavailable: %s is a directory", silencePath)
	}
	return nil
}
