package catalog

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"raydio/internal/audio"
	"raydio/internal/store"
)

func TestMergeMetadataSidecarOverridesTags(t *testing.T) {
	dir := t.TempDir()
	mp3 := filepath.Join(dir, "song.mp3")
	sidecar := filepath.Join(dir, "song.json")
	if err := os.WriteFile(sidecar, []byte(`{"title":"Side Title","artist":"Side Artist","album":"Side Album","comment":"Side Comment"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	meta := mergeMetadata(mp3, audio.Tags{
		Title:   "Tag Title",
		Artist:  "Tag Artist",
		Album:   "Tag Album",
		Comment: "Tag Comment",
	})
	if meta.Title != "Side Title" || meta.Artist != "Side Artist" || meta.Album != "Side Album" || meta.Comment != "Side Comment" {
		t.Fatalf("sidecar did not override tags: %+v", meta)
	}
}

func TestMergeMetadataFallsBackToTags(t *testing.T) {
	meta := mergeMetadata(filepath.Join(t.TempDir(), "song.mp3"), audio.Tags{
		Title:  "Tag Title",
		Artist: "Tag Artist",
	})
	if meta.Title != "Tag Title" || meta.Artist != "Tag Artist" {
		t.Fatalf("tag fallback failed: %+v", meta)
	}
}

func TestScanIsIdempotentAndUsesSidecar(t *testing.T) {
	requireFFmpeg(t)
	ctx := context.Background()
	dir := t.TempDir()
	inbox := filepath.Join(dir, "inbox")
	cache := filepath.Join(dir, "cache")
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(inbox, "song.mp3")
	if err := exec.CommandContext(ctx, "ffmpeg",
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "sine=frequency=330:duration=1",
		"-c:a", "libmp3lame", "-q:a", "4",
		"-f", "mp3", src,
	).Run(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inbox, "song.json"), []byte(`{"title":"Side Title","artist":"Side Artist"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(ctx, filepath.Join(dir, "raydio.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	svc := New(Config{InboxDir: inbox, CacheDir: cache, SilenceFrames: 5, StableDelay: time.Nanosecond}, st)
	first, err := svc.Scan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Changed {
		t.Fatal("first scan should change catalog")
	}
	second, err := svc.Scan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if second.Changed {
		t.Fatal("second scan without file changes should be idempotent")
	}
	tracks, err := st.ListActiveTracks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 1 {
		t.Fatalf("active tracks = %d, want 1", len(tracks))
	}
	if tracks[0].Title != "Side Title" || tracks[0].Artist != "Side Artist" {
		t.Fatalf("sidecar metadata missing: %+v", tracks[0])
	}
	if _, err := audio.ValidateCleanMP3(ctx, tracks[0].CachePath); err != nil {
		t.Fatal(err)
	}
}

func TestScanRemovesFutureSlotsForMissingTracks(t *testing.T) {
	requireFFmpeg(t)
	ctx := context.Background()
	dir := t.TempDir()
	inbox := filepath.Join(dir, "inbox")
	cache := filepath.Join(dir, "cache")
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(inbox, "gone.mp3")
	if err := exec.CommandContext(ctx, "ffmpeg",
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "sine=frequency=220:duration=1",
		"-c:a", "libmp3lame", "-q:a", "4",
		"-f", "mp3", src,
	).Run(); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(ctx, filepath.Join(dir, "raydio.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	svc := New(Config{InboxDir: inbox, CacheDir: cache, SilenceFrames: 5, StableDelay: time.Nanosecond}, st)
	if _, err := svc.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	tracks, err := st.ListActiveTracks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 1 {
		t.Fatalf("active tracks = %d, want 1", len(tracks))
	}
	start := time.Now().Add(time.Minute).UnixMilli()
	if err := st.UpsertSlot(ctx, store.Slot{
		ID:          "future-track",
		StartUnixMs: start,
		EndUnixMs:   start + 2400,
		TrackID:     sqlNullString(tracks[0].ID),
		FrameCount:  100,
	}); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(src); err != nil {
		t.Fatal(err)
	}
	result, err := svc.Scan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed {
		t.Fatal("removing source should change catalog")
	}
	active, err := st.ListActiveTracks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("active tracks after remove = %d, want 0", len(active))
	}
	slots, err := st.SlotsEndingAfter(ctx, time.Now().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if len(slots) != 0 {
		t.Fatalf("future slots should be deleted, got %+v", slots)
	}
}

func sqlNullString(v string) sql.NullString {
	return sql.NullString{String: v, Valid: true}
}

func requireFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg unavailable")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe unavailable")
	}
}
