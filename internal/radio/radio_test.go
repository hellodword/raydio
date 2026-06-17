package radio

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"raydio/internal/store"
)

const testStationUUID = "00000000-0000-0000-0000-000000000001"

func TestSchedulerCreatesTrackAndGapSlots(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "raydio.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mustUpsertStation(t, ctx, st)

	track := store.Track{
		ID:            "abcdef1234567890",
		StationUUID:   testStationUUID,
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
	s := NewScheduler(st, testStationUUID, "/cache/silence.mp3", 5)
	s.fillAhead = 10 * time.Second
	if err := s.Ensure(ctx, now); err != nil {
		t.Fatal(err)
	}
	slots, err := st.SlotsEndingAfter(ctx, testStationUUID, now.UnixMilli())
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
	mustUpsertStation(t, ctx, st)

	now := time.Unix(100, 0)
	s := NewScheduler(st, testStationUUID, "/cache/silence.mp3", 5)
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

func TestPositionDoesNotMutateSchedule(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "raydio.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mustUpsertStation(t, ctx, st)

	now := time.Unix(100, 0)
	s := NewScheduler(st, testStationUUID, "/cache/silence.mp3", 5)
	if _, err := s.Position(ctx, now); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("Position error = %v, want sql.ErrNoRows", err)
	}
	slots, err := st.SlotsEndingAfter(ctx, testStationUUID, now.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if len(slots) != 0 {
		t.Fatalf("Position should not create slots, got %+v", slots)
	}
}

func TestPositionOrEnsureRefillsMissingSchedule(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "raydio.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mustUpsertStation(t, ctx, st)

	now := time.Unix(100, 0)
	s := NewScheduler(st, testStationUUID, "/cache/silence.mp3", 5)
	s.fillAhead = 100 * time.Millisecond
	pos, err := s.PositionOrEnsure(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if !pos.Slot.IsSilence {
		t.Fatalf("expected fallback silence slot, got %+v", pos)
	}
}

func TestNowAndChunksRecoverFromMissingSchedule(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "raydio.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mustUpsertStation(t, ctx, st)

	now := time.Unix(100, 0)
	s := NewScheduler(st, testStationUUID, "/cache/silence.mp3", 5)
	s.fillAhead = 100 * time.Millisecond
	gotNow, err := s.Now(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if !gotNow.IsSilence {
		t.Fatalf("expected silence now response, got %+v", gotNow)
	}
	chunks, err := s.Chunks(ctx, now, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) == 0 || chunks[0].Path != "/cache/silence.mp3" {
		t.Fatalf("expected silence chunks, got %+v", chunks)
	}
}

func mustUpsertStation(t *testing.T, ctx context.Context, st *store.Store) {
	t.Helper()
	if err := st.UpsertStation(ctx, store.Station{UUID: testStationUUID, Alias: "monthly", Enabled: true}); err != nil {
		t.Fatal(err)
	}
}
