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
	silencePath string
	gapFrames   int64
	fillAhead   time.Duration
	mu          sync.Mutex
	bag         []string
	lastTrack   string
	rng         *rand.Rand
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
	ID        string `json:"id"`
	Title     string `json:"title"`
	Artist    string `json:"artist"`
	Album     string `json:"album,omitempty"`
	CoverURL  string `json:"coverUrl,omitempty"`
	LyricsURL string `json:"lyricsUrl,omitempty"`
}

func NewScheduler(st *store.Store, silencePath string, gapFrames int64) *Scheduler {
	return &Scheduler{
		store:       st,
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
	if err := s.ensureLocked(ctx, now); err != nil {
		return Position{}, err
	}
	nowMs := now.UnixMilli()
	slot, err := s.store.SlotAt(ctx, nowMs)
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
			pos.AssetURLs = assetURLs(ctx, s.store, t.ID)
		}
	}
	return pos, nil
}

func (s *Scheduler) Now(ctx context.Context, now time.Time) (Now, error) {
	pos, err := s.Position(ctx, now)
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
			ID:        pos.Track.ID,
			Title:     pos.Track.Title,
			Artist:    pos.Track.Artist,
			Album:     pos.Track.Album,
			CoverURL:  pos.AssetURLs["cover"],
			LyricsURL: pos.AssetURLs["lyrics"],
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
		pos, err := s.Position(ctx, cursor)
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
	if err := s.store.DeleteSlotsBefore(ctx, cutoff); err != nil {
		return err
	}
	targetEnd := now.Add(s.fillAhead).UnixMilli()
	last, err := s.store.LastSlot(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		last = store.Slot{EndUnixMs: now.UnixMilli()}
	} else if err != nil {
		return err
	}
	start := last.EndUnixMs
	if start < now.UnixMilli() {
		start = now.UnixMilli()
	}
	for start < targetEnd {
		tracks, err := s.store.ListActiveTracks(ctx)
		if err != nil {
			return err
		}
		if len(tracks) == 0 {
			slot := s.makeSilenceSlot(start, s.gapFrames, "empty")
			if err := s.store.UpsertSlot(ctx, slot); err != nil {
				return err
			}
			start = slot.EndUnixMs
			continue
		}
		t := s.nextTrack(tracks)
		trackSlot := store.Slot{
			ID:          fmt.Sprintf("%d-%s", start, t.ID[:12]),
			StartUnixMs: start,
			EndUnixMs:   start + framesToMs(t.FrameCount),
			TrackID:     sql.NullString{String: t.ID, Valid: true},
			IsSilence:   false,
			FrameCount:  t.FrameCount,
		}
		if err := s.store.UpsertSlot(ctx, trackSlot); err != nil {
			return err
		}
		start = trackSlot.EndUnixMs
		gap := s.makeSilenceSlot(start, s.gapFrames, "gap")
		if err := s.store.UpsertSlot(ctx, gap); err != nil {
			return err
		}
		start = gap.EndUnixMs
	}
	return nil
}

func (s *Scheduler) makeSilenceSlot(startMs int64, frames int64, prefix string) store.Slot {
	return store.Slot{
		ID:          fmt.Sprintf("%d-%s", startMs, prefix),
		StartUnixMs: startMs,
		EndUnixMs:   startMs + framesToMs(frames),
		IsSilence:   true,
		FrameCount:  frames,
	}
}

func (s *Scheduler) nextTrack(tracks []store.Track) store.Track {
	byID := map[string]store.Track{}
	ids := make([]string, 0, len(tracks))
	for _, t := range tracks {
		byID[t.ID] = t
		ids = append(ids, t.ID)
	}
	sort.Strings(ids)
	if len(ids) == 1 {
		s.lastTrack = ids[0]
		return byID[ids[0]]
	}
	for len(s.bag) == 0 {
		s.bag = append([]string(nil), ids...)
		s.rng.Shuffle(len(s.bag), func(i, j int) { s.bag[i], s.bag[j] = s.bag[j], s.bag[i] })
		if len(s.bag) > 1 && s.bag[0] == s.lastTrack {
			s.bag[0], s.bag[1] = s.bag[1], s.bag[0]
		}
	}
	id := s.bag[0]
	s.bag = s.bag[1:]
	if _, ok := byID[id]; !ok {
		return s.nextTrack(tracks)
	}
	s.lastTrack = id
	return byID[id]
}

func assetURLs(ctx context.Context, st *store.Store, trackID string) map[string]string {
	assets, err := st.AssetsByTrack(ctx, trackID)
	if err != nil {
		return map[string]string{}
	}
	out := map[string]string{}
	if _, ok := assets["cover"]; ok {
		out["cover"] = "/covers/" + trackID
	}
	if _, ok := assets["lyrics"]; ok {
		out["lyrics"] = "/lyrics/" + trackID
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
