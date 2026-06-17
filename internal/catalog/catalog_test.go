package catalog

import (
	"context"
	"database/sql"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"raydio/internal/audio"
	"raydio/internal/store"
)

const testStationUUID = "00000000-0000-0000-0000-000000000001"

func TestScanUsesFilenameMetadataAndIgnoresSourceSidecars(t *testing.T) {
	requireFFmpeg(t)
	ctx := context.Background()
	dir := t.TempDir()
	inbox := filepath.Join(dir, "inbox")
	cache := filepath.Join(dir, "cache")
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatal(err)
	}
	embeddedCover := filepath.Join(dir, "embedded.jpg")
	if err := exec.CommandContext(ctx, "ffmpeg",
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "color=c=red:s=16x16",
		"-frames:v", "1", embeddedCover,
	).Run(); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(inbox, "song.mp3")
	if err := exec.CommandContext(ctx, "ffmpeg",
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "sine=frequency=330:duration=1",
		"-i", embeddedCover,
		"-map", "0:a:0", "-map", "1:v:0",
		"-c:a", "libmp3lame", "-q:a", "4",
		"-c:v", "mjpeg", "-disposition:v:0", "attached_pic",
		"-metadata", "title=Tag Title",
		"-metadata", "artist=Tag Artist",
		"-f", "mp3", src,
	).Run(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inbox, "song.json"), []byte(`{"title":"Side Title","artist":"Side Artist"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inbox, "song.lrc"), []byte("[00:00.000]ignored"), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(ctx, filepath.Join(dir, "raydio.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mustUpsertStation(t, ctx, st)

	svc := New(Config{StationUUID: testStationUUID, InboxDir: inbox, CacheDir: cache, SilenceFrames: 5, StableDelay: time.Nanosecond}, st)
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
	tracks, err := st.ListActiveTracks(ctx, testStationUUID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 1 {
		t.Fatalf("active tracks = %d, want 1", len(tracks))
	}
	if tracks[0].Title != "song" || tracks[0].Artist != "Unknown artist" || tracks[0].Album != "" {
		t.Fatalf("unexpected imported metadata: %+v", tracks[0])
	}
	if _, err := audio.ValidateCleanMP3(ctx, tracks[0].CachePath); err != nil {
		t.Fatal(err)
	}
	assets, err := st.AssetsByTrack(ctx, tracks[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 0 {
		t.Fatalf("embedded cover or ignored sidecar produced assets: %+v", assets)
	}

	if err := copyTestFile(embeddedCover, filepath.Join(inbox, "song.jpg")); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	assets, err = st.AssetsByTrack(ctx, tracks[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if assets["cover"].Kind != "cover" || assets["cover"].MIME != "image/jpeg" {
		t.Fatalf("sidecar cover asset missing: %+v", assets)
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
	mustUpsertStation(t, ctx, st)

	svc := New(Config{StationUUID: testStationUUID, InboxDir: inbox, CacheDir: cache, SilenceFrames: 5, StableDelay: time.Nanosecond}, st)
	if _, err := svc.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	tracks, err := st.ListActiveTracks(ctx, testStationUUID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 1 {
		t.Fatalf("active tracks = %d, want 1", len(tracks))
	}
	start := time.Now().Add(time.Minute).UnixMilli()
	if err := st.UpsertSlot(ctx, store.Slot{
		ID:          "future-track",
		StationUUID: testStationUUID,
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
	active, err := st.ListActiveTracks(ctx, testStationUUID)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("active tracks after remove = %d, want 0", len(active))
	}
	slots, err := st.SlotsEndingAfter(ctx, testStationUUID, time.Now().UnixMilli())
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

func copyTestFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func mustUpsertStation(t *testing.T, ctx context.Context, st *store.Store) {
	t.Helper()
	if err := st.UpsertStation(ctx, store.Station{UUID: testStationUUID, Alias: "monthly", Enabled: true}); err != nil {
		t.Fatal(err)
	}
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
