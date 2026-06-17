package radio

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"time"

	"raydio/internal/audio"
	"raydio/internal/store"
)

type Scheduler struct {
	store       *store.Store
	stationUUID string
	gapFrames   int64
	fillAhead   time.Duration
	mu          sync.Mutex
	bag         []string
	lastTrack   string
	rng         *rand.Rand
	tracks      trackSet
	catalogRev  store.CatalogRevision
	tracksReady bool
}

type trackSet struct {
	byID map[string]store.Track
	ids  []string
}

type Now struct {
	ServerTimeMs int64     `json:"serverTimeMs"`
	SlotID       string    `json:"slotId"`
	IsSilence    bool      `json:"isSilence"`
	StartedAtMs  int64     `json:"startedAtMs"`
	EndsAtMs     int64     `json:"endsAtMs"`
	ElapsedMs    int64     `json:"elapsedMs"`
	DurationMs   int64     `json:"durationMs"`
	Track        *NowTrack `json:"track,omitempty"`
}

type NowTrack struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Artist   string `json:"artist"`
	Album    string `json:"album,omitempty"`
	CoverURL string `json:"coverUrl,omitempty"`
}

func NewScheduler(st *store.Store, stationUUID, _ string, gapFrames int64) *Scheduler {
	return &Scheduler{
		store:       st,
		stationUUID: stationUUID,
		gapFrames:   gapFrames,
		fillAhead:   30 * time.Minute,
		rng:         rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (s *Scheduler) Ensure(ctx context.Context, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ensureLocked(ctx, now)
}

func (s *Scheduler) ensureLocked(ctx context.Context, now time.Time) error {
	cutoff := now.Add(-10 * time.Minute).UnixMilli()
	if err := s.store.DeleteSlotsBefore(ctx, s.stationUUID, cutoff); err != nil {
		return err
	}
	targetEnd := now.Add(s.fillAhead).UnixMilli()
	last, err := s.store.LastSlot(ctx, s.stationUUID)
	if errors.Is(err, sql.ErrNoRows) {
		last = store.Slot{EndUnixMs: now.UnixMilli()}
	} else if err != nil {
		return err
	}
	start := last.EndUnixMs
	if start < now.UnixMilli() {
		start = now.UnixMilli()
	}
	set, err := s.activeTrackSet(ctx)
	if err != nil {
		return err
	}
	slots := []store.Slot{}
	for start < targetEnd {
		if len(set.ids) == 0 {
			slot := s.makeSilenceSlot(start, s.gapFrames, "empty")
			slots = append(slots, slot)
			start = slot.EndUnixMs
			continue
		}
		t := s.nextTrack(set)
		trackSlot := store.Slot{
			ID:          fmt.Sprintf("%s-%d-%s", s.stationUUID, start, t.ID[:12]),
			StationUUID: s.stationUUID,
			StartUnixMs: start,
			EndUnixMs:   start + framesToMs(t.FrameCount),
			TrackID:     sql.NullString{String: t.ID, Valid: true},
			IsSilence:   false,
			FrameCount:  t.FrameCount,
		}
		slots = append(slots, trackSlot)
		start = trackSlot.EndUnixMs
		gap := s.makeSilenceSlot(start, s.gapFrames, "gap")
		slots = append(slots, gap)
		start = gap.EndUnixMs
	}
	return s.store.UpsertSlots(ctx, slots)
}

func (s *Scheduler) activeTrackSet(ctx context.Context) (trackSet, error) {
	rev, err := s.store.CatalogRevision(ctx, s.stationUUID)
	if err != nil {
		return trackSet{}, err
	}
	if s.tracksReady && rev == s.catalogRev {
		return s.tracks, nil
	}
	tracks, err := s.store.ListActiveTracks(ctx, s.stationUUID)
	if err != nil {
		return trackSet{}, err
	}
	s.tracks = newTrackSet(tracks)
	s.catalogRev = rev
	s.tracksReady = true
	s.bag = nil
	return s.tracks, nil
}

func (s *Scheduler) makeSilenceSlot(startMs int64, frames int64, prefix string) store.Slot {
	return store.Slot{
		ID:          fmt.Sprintf("%s-%d-%s", s.stationUUID, startMs, prefix),
		StationUUID: s.stationUUID,
		StartUnixMs: startMs,
		EndUnixMs:   startMs + framesToMs(frames),
		IsSilence:   true,
		FrameCount:  frames,
	}
}

func newTrackSet(tracks []store.Track) trackSet {
	byID := map[string]store.Track{}
	ids := make([]string, 0, len(tracks))
	for _, t := range tracks {
		byID[t.ID] = t
		ids = append(ids, t.ID)
	}
	sort.Strings(ids)
	return trackSet{byID: byID, ids: ids}
}

func (s *Scheduler) nextTrack(set trackSet) store.Track {
	if len(set.ids) == 1 {
		s.lastTrack = set.ids[0]
		return set.byID[set.ids[0]]
	}
	for {
		for len(s.bag) == 0 {
			s.bag = append([]string(nil), set.ids...)
			s.rng.Shuffle(len(s.bag), func(i, j int) { s.bag[i], s.bag[j] = s.bag[j], s.bag[i] })
			if len(s.bag) > 1 && s.bag[0] == s.lastTrack {
				s.bag[0], s.bag[1] = s.bag[1], s.bag[0]
			}
		}
		id := s.bag[0]
		s.bag = s.bag[1:]
		if t, ok := set.byID[id]; ok {
			s.lastTrack = id
			return t
		}
	}
}

func framesToMs(frames int64) int64 {
	return frames * audio.SamplesPerFrame * 1000 / audio.SampleRate
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
