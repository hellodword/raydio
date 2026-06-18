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
	store              *store.Store
	stationUUID        string
	sourceStationUUIDs []string
	gapFrames          int64
	fillAhead          time.Duration
	mu                 sync.Mutex
	stationBag         []string
	trackBags          map[string][]string
	lastTrack          string
	rng                *rand.Rand
	tracks             aggregateTrackSet
	catalogRev         store.CatalogRevision
	tracksReady        bool
}

type trackSet struct {
	byID map[string]store.Track
	ids  []string
}

type aggregateTrackSet struct {
	byID      map[string]store.Track
	byStation map[string]trackSet
	stations  []string
	ids       []string
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
	return NewSchedulerWithSources(st, stationUUID, []string{stationUUID}, gapFrames)
}

func NewSchedulerWithSources(st *store.Store, stationUUID string, sourceStationUUIDs []string, gapFrames int64) *Scheduler {
	sourceStationUUIDs = uniqueNonEmpty(sourceStationUUIDs)
	if len(sourceStationUUIDs) == 0 && stationUUID != "" {
		sourceStationUUIDs = []string{stationUUID}
	}
	return &Scheduler{
		store:              st,
		stationUUID:        stationUUID,
		sourceStationUUIDs: append([]string(nil), sourceStationUUIDs...),
		gapFrames:          gapFrames,
		fillAhead:          30 * time.Minute,
		trackBags:          map[string][]string{},
		rng:                rand.New(rand.NewSource(time.Now().UnixNano())),
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

func (s *Scheduler) activeTrackSet(ctx context.Context) (aggregateTrackSet, error) {
	rev, err := s.store.CatalogRevisionForStations(ctx, s.sourceStationUUIDs)
	if err != nil {
		return aggregateTrackSet{}, err
	}
	if s.tracksReady && rev == s.catalogRev {
		return s.tracks, nil
	}
	tracks, err := s.store.ListActiveTracksForStations(ctx, s.sourceStationUUIDs)
	if err != nil {
		return aggregateTrackSet{}, err
	}
	s.tracks = newAggregateTrackSet(tracks, s.sourceStationUUIDs)
	s.catalogRev = rev
	s.tracksReady = true
	s.stationBag = nil
	s.trackBags = map[string][]string{}
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

func newAggregateTrackSet(tracks []store.Track, stationOrder []string) aggregateTrackSet {
	grouped := map[string][]store.Track{}
	byID := map[string]store.Track{}
	ids := make([]string, 0, len(tracks))
	for _, t := range tracks {
		grouped[t.StationUUID] = append(grouped[t.StationUUID], t)
		byID[t.ID] = t
		ids = append(ids, t.ID)
	}
	sort.Strings(ids)
	out := aggregateTrackSet{
		byID:      byID,
		byStation: map[string]trackSet{},
		ids:       ids,
	}
	for _, stationID := range stationOrder {
		ts := newTrackSet(grouped[stationID])
		if len(ts.ids) == 0 {
			continue
		}
		out.byStation[stationID] = ts
		out.stations = append(out.stations, stationID)
	}
	return out
}

func (s *Scheduler) nextTrack(set aggregateTrackSet) store.Track {
	if len(set.ids) == 1 {
		s.lastTrack = set.ids[0]
		return set.byID[set.ids[0]]
	}
	for {
		if len(s.stationBag) == 0 {
			s.stationBag = append([]string(nil), set.stations...)
			s.rng.Shuffle(len(s.stationBag), func(i, j int) { s.stationBag[i], s.stationBag[j] = s.stationBag[j], s.stationBag[i] })
			s.movePlayableStationForward(set)
		}
		stationID := s.stationBag[0]
		s.stationBag = s.stationBag[1:]
		t, ok := s.nextTrackFromStation(set, stationID)
		if !ok {
			continue
		}
		if t.ID == s.lastTrack {
			s.stationBag = append(s.stationBag, stationID)
			continue
		}
		s.lastTrack = t.ID
		return t
	}
}

func (s *Scheduler) nextTrackFromStation(set aggregateTrackSet, stationID string) (store.Track, bool) {
	ts, ok := set.byStation[stationID]
	if !ok || len(ts.ids) == 0 {
		return store.Track{}, false
	}
	if len(ts.ids) == 1 {
		return ts.byID[ts.ids[0]], true
	}
	if s.trackBags == nil {
		s.trackBags = map[string][]string{}
	}
	bag := s.trackBags[stationID]
	if len(bag) == 0 {
		bag = append([]string(nil), ts.ids...)
		s.rng.Shuffle(len(bag), func(i, j int) { bag[i], bag[j] = bag[j], bag[i] })
		if len(bag) > 1 && bag[0] == s.lastTrack {
			for i := 1; i < len(bag); i++ {
				if bag[i] != s.lastTrack {
					bag[0], bag[i] = bag[i], bag[0]
					break
				}
			}
		}
	}
	for len(bag) > 0 {
		id := bag[0]
		bag = bag[1:]
		s.trackBags[stationID] = bag
		if t, ok := ts.byID[id]; ok {
			return t, true
		}
	}
	return store.Track{}, false
}

func (s *Scheduler) movePlayableStationForward(set aggregateTrackSet) {
	if len(s.stationBag) <= 1 || s.lastTrack == "" {
		return
	}
	if stationHasTrackOtherThan(set.byStation[s.stationBag[0]], s.lastTrack) {
		return
	}
	for i := 1; i < len(s.stationBag); i++ {
		if stationHasTrackOtherThan(set.byStation[s.stationBag[i]], s.lastTrack) {
			s.stationBag[0], s.stationBag[i] = s.stationBag[i], s.stationBag[0]
			return
		}
	}
}

func stationHasTrackOtherThan(set trackSet, trackID string) bool {
	for _, id := range set.ids {
		if id != trackID {
			return true
		}
	}
	return false
}

func uniqueNonEmpty(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
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
