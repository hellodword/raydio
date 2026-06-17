package radio

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"raydio/internal/store"
)

func TestSchedulerCreatesTrackAndGapSlots(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "raydio.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	track := store.Track{
		ID:            "abcdef1234567890",
		SourcePath:    "/inbox/a.mp3",
		CachePath:     "/cache/a.mp3",
		Title:         "A",
		Artist:        "Artist",
		DurationMs:    2400,
		FrameCount:    100,
		FrameSize:     576,
		Bitrate:       192000,
		SampleRate:    48000,
		Channels:      2,
		Status:        store.TrackStatusActive,
		SourceModUnix: 1,
	}
	if err := st.UpsertTrack(ctx, track); err != nil {
		t.Fatal(err)
	}

	now := time.Unix(100, 0)
	s := NewScheduler(st, "/cache/silence.mp3", 5)
	s.fillAhead = 10 * time.Second
	if err := s.Ensure(ctx, now); err != nil {
		t.Fatal(err)
	}
	slots, err := st.SlotsEndingAfter(ctx, now.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if len(slots) < 2 {
		t.Fatalf("want track and gap slots, got %+v", slots)
	}
	if slots[0].IsSilence || !slots[0].TrackID.Valid {
		t.Fatalf("first slot should be track: %+v", slots[0])
	}
	if !slots[1].IsSilence || slots[1].FrameCount != 5 {
		t.Fatalf("second slot should be 5-frame gap: %+v", slots[1])
	}
}

func TestSchedulerUsesSilenceWhenCatalogEmpty(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "raydio.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Unix(100, 0)
	s := NewScheduler(st, "/cache/silence.mp3", 5)
	s.fillAhead = 100 * time.Millisecond
	if err := s.Ensure(ctx, now); err != nil {
		t.Fatal(err)
	}
	pos, err := s.Position(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if !pos.Slot.IsSilence || pos.Track != nil {
		t.Fatalf("empty catalog should produce silence: %+v", pos)
	}
}
