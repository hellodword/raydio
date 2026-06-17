package radio

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"raydio/internal/audio"
	"raydio/internal/store"
)

func TestAudioRingSkipsSlowSubscriberToOldestBufferedPacket(t *testing.T) {
	r := newAudioRing(2)
	r.publish([]byte("a"))
	r.publish([]byte("b"))
	r.publish([]byte("c"))

	p, next, err := r.wait(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if p.Seq != 1 || string(p.Data) != "b" {
		t.Fatalf("packet = seq %d data %q, want seq 1 data b", p.Seq, p.Data)
	}
	if next != 2 {
		t.Fatalf("next = %d, want 2", next)
	}
}

func TestAudioRingLiveSeqStartsAtLatestPacket(t *testing.T) {
	r := newAudioRing(4)
	if got := r.liveSeq(); got != 0 {
		t.Fatalf("empty live seq = %d, want 0", got)
	}
	r.publish([]byte("a"))
	r.publish([]byte("b"))

	p, next, err := r.wait(context.Background(), r.liveSeq())
	if err != nil {
		t.Fatal(err)
	}
	if p.Seq != 1 || string(p.Data) != "b" {
		t.Fatalf("packet = seq %d data %q, want seq 1 data b", p.Seq, p.Data)
	}
	if next != 2 {
		t.Fatalf("next = %d, want 2", next)
	}
}

func TestFramesForDurationRoundsUpToWholeFrame(t *testing.T) {
	if got := framesForDuration(frameDuration(10)); got != 10 {
		t.Fatalf("exact frames = %d, want 10", got)
	}
	if got := framesForDuration(frameDuration(10) + 1); got != 11 {
		t.Fatalf("rounded frames = %d, want 11", got)
	}
}

func TestEngineRefreshSkipsCatalogReloadWhenRevisionUnchanged(t *testing.T) {
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

	e, err := NewEngine(EngineConfig{
		StationUUID:        testStationUUID,
		Scheduler:          NewScheduler(st, testStationUUID, "/cache/silence.mp3", 5),
		Store:              st,
		SilencePath:        "/cache/silence.mp3",
		RefreshInterval:    time.Minute,
		StreamChunkWindow:  frameDuration(1),
		StreamBufferWindow: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0)
	if err := e.Refresh(ctx, now); err != nil {
		t.Fatal(err)
	}
	e.stateMu.Lock()
	sentinel := e.tracks[track.ID]
	sentinel.Title = "Sentinel"
	e.tracks[track.ID] = sentinel
	e.stateMu.Unlock()

	if err := e.Refresh(ctx, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	e.stateMu.RLock()
	got := e.tracks[track.ID].Title
	e.stateMu.RUnlock()
	if got != "Sentinel" {
		t.Fatalf("catalog reloaded despite stable revision, title = %q", got)
	}

	time.Sleep(time.Millisecond)
	track.Title = "B"
	if err := st.UpsertTrack(ctx, track); err != nil {
		t.Fatal(err)
	}
	if err := e.Refresh(ctx, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	e.stateMu.RLock()
	got = e.tracks[track.ID].Title
	e.stateMu.RUnlock()
	if got != "B" {
		t.Fatalf("catalog did not reload after revision change, title = %q", got)
	}
}

func TestEngineRefreshCachesScheduledAssets(t *testing.T) {
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
	if err := st.UpsertAsset(ctx, store.Asset{TrackID: track.ID, Kind: "cover", Path: "/cache/a.jpg", MIME: "image/jpeg"}); err != nil {
		t.Fatal(err)
	}

	e, err := NewEngine(EngineConfig{
		StationUUID:        testStationUUID,
		Scheduler:          NewScheduler(st, testStationUUID, "/cache/silence.mp3", 5),
		Store:              st,
		SilencePath:        "/cache/silence.mp3",
		RefreshInterval:    time.Minute,
		StreamChunkWindow:  frameDuration(1),
		StreamBufferWindow: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Refresh(ctx, time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	asset, ok := e.Asset(track.ID, "cover")
	if !ok || asset.Path != "/cache/a.jpg" {
		t.Fatalf("cached asset = %+v ok=%v", asset, ok)
	}
}

func TestEngineRefreshDoesNotCacheAssetsOutsideSchedule(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "raydio.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mustUpsertStation(t, ctx, st)

	active := store.Track{
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
	inactive := active
	inactive.ID = "fedcba0987654321"
	inactive.SourcePath = "/inbox/inactive.mp3"
	inactive.CachePath = "/cache/inactive.mp3"
	inactive.Title = "Inactive"
	inactive.Status = store.TrackStatusMissing
	if err := st.UpsertTrack(ctx, active); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertTrack(ctx, inactive); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertAsset(ctx, store.Asset{TrackID: inactive.ID, Kind: "cover", Path: "/cache/inactive.jpg", MIME: "image/jpeg"}); err != nil {
		t.Fatal(err)
	}

	e, err := NewEngine(EngineConfig{
		StationUUID:        testStationUUID,
		Scheduler:          NewScheduler(st, testStationUUID, "/cache/silence.mp3", 5),
		Store:              st,
		SilencePath:        "/cache/silence.mp3",
		RefreshInterval:    time.Minute,
		StreamChunkWindow:  frameDuration(1),
		StreamBufferWindow: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Refresh(ctx, time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	asset, ok := e.Asset(inactive.ID, "cover")
	if ok {
		t.Fatalf("cached inactive asset = %+v", asset)
	}
}

func TestEngineReadAudioMissPublishesSilenceAndRequestsRefresh(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	silencePath := filepath.Join(dir, "silence.mp3")
	if err := os.WriteFile(silencePath, make([]byte, 5*audio.FrameSize), 0o644); err != nil {
		t.Fatal(err)
	}

	e := &Engine{
		cfg: EngineConfig{
			Scheduler:   &Scheduler{gapFrames: 5},
			SilencePath: silencePath,
		},
		refreshCh: make(chan struct{}, 1),
	}
	got, err := e.readAudio(ctx, time.Unix(100, 0), 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3*int(audio.FrameSize) {
		t.Fatalf("silence length = %d, want %d", len(got), 3*audio.FrameSize)
	}
	select {
	case <-e.refreshCh:
	default:
		t.Fatal("expected cache miss to request refresh")
	}
}
