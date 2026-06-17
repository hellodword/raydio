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
	StationUUID        string
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
	assets    map[string]map[string]store.Asset

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
	fileMu          sync.Mutex
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

var emptyAssetURLs = map[string]string{}

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
	if cfg.StationUUID == "" {
		cfg.StationUUID = cfg.Scheduler.stationUUID
	}
	if cfg.StationUUID == "" {
		return nil, errors.New("station uuid is required")
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
		assets:        map[string]map[string]store.Asset{},
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
	slots, err := e.cfg.Store.SlotsEndingAfter(ctx, e.cfg.StationUUID, now.Add(-e.cfg.StreamBufferWindow).UnixMilli())
	if err != nil {
		return err
	}
	rev, err := e.cfg.Store.CatalogRevision(ctx, e.cfg.StationUUID)
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

	ids := trackIDsFromSlots(slots)
	tracks, err := e.cfg.Store.TracksByID(ctx, ids)
	if err != nil {
		return err
	}
	allAssets, err := e.cfg.Store.ListAssets(ctx, e.cfg.StationUUID)
	if err != nil {
		return err
	}
	assets := assetsByTrack(allAssets)
	urls := assetURLsByTrack(e.cfg.StationUUID, assets)

	e.stateMu.Lock()
	e.slots = slots
	e.tracks = tracks
	e.assetURLs = urls
	e.assets = assets
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

func (e *Engine) Asset(trackID, kind string) (store.Asset, bool) {
	e.stateMu.RLock()
	defer e.stateMu.RUnlock()
	if byKind := e.assets[trackID]; byKind != nil {
		a, ok := byKind[kind]
		return a, ok
	}
	return store.Asset{}, false
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

func (e *Engine) RequestRefresh() {
	e.requestRefresh()
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

	at := time.Now()
	if err := e.publishTick(ctx, at); err != nil {
		if ctx.Err() != nil {
			return
		}
		slog.Error("radio engine publish failed", "error", err)
	}
	readReq, readCh := e.startAudioReader(ctx)
	if !queueAudioRead(ctx, readReq, at.Add(e.chunkDuration)) {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		var result audioReadResult
		select {
		case <-ctx.Done():
			return
		case result = <-readCh:
		}
		if result.err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("radio engine publish failed", "error", result.err)
		} else if err := e.publishAudio(ctx, result.at, result.data); err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("radio engine publish failed", "error", err)
		}
		if !queueAudioRead(ctx, readReq, result.at.Add(e.chunkDuration)) {
			return
		}
	}
}

type audioReadResult struct {
	at   time.Time
	data []byte
	err  error
}

func (e *Engine) startAudioReader(ctx context.Context) (chan<- time.Time, <-chan audioReadResult) {
	reqCh := make(chan time.Time, 1)
	resCh := make(chan audioReadResult, 1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case at := <-reqCh:
				data, err := e.readAudio(ctx, at, e.chunkFrames)
				select {
				case <-ctx.Done():
					return
				case resCh <- audioReadResult{at: at, data: data, err: err}:
				}
			}
		}
	}()
	return reqCh, resCh
}

func queueAudioRead(ctx context.Context, ch chan<- time.Time, at time.Time) bool {
	select {
	case <-ctx.Done():
		return false
	case ch <- at:
		return true
	}
}

func (e *Engine) publishTick(ctx context.Context, now time.Time) error {
	data, err := e.readAudio(ctx, now, e.chunkFrames)
	if err != nil {
		return err
	}
	return e.publishAudio(ctx, now, data)
}

func (e *Engine) publishAudio(ctx context.Context, now time.Time, data []byte) error {
	if err := e.updateNow(ctx, now); err != nil {
		return err
	}
	e.ring.publish(data)
	return nil
}

func (e *Engine) readAudio(ctx context.Context, now time.Time, frames int64) ([]byte, error) {
	out := make([]byte, frames*audio.FrameSize)
	cursor := now
	remaining := frames
	written := int64(0)
	for remaining > 0 {
		pos, err := e.position(ctx, cursor)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return e.appendSilence(ctx, out, written, remaining)
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
		start := written * audio.FrameSize
		end := start + use*audio.FrameSize
		if err := e.readFrameRange(ctx, path, frameIndex, use, out[start:end]); err != nil {
			return nil, err
		}
		written += use
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
		pos.assetURLs = emptyAssetURLs
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

func assetsByTrack(assets []store.Asset) map[string]map[string]store.Asset {
	out := make(map[string]map[string]store.Asset, len(assets))
	for _, a := range assets {
		if out[a.TrackID] == nil {
			out[a.TrackID] = map[string]store.Asset{}
		}
		out[a.TrackID][a.Kind] = a
	}
	return out
}

func assetURLsByTrack(stationUUID string, assets map[string]map[string]store.Asset) map[string]map[string]string {
	out := make(map[string]map[string]string, len(assets))
	for trackID, byKind := range assets {
		for _, a := range byKind {
			if out[trackID] == nil {
				out[trackID] = map[string]string{}
			}
			switch a.Kind {
			case "cover":
				out[trackID]["cover"] = "/radio/" + stationUUID + "/covers/" + trackID
			case "lyrics":
				out[trackID]["lyrics"] = "/radio/" + stationUUID + "/lyrics/" + trackID
			}
		}
	}
	return out
}

func (e *Engine) appendSilence(ctx context.Context, out []byte, written, frames int64) ([]byte, error) {
	silenceFrames := e.cfg.Scheduler.gapFrames
	if silenceFrames <= 0 {
		silenceFrames = frames
	}
	remaining := frames
	for remaining > 0 {
		use := minInt64(remaining, silenceFrames)
		start := written * audio.FrameSize
		end := start + use*audio.FrameSize
		if err := e.readFrameRange(ctx, e.cfg.SilencePath, 0, use, out[start:end]); err != nil {
			return nil, err
		}
		written += use
		remaining -= use
	}
	return out, nil
}

func (e *Engine) readFrameRange(ctx context.Context, path string, frameIndex, frames int64, dst []byte) error {
	e.fileMu.Lock()
	defer e.fileMu.Unlock()
	f, err := e.openCurrentFile(path)
	if err != nil {
		return err
	}
	if _, err := f.Seek(frameIndex*audio.FrameSize, io.SeekStart); err != nil {
		return err
	}
	length := frames * audio.FrameSize
	offset := int64(0)
	for offset < length {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		n, err := f.Read(dst[offset:])
		if n > 0 {
			offset += int64(n)
		}
		if errors.Is(err, io.EOF) && offset == length {
			return nil
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) openCurrentFile(path string) (*os.File, error) {
	if e.currentFile != nil && e.currentFilePath == path {
		return e.currentFile, nil
	}
	e.closeCurrentFileLocked()
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	e.currentFile = f
	e.currentFilePath = path
	return f, nil
}

func (e *Engine) closeCurrentFile() {
	e.fileMu.Lock()
	defer e.fileMu.Unlock()
	e.closeCurrentFileLocked()
}

func (e *Engine) closeCurrentFileLocked() {
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
	packets  []atomic.Value
	nextSeq  atomic.Int64
	capacity int
}

func newAudioRing(capacity int) *audioRing {
	return &audioRing{
		notify:   make(chan struct{}),
		packets:  make([]atomic.Value, capacity),
		capacity: capacity,
	}
}

func (r *audioRing) publish(data []byte) {
	seq := r.nextSeq.Load()
	r.packets[int(seq%int64(r.capacity))].Store(AudioPacket{Seq: seq, Data: data})
	r.mu.Lock()
	r.nextSeq.Store(seq + 1)
	close(r.notify)
	r.notify = make(chan struct{})
	r.mu.Unlock()
}

func (r *audioRing) liveSeq() int64 {
	next := r.nextSeq.Load()
	if next == 0 {
		return 0
	}
	return next - 1
}

func (r *audioRing) wait(ctx context.Context, seq int64) (AudioPacket, int64, error) {
	for {
		nextSeq := r.nextSeq.Load()
		if p, ok := r.packet(seq, nextSeq); ok {
			next := p.Seq + 1
			return p, next, nil
		}
		seq = clampSeq(seq, nextSeq, r.capacity)

		r.mu.Lock()
		nextSeq = r.nextSeq.Load()
		seq = clampSeq(seq, nextSeq, r.capacity)
		if _, ok := r.packet(seq, nextSeq); ok {
			r.mu.Unlock()
			continue
		}
		if seq < nextSeq {
			r.mu.Unlock()
			continue
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
	n := r.nextSeq.Load()
	if n > int64(r.capacity) {
		return r.capacity
	}
	return int(n)
}

func (r *audioRing) packet(seq, nextSeq int64) (AudioPacket, bool) {
	seq = clampSeq(seq, nextSeq, r.capacity)
	if seq >= nextSeq {
		return AudioPacket{}, false
	}
	v := r.packets[int(seq%int64(r.capacity))].Load()
	if v == nil {
		return AudioPacket{}, false
	}
	p := v.(AudioPacket)
	if p.Seq != seq {
		return AudioPacket{}, false
	}
	return p, true
}

func clampSeq(seq, nextSeq int64, capacity int) int64 {
	oldest := nextSeq - int64(capacity)
	if oldest < 0 {
		oldest = 0
	}
	if seq < oldest {
		return oldest
	}
	return seq
}

func trackIDsFromSlots(slots []store.Slot) []string {
	ids := make([]string, 0, len(slots))
	for _, slot := range slots {
		if slot.TrackID.Valid {
			ids = append(ids, slot.TrackID.String)
		}
	}
	return ids
}
