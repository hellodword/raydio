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
	silencePath string
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

type Chunk struct {
	Path   string
	Offset int64
	Length int64
}

type Position struct {
	Slot       store.Slot
	Track      *store.Track
	AssetURLs  map[string]string
	ElapsedMs  int64
	DurationMs int64
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

func NewScheduler(st *store.Store, stationUUID, silencePath string, gapFrames int64) *Scheduler {
	return &Scheduler{
		store:       st,
		stationUUID: stationUUID,
		silencePath: silencePath,
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

func (s *Scheduler) Position(ctx context.Context, now time.Time) (Position, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.positionLocked(ctx, now)
}

func (s *Scheduler) PositionOrEnsure(ctx context.Context, now time.Time) (Position, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pos, err := s.positionLocked(ctx, now)
	if err == nil {
		return pos, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Position{}, err
	}
	if err := s.store.DeleteFutureSlots(ctx, s.stationUUID, now.UnixMilli()); err != nil {
		return Position{}, err
	}
	if err := s.ensureLocked(ctx, now); err != nil {
		return Position{}, err
	}
	return s.positionLocked(ctx, now)
}

func (s *Scheduler) positionLocked(ctx context.Context, now time.Time) (Position, error) {
	nowMs := now.UnixMilli()
	slot, err := s.store.SlotAt(ctx, s.stationUUID, nowMs)
	if err != nil {
		return Position{}, err
	}
	pos := Position{
		Slot:       slot,
		ElapsedMs:  maxInt64(0, nowMs-slot.StartUnixMs),
		DurationMs: slot.EndUnixMs - slot.StartUnixMs,
	}
	if slot.TrackID.Valid {
		t, err := s.store.Track(ctx, slot.TrackID.String)
		if err == nil {
			pos.Track = &t
			pos.AssetURLs = assetURLs(ctx, s.store, s.stationUUID, t.ID)
		}
	}
	return pos, nil
}

func (s *Scheduler) Now(ctx context.Context, now time.Time) (Now, error) {
	pos, err := s.PositionOrEnsure(ctx, now)
	if err != nil {
		return Now{}, err
	}
	out := Now{
		ServerTimeMs: now.UnixMilli(),
		SlotID:       pos.Slot.ID,
		IsSilence:    pos.Slot.IsSilence,
		StartedAtMs:  pos.Slot.StartUnixMs,
		EndsAtMs:     pos.Slot.EndUnixMs,
		ElapsedMs:    pos.ElapsedMs,
		DurationMs:   pos.DurationMs,
	}
	if pos.Track != nil {
		out.Track = &NowTrack{
			ID:       pos.Track.ID,
			Title:    pos.Track.Title,
			Artist:   pos.Track.Artist,
			Album:    pos.Track.Album,
			CoverURL: pos.AssetURLs["cover"],
		}
	}
	return out, nil
}

func (s *Scheduler) Chunks(ctx context.Context, now time.Time, frames int64) ([]Chunk, error) {
	if frames <= 0 {
		return nil, errors.New("frames must be positive")
	}
	var chunks []Chunk
	cursor := now
	remaining := frames
	for remaining > 0 {
		pos, err := s.PositionOrEnsure(ctx, cursor)
		if err != nil {
			return nil, err
		}
		frameIndex := pos.ElapsedMs / audio.FrameDurationMs
		if frameIndex < 0 {
			frameIndex = 0
		}
		if frameIndex >= pos.Slot.FrameCount {
			cursor = time.UnixMilli(pos.Slot.EndUnixMs)
			continue
		}
		available := pos.Slot.FrameCount - frameIndex
		use := minInt64(remaining, available)
		path := s.silencePath
		if pos.Track != nil {
			path = pos.Track.CachePath
		}
		chunks = append(chunks, Chunk{
			Path:   path,
			Offset: frameIndex * audio.FrameSize,
			Length: use * audio.FrameSize,
		})
		remaining -= use
		cursor = cursor.Add(time.Duration(use*audio.SamplesPerFrame) * time.Second / time.Duration(audio.SampleRate))
	}
	return chunks, nil
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

func assetURLs(ctx context.Context, st *store.Store, stationUUID, trackID string) map[string]string {
	assets, err := st.AssetsByTrack(ctx, trackID)
	if err != nil {
		return map[string]string{}
	}
	out := map[string]string{}
	if _, ok := assets["cover"]; ok {
		out["cover"] = "/radio/" + stationUUID + "/covers/" + trackID
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
