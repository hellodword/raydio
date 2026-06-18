package radio

import (
	"context"
	"math/rand"
	"path/filepath"
	"testing"
	"time"

	"raydio/internal/store"
)

const (
	testStationUUID      = "00000000-0000-0000-0000-000000000001"
	secondStationUUID    = "00000000-0000-0000-0000-000000000002"
	aggregateStationUUID = "00000000-0000-0000-0000-000000000000"
)

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
	slots, err := st.SlotsEndingAfter(ctx, testStationUUID, now.UnixMilli()-1)
	if err != nil {
		t.Fatal(err)
	}
	if len(slots) == 0 || !slots[0].IsSilence {
		t.Fatalf("empty catalog should produce silence slots: %+v", slots)
	}
}

func TestSchedulerChoosesSourceStationsEvenly(t *testing.T) {
	s := NewSchedulerWithSources(nil, aggregateStationUUID, []string{testStationUUID, secondStationUUID}, 5)
	s.rng = rand.New(rand.NewSource(1))
	set := newAggregateTrackSet([]store.Track{
		{ID: "a1", StationUUID: testStationUUID},
		{ID: "b1", StationUUID: secondStationUUID},
		{ID: "b2", StationUUID: secondStationUUID},
		{ID: "b3", StationUUID: secondStationUUID},
	}, []string{testStationUUID, secondStationUUID})

	counts := map[string]int{}
	for i := 0; i < 20; i++ {
		track := s.nextTrack(set)
		counts[track.StationUUID]++
	}
	if counts[testStationUUID] != 10 || counts[secondStationUUID] != 10 {
		t.Fatalf("counts = %+v, want even station selection", counts)
	}
}

func TestAggregateSchedulerCreatesSlotsFromAllSourceStations(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "raydio.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mustUpsertStation(t, ctx, st)
	if err := st.UpsertStation(ctx, store.Station{UUID: secondStationUUID, Alias: "daily", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertStation(ctx, store.Station{UUID: aggregateStationUUID, Alias: "all", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	for _, track := range []store.Track{
		{ID: "aaaaaaaaaaaa0001", StationUUID: testStationUUID, SourcePath: "/inbox/a.mp3", CachePath: "/cache/a.mp3", Title: "A", Artist: "Artist", DurationMs: 2400, FrameCount: 100, FrameSize: 576, Bitrate: 192000, SampleRate: 48000, Channels: 2, Status: store.TrackStatusActive, SourceModUnix: 1},
		{ID: "bbbbbbbbbbbb0002", StationUUID: secondStationUUID, SourcePath: "/inbox/b.mp3", CachePath: "/cache/b.mp3", Title: "B", Artist: "Artist", DurationMs: 2400, FrameCount: 100, FrameSize: 576, Bitrate: 192000, SampleRate: 48000, Channels: 2, Status: store.TrackStatusActive, SourceModUnix: 1},
	} {
		if err := st.UpsertTrack(ctx, track); err != nil {
			t.Fatal(err)
		}
	}

	now := time.Unix(100, 0)
	s := NewSchedulerWithSources(st, aggregateStationUUID, []string{testStationUUID, secondStationUUID}, 5)
	s.fillAhead = 10 * time.Second
	if err := s.Ensure(ctx, now); err != nil {
		t.Fatal(err)
	}
	slots, err := st.SlotsEndingAfter(ctx, aggregateStationUUID, now.UnixMilli()-1)
	if err != nil {
		t.Fatal(err)
	}
	seenTracks := map[string]struct{}{}
	for _, slot := range slots {
		if slot.StationUUID != aggregateStationUUID {
			t.Fatalf("slot station = %q, want aggregate", slot.StationUUID)
		}
		if slot.TrackID.Valid {
			seenTracks[slot.TrackID.String] = struct{}{}
		}
	}
	if _, ok := seenTracks["aaaaaaaaaaaa0001"]; !ok {
		t.Fatalf("missing first source track in slots: %+v", slots)
	}
	if _, ok := seenTracks["bbbbbbbbbbbb0002"]; !ok {
		t.Fatalf("missing second source track in slots: %+v", slots)
	}
}

func mustUpsertStation(t *testing.T, ctx context.Context, st *store.Store) {
	t.Helper()
	if err := st.UpsertStation(ctx, store.Station{UUID: testStationUUID, Alias: "monthly", Enabled: true}); err != nil {
		t.Fatal(err)
	}
}
