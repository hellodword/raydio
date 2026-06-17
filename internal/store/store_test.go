package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestMigrationAndTrackUpsert(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "raydio.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	first := Track{
		ID:            "aaa",
		SourcePath:    "/music/song.mp3",
		CachePath:     "/cache/aaa.mp3",
		Title:         "Song",
		Artist:        "Artist",
		DurationMs:    2400,
		FrameCount:    100,
		FrameSize:     576,
		Bitrate:       192000,
		SampleRate:    48000,
		Channels:      2,
		Status:        TrackStatusActive,
		SourceModUnix: 1,
	}
	if err := st.UpsertTrack(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.ID = "bbb"
	second.CachePath = "/cache/bbb.mp3"
	if err := st.UpsertTrack(ctx, second); err != nil {
		t.Fatal(err)
	}
	active, err := st.ListActiveTracks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].ID != "bbb" {
		t.Fatalf("source replacement should leave only bbb active: %+v", active)
	}
}

func TestSlotPersistence(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "raydio.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	slot := Slot{ID: "1-gap", StartUnixMs: 1000, EndUnixMs: 2000, IsSilence: true, FrameCount: 42}
	if err := st.UpsertSlot(ctx, slot); err != nil {
		t.Fatal(err)
	}
	got, err := st.SlotAt(ctx, 1500)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != slot.ID || !got.IsSilence || got.FrameCount != 42 {
		t.Fatalf("bad slot: %+v", got)
	}
}

func TestCatalogRevisionChangesWithTracksAndAssets(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "raydio.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	empty, err := st.CatalogRevision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	track := Track{
		ID:            "aaa",
		SourcePath:    "/music/song.mp3",
		CachePath:     "/cache/aaa.mp3",
		Title:         "Song",
		Artist:        "Artist",
		DurationMs:    2400,
		FrameCount:    100,
		FrameSize:     576,
		Bitrate:       192000,
		SampleRate:    48000,
		Channels:      2,
		Status:        TrackStatusActive,
		SourceModUnix: 1,
	}
	if err := st.UpsertTrack(ctx, track); err != nil {
		t.Fatal(err)
	}
	withTrack, err := st.CatalogRevision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if withTrack.Revision <= empty.Revision {
		t.Fatalf("track revision did not change: empty=%+v withTrack=%+v", empty, withTrack)
	}

	time.Sleep(time.Millisecond)
	if err := st.UpsertAsset(ctx, Asset{TrackID: track.ID, Kind: "cover", Path: "/cache/cover.jpg", MIME: "image/jpeg"}); err != nil {
		t.Fatal(err)
	}
	withAsset, err := st.CatalogRevision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if withAsset.Revision <= withTrack.Revision {
		t.Fatalf("asset revision did not change: withTrack=%+v withAsset=%+v", withTrack, withAsset)
	}
}

func TestNoopTrackUpsertDoesNotChangeCatalogRevision(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "raydio.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	track := Track{
		ID:            "aaa",
		SourcePath:    "/music/song.mp3",
		CachePath:     "/cache/aaa.mp3",
		Title:         "Song",
		Artist:        "Artist",
		DurationMs:    2400,
		FrameCount:    100,
		FrameSize:     576,
		Bitrate:       192000,
		SampleRate:    48000,
		Channels:      2,
		Status:        TrackStatusActive,
		SourceModUnix: 1,
	}
	if err := st.UpsertTrack(ctx, track); err != nil {
		t.Fatal(err)
	}
	first, err := st.CatalogRevision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertTrack(ctx, track); err != nil {
		t.Fatal(err)
	}
	same, err := st.CatalogRevision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if same.Revision != first.Revision {
		t.Fatalf("revision changed on noop upsert: first=%+v same=%+v", first, same)
	}

	track.Title = "Song v2"
	if err := st.UpsertTrack(ctx, track); err != nil {
		t.Fatal(err)
	}
	changed, err := st.CatalogRevision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Revision <= same.Revision {
		t.Fatalf("revision did not change after track update: same=%+v changed=%+v", same, changed)
	}
}

func TestNoopAssetUpsertDoesNotChangeCatalogRevision(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "raydio.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	track := Track{
		ID:            "aaa",
		SourcePath:    "/music/song.mp3",
		CachePath:     "/cache/aaa.mp3",
		Title:         "Song",
		Artist:        "Artist",
		DurationMs:    2400,
		FrameCount:    100,
		FrameSize:     576,
		Bitrate:       192000,
		SampleRate:    48000,
		Channels:      2,
		Status:        TrackStatusActive,
		SourceModUnix: 1,
	}
	if err := st.UpsertTrack(ctx, track); err != nil {
		t.Fatal(err)
	}
	asset := Asset{TrackID: track.ID, Kind: "cover", Path: "/cache/cover.jpg", MIME: "image/jpeg"}
	if err := st.UpsertAsset(ctx, asset); err != nil {
		t.Fatal(err)
	}
	first, err := st.CatalogRevision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertAsset(ctx, asset); err != nil {
		t.Fatal(err)
	}
	same, err := st.CatalogRevision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if same.Revision != first.Revision {
		t.Fatalf("revision changed on noop asset upsert: first=%+v same=%+v", first, same)
	}

	asset.Path = "/cache/cover-v2.jpg"
	if err := st.UpsertAsset(ctx, asset); err != nil {
		t.Fatal(err)
	}
	changed, err := st.CatalogRevision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Revision <= same.Revision {
		t.Fatalf("revision did not change after asset update: same=%+v changed=%+v", same, changed)
	}
}

func TestListCatalogPageReturnsPublicFieldsAndAssetURLs(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "raydio.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	tracks := []Track{
		{ID: "ccc", SourcePath: "/music/c.mp3", CachePath: "/cache/c.mp3", Title: "Charlie", Artist: "Artist", DurationMs: 3000, FrameCount: 125, FrameSize: 576, Bitrate: 192000, SampleRate: 48000, Channels: 2, Status: TrackStatusActive, SourceModUnix: 1},
		{ID: "aaa", SourcePath: "/music/a.mp3", CachePath: "/cache/a.mp3", Title: "Alpha", Artist: "Artist", DurationMs: 1000, FrameCount: 42, FrameSize: 576, Bitrate: 192000, SampleRate: 48000, Channels: 2, Status: TrackStatusActive, SourceModUnix: 1},
		{ID: "bbb", SourcePath: "/music/b.mp3", CachePath: "/cache/b.mp3", Title: "Bravo", Artist: "Artist", DurationMs: 2000, FrameCount: 84, FrameSize: 576, Bitrate: 192000, SampleRate: 48000, Channels: 2, Status: TrackStatusActive, SourceModUnix: 1},
	}
	for _, track := range tracks {
		if err := st.UpsertTrack(ctx, track); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.UpsertAsset(ctx, Asset{TrackID: "bbb", Kind: "cover", Path: "/cache/b.jpg", MIME: "image/jpeg"}); err != nil {
		t.Fatal(err)
	}

	first, err := st.ListCatalogPage(ctx, "", "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || first[0].ID != "aaa" || first[1].ID != "bbb" {
		t.Fatalf("first page = %+v", first)
	}
	if first[1].CoverURL != "/covers/bbb" {
		t.Fatalf("cover URL = %q", first[1].CoverURL)
	}

	next, err := st.ListCatalogPage(ctx, first[1].Title, first[1].ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(next) != 1 || next[0].ID != "ccc" {
		t.Fatalf("next page = %+v", next)
	}
}

func TestMigrationCreatesPerformanceIndexes(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "raydio.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	for _, name := range []string{
		"idx_schedule_slots_end",
		"idx_tracks_title_nocase",
		"idx_tracks_status_title_nocase",
	} {
		var count int
		if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("index %s count = %d, want 1", name, count)
		}
	}

	for _, name := range []string{
		"trg_tracks_catalog_revision_insert",
		"trg_tracks_catalog_revision_update",
		"trg_tracks_catalog_revision_delete",
		"trg_track_assets_catalog_revision_insert",
		"trg_track_assets_catalog_revision_update",
		"trg_track_assets_catalog_revision_delete",
	} {
		var count int
		if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name=?`, name).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("trigger %s count = %d, want 1", name, count)
		}
	}
}
