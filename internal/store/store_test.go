package store

import (
	"context"
	"path/filepath"
	"testing"
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
