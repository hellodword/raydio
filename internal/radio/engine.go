package radio

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"math"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"raydio/internal/audio"
	"raydio/internal/store"
)

type EngineConfig struct {
	Scheduler          *Scheduler
	Store              *store.Store
	SilencePath        string
	RefreshInterval    time.Duration
	StreamChunkWindow  time.Duration
	StreamBufferWindow time.Duration
}

type Engine struct {
	cfg           EngineConfig
	chunkFrames   int64
	chunkDuration time.Duration
	ring          *audioRing

	stateMu   sync.RWMutex
	slots     []store.Slot
	tracks    map[string]store.Track
	assetURLs map[string]map[string]string
	catalog   []store.Track

	refreshMu sync.Mutex
	refreshCh chan struct{}

	now atomic.Value

	eventMu       sync.Mutex
	eventSubs     map[chan Now]struct{}
	lastEventSlot string
	catalogRev    store.CatalogRevision
	catalogLoaded bool

	currentFilePath string
	currentFile     *os.File
}

type AudioPacket struct {
	Seq  int64
	Data []byte
}

type enginePosition struct {
	slot       store.Slot
	track      *store.Track
	assetURLs  map[string]string
	elapsedMs  int64
	durationMs int64
}

func NewEngine(cfg EngineConfig) (*Engine, error) {
	if cfg.Scheduler == nil {
		return nil, errors.New("scheduler is required")
	}
	if cfg.Store == nil {
		return nil, errors.New("store is required")
	}
	if cfg.SilencePath == "" {
		return nil, errors.New("silence path is required")
	}
	if cfg.RefreshInterval <= 0 {
		return nil, errors.New("refresh interval must be positive")
	}
	frames := framesForDuration(cfg.StreamChunkWindow)
	if frames <= 0 {
		return nil, errors.New("stream chunk window must be positive")
	}
	chunkDuration := frameDuration(frames)
	if cfg.StreamBufferWindow <= 0 {
		return nil, errors.New("stream buffer window must be positive")
	}
	capacity := int(math.Ceil(float64(cfg.StreamBufferWindow) / float64(chunkDuration)))
	if capacity < 2 {
		capacity = 2
	}
	e := &Engine{
		cfg:           cfg,
		chunkFrames:   frames,
		chunkDuration: chunkDuration,
		ring:          newAudioRing(capacity),
		tracks:        map[string]store.Track{},
		assetURLs:     map[string]map[string]string{},
		refreshCh:     make(chan struct{}, 1),
		eventSubs:     map[chan Now]struct{}{},
	}
	e.now.Store(Now{ServerTimeMs: time.Now().UnixMilli()})
	return e, nil
}

func (e *Engine) Start(ctx context.Context) error {
	now := time.Now()
	if err := e.Refresh(ctx, now); err != nil {
		return err
	}
	if err := e.updateNow(ctx, now); err != nil {
		return err
	}
	go e.refreshLoop(ctx)
	go e.producerLoop(ctx)
	return nil
}

func (e *Engine) Refresh(ctx context.Context, now time.Time) error {
	e.refreshMu.Lock()
	defer e.refreshMu.Unlock()

	if err := e.cfg.Scheduler.Ensure(ctx, now); err != nil {
		return err
	}
	slots, err := e.cfg.Store.SlotsEndingAfter(ctx, now.Add(-e.cfg.StreamBufferWindow).UnixMilli())
	if err != nil {
		return err
	}
	rev, err := e.cfg.Store.CatalogRevision(ctx)
	if err != nil {
		return err
	}

	e.stateMu.RLock()
	loaded := e.catalogLoaded
	oldRev := e.catalogRev
	unknownTrack := loaded && hasUnknownTrack(slots, e.tracks)
	e.stateMu.RUnlock()

	if loaded && rev == oldRev && !unknownTrack {
		e.stateMu.Lock()
		e.slots = slots
		e.stateMu.Unlock()
		return nil
	}

	tracks, err := e.cfg.Store.ListTracks(ctx)
	if err != nil {
		return err
	}
	assets, err := e.cfg.Store.ListAssets(ctx)
	if err != nil {
		return err
	}

	trackByID := make(map[string]store.Track, len(tracks))
	for _, t := range tracks {
		trackByID[t.ID] = t
	}
	urls := make(map[string]map[string]string)
	for _, a := range assets {
		if urls[a.TrackID] == nil {
			urls[a.TrackID] = map[string]string{}
		}
		switch a.Kind {
		case "cover":
			urls[a.TrackID]["cover"] = "/covers/" + a.TrackID
		case "lyrics":
			urls[a.TrackID]["lyrics"] = "/lyrics/" + a.TrackID
		}
	}

	e.stateMu.Lock()
	e.slots = slots
	e.tracks = trackByID
	e.assetURLs = urls
	e.catalog = append([]store.Track(nil), tracks...)
	e.catalogRev = rev
	e.catalogLoaded = true
	e.stateMu.Unlock()
	return nil
}

func (e *Engine) Now() Now {
	if v := e.now.Load(); v != nil {
		return v.(Now)
	}
	return Now{ServerTimeMs: time.Now().UnixMilli()}
}

func (e *Engine) Catalog() []store.Track {
	e.stateMu.RLock()
	defer e.stateMu.RUnlock()
	return append([]store.Track(nil), e.catalog...)
}

func (e *Engine) SubscribeEvents() (<-chan Now, func()) {
	ch := make(chan Now, 8)
	e.eventMu.Lock()
	e.eventSubs[ch] = struct{}{}
	e.eventMu.Unlock()
	return ch, func() {
		e.eventMu.Lock()
		delete(e.eventSubs, ch)
		close(ch)
		e.eventMu.Unlock()
	}
}

func (e *Engine) LiveSeq() int64 {
	return e.ring.liveSeq()
}

func (e *Engine) WaitPacket(ctx context.Context, seq int64) (AudioPacket, int64, error) {
	return e.ring.wait(ctx, seq)
}

func (e *Engine) refreshLoop(ctx context.Context) {
	ticker := time.NewTicker(e.cfg.RefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := e.Refresh(ctx, time.Now()); err != nil {
				slog.Error("radio engine refresh failed", "error", err)
			} else {
				slog.Debug("radio engine refresh complete")
			}
		case <-e.refreshCh:
			if err := e.Refresh(ctx, time.Now()); err != nil {
				slog.Error("radio engine requested refresh failed", "error", err)
			} else {
				slog.Debug("radio engine requested refresh complete")
			}
		}
	}
}

func (e *Engine) producerLoop(ctx context.Context) {
	defer e.closeCurrentFile()
	ticker := time.NewTicker(e.chunkDuration)
	defer ticker.Stop()
	for {
		if err := e.publishTick(ctx, time.Now()); err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("radio engine publish failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (e *Engine) publishTick(ctx context.Context, now time.Time) error {
	data, err := e.readAudio(ctx, now, e.chunkFrames)
	if err != nil {
		return err
	}
	if err := e.updateNow(ctx, now); err != nil {
		return err
	}
	e.ring.publish(data)
	return nil
}

func (e *Engine) readAudio(ctx context.Context, now time.Time, frames int64) ([]byte, error) {
	out := make([]byte, 0, frames*audio.FrameSize)
	cursor := now
	remaining := frames
	for remaining > 0 {
		pos, err := e.position(ctx, cursor)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return e.appendSilence(ctx, out, remaining)
			}
			return nil, err
		}
		frameIndex := pos.elapsedMs / audio.FrameDurationMs
		if frameIndex < 0 {
			frameIndex = 0
		}
		if frameIndex >= pos.slot.FrameCount {
			cursor = time.UnixMilli(pos.slot.EndUnixMs)
			continue
		}
		available := pos.slot.FrameCount - frameIndex
		use := minInt64(remaining, available)
		path := e.cfg.SilencePath
		if pos.track != nil {
			path = pos.track.CachePath
		}
		b, err := e.readFrameRange(ctx, path, frameIndex, use)
		if err != nil {
			return nil, err
		}
		out = append(out, b...)
		remaining -= use
		cursor = cursor.Add(frameDuration(use))
	}
	return out, nil
}

func (e *Engine) updateNow(ctx context.Context, now time.Time) error {
	pos, err := e.position(ctx, now)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		nowMs := now.UnixMilli()
		out := Now{
			ServerTimeMs: nowMs,
			SlotID:       "engine-miss",
			IsSilence:    true,
			StartedAtMs:  nowMs,
			EndsAtMs:     nowMs + e.chunkDuration.Milliseconds(),
			ElapsedMs:    0,
			DurationMs:   e.chunkDuration.Milliseconds(),
		}
		e.now.Store(out)
		e.publishEvent(out)
		return nil
	}
	out := Now{
		ServerTimeMs: now.UnixMilli(),
		SlotID:       pos.slot.ID,
		IsSilence:    pos.slot.IsSilence,
		StartedAtMs:  pos.slot.StartUnixMs,
		EndsAtMs:     pos.slot.EndUnixMs,
		ElapsedMs:    pos.elapsedMs,
		DurationMs:   pos.durationMs,
	}
	if pos.track != nil {
		out.Track = &NowTrack{
			ID:        pos.track.ID,
			Title:     pos.track.Title,
			Artist:    pos.track.Artist,
			Album:     pos.track.Album,
			CoverURL:  pos.assetURLs["cover"],
			LyricsURL: pos.assetURLs["lyrics"],
		}
	}
	e.now.Store(out)
	e.publishEvent(out)
	return nil
}

func (e *Engine) position(ctx context.Context, now time.Time) (enginePosition, error) {
	pos, ok := e.cachedPosition(now)
	if ok {
		return pos, nil
	}
	e.requestRefresh()
	return enginePosition{}, sql.ErrNoRows
}

func (e *Engine) cachedPosition(now time.Time) (enginePosition, bool) {
	nowMs := now.UnixMilli()
	e.stateMu.RLock()
	defer e.stateMu.RUnlock()
	i := sort.Search(len(e.slots), func(i int) bool {
		return e.slots[i].EndUnixMs > nowMs
	})
	if i >= len(e.slots) {
		return enginePosition{}, false
	}
	slot := e.slots[i]
	if slot.StartUnixMs > nowMs || slot.EndUnixMs <= nowMs {
		return enginePosition{}, false
	}
	pos := enginePosition{
		slot:       slot,
		elapsedMs:  maxInt64(0, nowMs-slot.StartUnixMs),
		durationMs: slot.EndUnixMs - slot.StartUnixMs,
	}
	if slot.TrackID.Valid {
		if t, ok := e.tracks[slot.TrackID.String]; ok {
			track := t
			pos.track = &track
			pos.assetURLs = e.assetURLs[t.ID]
		}
	}
	if pos.assetURLs == nil {
		pos.assetURLs = map[string]string{}
	}
	return pos, true
}

func (e *Engine) publishEvent(now Now) {
	e.eventMu.Lock()
	defer e.eventMu.Unlock()
	if now.SlotID == e.lastEventSlot {
		return
	}
	e.lastEventSlot = now.SlotID
	for ch := range e.eventSubs {
		select {
		case ch <- now:
		default:
		}
	}
}

func (e *Engine) requestRefresh() {
	select {
	case e.refreshCh <- struct{}{}:
	default:
	}
}

func hasUnknownTrack(slots []store.Slot, tracks map[string]store.Track) bool {
	for _, sl := range slots {
		if sl.TrackID.Valid {
			if _, ok := tracks[sl.TrackID.String]; !ok {
				return true
			}
		}
	}
	return false
}

func (e *Engine) appendSilence(ctx context.Context, out []byte, frames int64) ([]byte, error) {
	silenceFrames := e.cfg.Scheduler.gapFrames
	if silenceFrames <= 0 {
		silenceFrames = frames
	}
	remaining := frames
	for remaining > 0 {
		use := minInt64(remaining, silenceFrames)
		b, err := e.readFrameRange(ctx, e.cfg.SilencePath, 0, use)
		if err != nil {
			return nil, err
		}
		out = append(out, b...)
		remaining -= use
	}
	return out, nil
}

func (e *Engine) readFrameRange(ctx context.Context, path string, frameIndex, frames int64) ([]byte, error) {
	f, err := e.openCurrentFile(path)
	if err != nil {
		return nil, err
	}
	if _, err := f.Seek(frameIndex*audio.FrameSize, io.SeekStart); err != nil {
		return nil, err
	}
	length := frames * audio.FrameSize
	buf := make([]byte, length)
	offset := int64(0)
	for offset < length {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		n, err := f.Read(buf[offset:])
		if n > 0 {
			offset += int64(n)
		}
		if errors.Is(err, io.EOF) && offset == length {
			return buf, nil
		}
		if err != nil {
			return nil, err
		}
	}
	return buf, nil
}

func (e *Engine) openCurrentFile(path string) (*os.File, error) {
	if e.currentFile != nil && e.currentFilePath == path {
		return e.currentFile, nil
	}
	e.closeCurrentFile()
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	e.currentFile = f
	e.currentFilePath = path
	return f, nil
}

func (e *Engine) closeCurrentFile() {
	if e.currentFile != nil {
		_ = e.currentFile.Close()
		e.currentFile = nil
		e.currentFilePath = ""
	}
}

func framesForDuration(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	fd := frameDuration(1)
	frames := int64(d / fd)
	if d%fd != 0 {
		frames++
	}
	if frames < 1 {
		frames = 1
	}
	return frames
}

func frameDuration(frames int64) time.Duration {
	return time.Duration(frames*audio.SamplesPerFrame) * time.Second / time.Duration(audio.SampleRate)
}

type audioRing struct {
	mu       sync.Mutex
	notify   chan struct{}
	packets  []AudioPacket
	nextSeq  int64
	capacity int
}

func newAudioRing(capacity int) *audioRing {
	return &audioRing{
		notify:   make(chan struct{}),
		packets:  make([]AudioPacket, capacity),
		capacity: capacity,
	}
}

func (r *audioRing) publish(data []byte) {
	r.mu.Lock()
	seq := r.nextSeq
	r.packets[int(seq%int64(r.capacity))] = AudioPacket{Seq: seq, Data: data}
	r.nextSeq++
	close(r.notify)
	r.notify = make(chan struct{})
	r.mu.Unlock()
}

func (r *audioRing) liveSeq() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.nextSeq == 0 {
		return 0
	}
	return r.nextSeq - 1
}

func (r *audioRing) wait(ctx context.Context, seq int64) (AudioPacket, int64, error) {
	for {
		r.mu.Lock()
		oldest := r.nextSeq - int64(r.capacity)
		if oldest < 0 {
			oldest = 0
		}
		if seq < oldest {
			seq = oldest
		}
		if seq < r.nextSeq {
			p := r.packets[int(seq%int64(r.capacity))]
			next := p.Seq + 1
			r.mu.Unlock()
			return p, next, nil
		}
		notify := r.notify
		r.mu.Unlock()

		select {
		case <-ctx.Done():
			return AudioPacket{}, seq, ctx.Err()
		case <-notify:
		}
	}
}

func (r *audioRing) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.nextSeq
	if n > int64(r.capacity) {
		return r.capacity
	}
	return int(n)
}
