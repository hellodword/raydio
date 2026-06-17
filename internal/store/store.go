package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const (
	TrackStatusActive  = "active"
	TrackStatusMissing = "missing"
	TrackStatusError   = "error"
)

type Store struct {
	db *sql.DB
}

type CatalogRevision struct {
	Revision  int64
	UpdatedAt string
}

type CatalogTrack struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Artist     string `json:"artist"`
	Album      string `json:"album,omitempty"`
	DurationMs int64  `json:"durationMs"`
	CoverURL   string `json:"coverUrl,omitempty"`
	LyricsURL  string `json:"lyricsUrl,omitempty"`
}

type Track struct {
	ID            string         `json:"id"`
	SourcePath    string         `json:"sourcePath"`
	SourceSize    int64          `json:"sourceSize"`
	SourceModUnix int64          `json:"sourceModUnix"`
	CachePath     string         `json:"cachePath"`
	Title         string         `json:"title"`
	Artist        string         `json:"artist"`
	Album         string         `json:"album,omitempty"`
	Comment       string         `json:"comment,omitempty"`
	DurationMs    int64          `json:"durationMs"`
	FrameCount    int64          `json:"frameCount"`
	FrameSize     int64          `json:"frameSize"`
	Bitrate       int64          `json:"bitrate"`
	SampleRate    int64          `json:"sampleRate"`
	Channels      int64          `json:"channels"`
	Status        string         `json:"status"`
	Error         sql.NullString `json:"-"`
	CreatedAt     time.Time      `json:"createdAt"`
	UpdatedAt     time.Time      `json:"updatedAt"`
}

type Asset struct {
	TrackID string `json:"trackId"`
	Kind    string `json:"kind"`
	Path    string `json:"path"`
	MIME    string `json:"mime"`
}

type Slot struct {
	ID          string         `json:"id"`
	StartUnixMs int64          `json:"startUnixMs"`
	EndUnixMs   int64          `json:"endUnixMs"`
	TrackID     sql.NullString `json:"trackId"`
	IsSilence   bool           `json:"isSilence"`
	FrameCount  int64          `json:"frameCount"`
}

func Open(ctx context.Context, path string) (*Store, error) {
	db, err := sql.Open("sqlite3", sqliteDSN(path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)

	s := &Store{db: db}
	if err := s.configure(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.Migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func sqliteDSN(path string) string {
	values := url.Values{}
	values.Set("_busy_timeout", "5000")
	values.Set("_foreign_keys", "on")
	values.Set("_journal_mode", "WAL")
	values.Set("_synchronous", "NORMAL")
	return "file:" + path + "?" + values.Encode()
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) DB() *sql.DB {
	return s.db
}

func (s *Store) configure(ctx context.Context) error {
	stmts := []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA busy_timeout=5000`,
		`PRAGMA foreign_keys=ON`,
		`PRAGMA synchronous=NORMAL`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("%s: %w", stmt, err)
		}
	}
	return nil
}

func (s *Store) Migrate(ctx context.Context) error {
	schema := []string{
		`CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY,
			applied_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS tracks (
			id TEXT PRIMARY KEY,
			source_path TEXT NOT NULL,
			source_size INTEGER NOT NULL,
			source_mod_unix INTEGER NOT NULL,
			cache_path TEXT NOT NULL,
			title TEXT NOT NULL,
			artist TEXT NOT NULL,
			album TEXT NOT NULL DEFAULT '',
			comment TEXT NOT NULL DEFAULT '',
			duration_ms INTEGER NOT NULL,
			frame_count INTEGER NOT NULL,
			frame_size INTEGER NOT NULL,
			bitrate INTEGER NOT NULL,
			sample_rate INTEGER NOT NULL,
			channels INTEGER NOT NULL,
			status TEXT NOT NULL,
			error TEXT,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_tracks_status ON tracks(status)`,
		`DROP INDEX IF EXISTS idx_tracks_source_path`,
		`CREATE INDEX IF NOT EXISTS idx_tracks_source_path_lookup ON tracks(source_path)`,
		`CREATE TABLE IF NOT EXISTS track_assets (
			track_id TEXT NOT NULL,
			kind TEXT NOT NULL,
			path TEXT NOT NULL,
			mime TEXT NOT NULL,
			updated_at TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (track_id, kind),
			FOREIGN KEY (track_id) REFERENCES tracks(id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS schedule_slots (
			id TEXT PRIMARY KEY,
			start_unix_ms INTEGER NOT NULL,
			end_unix_ms INTEGER NOT NULL,
			track_id TEXT,
			is_silence INTEGER NOT NULL,
			frame_count INTEGER NOT NULL,
			created_at TEXT NOT NULL,
			FOREIGN KEY (track_id) REFERENCES tracks(id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_schedule_slots_time ON schedule_slots(start_unix_ms, end_unix_ms)`,
		`CREATE INDEX IF NOT EXISTS idx_schedule_slots_end ON schedule_slots(end_unix_ms)`,
		`CREATE INDEX IF NOT EXISTS idx_tracks_title_nocase ON tracks(title COLLATE NOCASE, id)`,
		`CREATE INDEX IF NOT EXISTS idx_tracks_status_title_nocase ON tracks(status, title COLLATE NOCASE, id)`,
		`CREATE TABLE IF NOT EXISTS settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS catalog_state (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			revision INTEGER NOT NULL DEFAULT 0,
			updated_at TEXT NOT NULL DEFAULT ''
		)`,
		`INSERT OR IGNORE INTO catalog_state(id, revision, updated_at) VALUES (1, 0, datetime('now'))`,
		`CREATE TRIGGER IF NOT EXISTS trg_tracks_catalog_revision_insert
			AFTER INSERT ON tracks
			BEGIN
				UPDATE catalog_state SET revision = revision + 1, updated_at = datetime('now') WHERE id = 1;
			END`,
		`CREATE TRIGGER IF NOT EXISTS trg_tracks_catalog_revision_update
			AFTER UPDATE ON tracks
			BEGIN
				UPDATE catalog_state SET revision = revision + 1, updated_at = datetime('now') WHERE id = 1;
			END`,
		`CREATE TRIGGER IF NOT EXISTS trg_tracks_catalog_revision_delete
			AFTER DELETE ON tracks
			BEGIN
				UPDATE catalog_state SET revision = revision + 1, updated_at = datetime('now') WHERE id = 1;
			END`,
		`CREATE TRIGGER IF NOT EXISTS trg_track_assets_catalog_revision_insert
			AFTER INSERT ON track_assets
			BEGIN
				UPDATE catalog_state SET revision = revision + 1, updated_at = datetime('now') WHERE id = 1;
			END`,
		`CREATE TRIGGER IF NOT EXISTS trg_track_assets_catalog_revision_update
			AFTER UPDATE ON track_assets
			BEGIN
				UPDATE catalog_state SET revision = revision + 1, updated_at = datetime('now') WHERE id = 1;
			END`,
		`CREATE TRIGGER IF NOT EXISTS trg_track_assets_catalog_revision_delete
			AFTER DELETE ON track_assets
			BEGIN
				UPDATE catalog_state SET revision = revision + 1, updated_at = datetime('now') WHERE id = 1;
			END`,
		`INSERT OR IGNORE INTO schema_migrations(version, applied_at) VALUES (1, datetime('now'))`,
	}
	for _, stmt := range schema {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	if err := s.ensureColumn(ctx, "track_assets", "updated_at", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	return nil
}

func (s *Store) ensureColumn(ctx context.Context, table, column, definition string) error {
	_, err := s.db.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, column, definition))
	if err == nil {
		return nil
	}
	if strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
		return nil
	}
	return err
}

func (s *Store) UpsertTrack(ctx context.Context, t Track) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if t.Status == "" {
		t.Status = TrackStatusActive
	}
	if t.FrameSize == 0 {
		t.FrameSize = 576
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE tracks SET status=?, updated_at=? WHERE source_path=? AND id<>?`,
		TrackStatusMissing, now, t.SourcePath, t.ID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO tracks (
		id, source_path, source_size, source_mod_unix, cache_path, title, artist, album, comment,
		duration_ms, frame_count, frame_size, bitrate, sample_rate, channels, status, error,
		created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(id) DO UPDATE SET
		source_path=excluded.source_path,
		source_size=excluded.source_size,
		source_mod_unix=excluded.source_mod_unix,
		cache_path=excluded.cache_path,
		title=excluded.title,
		artist=excluded.artist,
		album=excluded.album,
		comment=excluded.comment,
		duration_ms=excluded.duration_ms,
		frame_count=excluded.frame_count,
		frame_size=excluded.frame_size,
		bitrate=excluded.bitrate,
		sample_rate=excluded.sample_rate,
		channels=excluded.channels,
		status=excluded.status,
		error=excluded.error,
		updated_at=excluded.updated_at`,
		t.ID, t.SourcePath, t.SourceSize, t.SourceModUnix, t.CachePath, t.Title, t.Artist, t.Album, t.Comment,
		t.DurationMs, t.FrameCount, t.FrameSize, t.Bitrate, t.SampleRate, t.Channels, t.Status, t.Error,
		now, now)
	return err
}

func (s *Store) UpsertAsset(ctx context.Context, a Asset) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO track_assets(track_id, kind, path, mime, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(track_id, kind) DO UPDATE SET path=excluded.path, mime=excluded.mime, updated_at=excluded.updated_at`,
		a.TrackID, a.Kind, a.Path, a.MIME, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) DeleteAsset(ctx context.Context, trackID, kind string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM track_assets WHERE track_id=? AND kind=?`, trackID, kind)
	return err
}

func (s *Store) AssetsByTrack(ctx context.Context, trackID string) (map[string]Asset, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT track_id, kind, path, mime FROM track_assets WHERE track_id=?`, trackID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]Asset{}
	for rows.Next() {
		var a Asset
		if err := rows.Scan(&a.TrackID, &a.Kind, &a.Path, &a.MIME); err != nil {
			return nil, err
		}
		out[a.Kind] = a
	}
	return out, rows.Err()
}

func (s *Store) Asset(ctx context.Context, trackID, kind string) (Asset, error) {
	var a Asset
	err := s.db.QueryRowContext(ctx, `SELECT track_id, kind, path, mime FROM track_assets WHERE track_id=? AND kind=?`, trackID, kind).
		Scan(&a.TrackID, &a.Kind, &a.Path, &a.MIME)
	if err != nil {
		return Asset{}, err
	}
	return a, nil
}

func (s *Store) ListAssets(ctx context.Context) ([]Asset, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT track_id, kind, path, mime FROM track_assets ORDER BY track_id, kind`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Asset
	for rows.Next() {
		var a Asset
		if err := rows.Scan(&a.TrackID, &a.Kind, &a.Path, &a.MIME); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) CatalogRevision(ctx context.Context) (CatalogRevision, error) {
	var rev CatalogRevision
	err := s.db.QueryRowContext(ctx, `SELECT revision, updated_at FROM catalog_state WHERE id=1`).
		Scan(&rev.Revision, &rev.UpdatedAt)
	if err != nil {
		return CatalogRevision{}, err
	}
	return rev, nil
}

func (s *Store) SetTrackStatus(ctx context.Context, id, status string, errText sql.NullString) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tracks SET status=?, error=?, updated_at=? WHERE id=?`,
		status, errText, time.Now().UTC().Format(time.RFC3339Nano), id)
	return err
}

func (s *Store) MarkMissingExcept(ctx context.Context, seen map[string]struct{}) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, source_path FROM tracks WHERE status=?`, TrackStatusActive)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type row struct {
		id, path string
	}
	var missing []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.path); err != nil {
			return nil, err
		}
		if _, ok := seen[r.path]; !ok {
			missing = append(missing, r)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(missing))
	for _, r := range missing {
		if err := s.SetTrackStatus(ctx, r.id, TrackStatusMissing, sql.NullString{}); err != nil {
			return nil, err
		}
		ids = append(ids, r.id)
	}
	return ids, nil
}

func (s *Store) ListActiveTracks(ctx context.Context) ([]Track, error) {
	return s.listTracks(ctx, `WHERE status='active' ORDER BY title COLLATE NOCASE, id`)
}

func (s *Store) ListTracks(ctx context.Context) ([]Track, error) {
	return s.listTracks(ctx, `ORDER BY title COLLATE NOCASE, id`)
}

func (s *Store) ListCatalogPage(ctx context.Context, afterTitle, afterID string, limit int) ([]CatalogTrack, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT
			t.id, t.title, t.artist, t.album, t.duration_ms,
			EXISTS(SELECT 1 FROM track_assets a WHERE a.track_id=t.id AND a.kind='cover'),
			EXISTS(SELECT 1 FROM track_assets a WHERE a.track_id=t.id AND a.kind='lyrics')
		FROM tracks t
		WHERE t.status=?
			AND ((?='' AND ?='') OR t.title COLLATE NOCASE > ? COLLATE NOCASE
				OR (t.title COLLATE NOCASE = ? COLLATE NOCASE AND t.id > ?))
		ORDER BY t.title COLLATE NOCASE, t.id
		LIMIT ?`,
		TrackStatusActive, afterTitle, afterID, afterTitle, afterTitle, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CatalogTrack
	for rows.Next() {
		var t CatalogTrack
		var hasCover, hasLyrics int
		if err := rows.Scan(&t.ID, &t.Title, &t.Artist, &t.Album, &t.DurationMs, &hasCover, &hasLyrics); err != nil {
			return nil, err
		}
		if hasCover != 0 {
			t.CoverURL = "/covers/" + t.ID
		}
		if hasLyrics != 0 {
			t.LyricsURL = "/lyrics/" + t.ID
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) TracksByID(ctx context.Context, ids []string) (map[string]Track, error) {
	unique := uniqueStrings(ids)
	out := make(map[string]Track, len(unique))
	if len(unique) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(unique))
	for _, id := range unique {
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, selectTracksSQL()+` WHERE id IN (`+QuotePlaceholders(len(unique))+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		t, err := scanTrack(rows)
		if err != nil {
			return nil, err
		}
		out[t.ID] = t
	}
	return out, rows.Err()
}

func (s *Store) AssetsByTrackIDs(ctx context.Context, ids []string) (map[string]map[string]Asset, error) {
	unique := uniqueStrings(ids)
	out := make(map[string]map[string]Asset, len(unique))
	if len(unique) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(unique))
	for _, id := range unique {
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT track_id, kind, path, mime
		FROM track_assets WHERE track_id IN (`+QuotePlaceholders(len(unique))+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a Asset
		if err := rows.Scan(&a.TrackID, &a.Kind, &a.Path, &a.MIME); err != nil {
			return nil, err
		}
		if out[a.TrackID] == nil {
			out[a.TrackID] = map[string]Asset{}
		}
		out[a.TrackID][a.Kind] = a
	}
	return out, rows.Err()
}

func (s *Store) Track(ctx context.Context, id string) (Track, error) {
	rows, err := s.db.QueryContext(ctx, selectTracksSQL()+` WHERE id=?`, id)
	if err != nil {
		return Track{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		return Track{}, sql.ErrNoRows
	}
	t, err := scanTrack(rows)
	if err != nil {
		return Track{}, err
	}
	return t, rows.Err()
}

func (s *Store) listTracks(ctx context.Context, suffix string) ([]Track, error) {
	rows, err := s.db.QueryContext(ctx, selectTracksSQL()+` `+suffix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Track
	for rows.Next() {
		t, err := scanTrack(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func selectTracksSQL() string {
	return `SELECT id, source_path, source_size, source_mod_unix, cache_path, title, artist, album, comment,
		duration_ms, frame_count, frame_size, bitrate, sample_rate, channels, status, error, created_at, updated_at FROM tracks`
}

func scanTrack(rows interface {
	Scan(dest ...any) error
}) (Track, error) {
	var t Track
	var created, updated string
	if err := rows.Scan(&t.ID, &t.SourcePath, &t.SourceSize, &t.SourceModUnix, &t.CachePath, &t.Title, &t.Artist, &t.Album, &t.Comment,
		&t.DurationMs, &t.FrameCount, &t.FrameSize, &t.Bitrate, &t.SampleRate, &t.Channels, &t.Status, &t.Error, &created, &updated); err != nil {
		return Track{}, err
	}
	t.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	t.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return t, nil
}

func (s *Store) DeleteSlotsBefore(ctx context.Context, cutoffMs int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM schedule_slots WHERE end_unix_ms < ?`, cutoffMs)
	return err
}

func (s *Store) DeleteFutureSlots(ctx context.Context, cutoffMs int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM schedule_slots WHERE start_unix_ms > ?`, cutoffMs)
	return err
}

func (s *Store) UpsertSlot(ctx context.Context, sl Slot) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO schedule_slots(id, start_unix_ms, end_unix_ms, track_id, is_silence, frame_count, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			start_unix_ms=excluded.start_unix_ms,
			end_unix_ms=excluded.end_unix_ms,
			track_id=excluded.track_id,
			is_silence=excluded.is_silence,
			frame_count=excluded.frame_count`,
		sl.ID, sl.StartUnixMs, sl.EndUnixMs, nullableString(sl.TrackID), boolInt(sl.IsSilence), sl.FrameCount, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) UpsertSlots(ctx context.Context, slots []Slot) error {
	if len(slots) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO schedule_slots(id, start_unix_ms, end_unix_ms, track_id, is_silence, frame_count, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			start_unix_ms=excluded.start_unix_ms,
			end_unix_ms=excluded.end_unix_ms,
			track_id=excluded.track_id,
			is_silence=excluded.is_silence,
			frame_count=excluded.frame_count`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer stmt.Close()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, sl := range slots {
		if _, err := stmt.ExecContext(ctx, sl.ID, sl.StartUnixMs, sl.EndUnixMs, nullableString(sl.TrackID), boolInt(sl.IsSilence), sl.FrameCount, now); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) SlotsEndingAfter(ctx context.Context, unixMs int64) ([]Slot, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, start_unix_ms, end_unix_ms, track_id, is_silence, frame_count
		FROM schedule_slots WHERE end_unix_ms > ? ORDER BY end_unix_ms ASC`, unixMs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Slot
	for rows.Next() {
		var sl Slot
		var isSilence int
		if err := rows.Scan(&sl.ID, &sl.StartUnixMs, &sl.EndUnixMs, &sl.TrackID, &isSilence, &sl.FrameCount); err != nil {
			return nil, err
		}
		sl.IsSilence = isSilence != 0
		out = append(out, sl)
	}
	return out, rows.Err()
}

func (s *Store) SlotAt(ctx context.Context, unixMs int64) (Slot, error) {
	var sl Slot
	var isSilence int
	err := s.db.QueryRowContext(ctx, `SELECT id, start_unix_ms, end_unix_ms, track_id, is_silence, frame_count
		FROM schedule_slots WHERE start_unix_ms <= ? AND end_unix_ms > ? ORDER BY start_unix_ms DESC LIMIT 1`, unixMs, unixMs).
		Scan(&sl.ID, &sl.StartUnixMs, &sl.EndUnixMs, &sl.TrackID, &isSilence, &sl.FrameCount)
	if err != nil {
		return Slot{}, err
	}
	sl.IsSilence = isSilence != 0
	return sl, nil
}

func (s *Store) LastSlot(ctx context.Context) (Slot, error) {
	var sl Slot
	var isSilence int
	err := s.db.QueryRowContext(ctx, `SELECT id, start_unix_ms, end_unix_ms, track_id, is_silence, frame_count
		FROM schedule_slots ORDER BY end_unix_ms DESC LIMIT 1`).
		Scan(&sl.ID, &sl.StartUnixMs, &sl.EndUnixMs, &sl.TrackID, &isSilence, &sl.FrameCount)
	if err != nil {
		return Slot{}, err
	}
	sl.IsSilence = isSilence != 0
	return sl, nil
}

func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings(key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		key, value, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) Setting(ctx context.Context, key string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return value, err
}

func nullableString(ns sql.NullString) any {
	if !ns.Valid {
		return nil
	}
	return ns.String
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func QuotePlaceholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimRight(strings.Repeat("?,", n), ",")
}

func uniqueStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
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
