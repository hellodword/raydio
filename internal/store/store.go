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

const currentSchemaSQL = `
CREATE TABLE stations (
	uuid TEXT PRIMARY KEY,
	alias TEXT NOT NULL UNIQUE,
	enabled INTEGER NOT NULL,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

CREATE TABLE tracks (
	id TEXT PRIMARY KEY,
	station_uuid TEXT NOT NULL,
	content_hash TEXT NOT NULL,
	source_path TEXT NOT NULL,
	source_size INTEGER NOT NULL,
	source_mod_unix INTEGER NOT NULL,
	cache_path TEXT NOT NULL,
	title TEXT NOT NULL,
	artist TEXT NOT NULL,
	album TEXT NOT NULL DEFAULT '',
	duration_ms INTEGER NOT NULL,
	frame_count INTEGER NOT NULL,
	frame_size INTEGER NOT NULL,
	bitrate INTEGER NOT NULL,
	sample_rate INTEGER NOT NULL,
	channels INTEGER NOT NULL,
	status TEXT NOT NULL,
	error TEXT,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	FOREIGN KEY (station_uuid) REFERENCES stations(uuid)
);

CREATE INDEX idx_tracks_source_path_lookup ON tracks(station_uuid, source_path);
CREATE INDEX idx_tracks_title_nocase ON tracks(title COLLATE NOCASE, id);
CREATE INDEX idx_tracks_status_title_nocase ON tracks(station_uuid, status, title COLLATE NOCASE, id);

CREATE TABLE track_assets (
	track_id TEXT NOT NULL,
	kind TEXT NOT NULL CHECK (kind = 'cover'),
	path TEXT NOT NULL,
	mime TEXT NOT NULL,
	updated_at TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (track_id, kind),
	FOREIGN KEY (track_id) REFERENCES tracks(id) ON DELETE CASCADE
);

CREATE TABLE schedule_slots (
	id TEXT PRIMARY KEY,
	station_uuid TEXT NOT NULL,
	start_unix_ms INTEGER NOT NULL,
	end_unix_ms INTEGER NOT NULL,
	track_id TEXT,
	is_silence INTEGER NOT NULL,
	frame_count INTEGER NOT NULL,
	created_at TEXT NOT NULL,
	FOREIGN KEY (station_uuid) REFERENCES stations(uuid),
	FOREIGN KEY (track_id) REFERENCES tracks(id)
);

CREATE INDEX idx_schedule_slots_time ON schedule_slots(station_uuid, start_unix_ms, end_unix_ms);
CREATE INDEX idx_schedule_slots_end ON schedule_slots(station_uuid, end_unix_ms);

CREATE TABLE catalog_state (
	station_uuid TEXT PRIMARY KEY,
	revision INTEGER NOT NULL DEFAULT 0,
	updated_at TEXT NOT NULL DEFAULT '',
	FOREIGN KEY (station_uuid) REFERENCES stations(uuid)
);

CREATE TRIGGER trg_tracks_catalog_revision_insert
	AFTER INSERT ON tracks
	BEGIN
		UPDATE catalog_state SET revision = revision + 1, updated_at = datetime('now') WHERE station_uuid = NEW.station_uuid;
	END;

CREATE TRIGGER trg_tracks_catalog_revision_update
	AFTER UPDATE ON tracks
	BEGIN
		UPDATE catalog_state SET revision = revision + 1, updated_at = datetime('now') WHERE station_uuid = NEW.station_uuid;
	END;

CREATE TRIGGER trg_tracks_catalog_revision_delete
	AFTER DELETE ON tracks
	BEGIN
		UPDATE catalog_state SET revision = revision + 1, updated_at = datetime('now') WHERE station_uuid = OLD.station_uuid;
	END;

CREATE TRIGGER trg_track_assets_catalog_revision_insert
	AFTER INSERT ON track_assets
	BEGIN
		UPDATE catalog_state SET revision = revision + 1, updated_at = datetime('now')
		WHERE station_uuid = (SELECT station_uuid FROM tracks WHERE id = NEW.track_id);
	END;

CREATE TRIGGER trg_track_assets_catalog_revision_update
	AFTER UPDATE ON track_assets
	BEGIN
		UPDATE catalog_state SET revision = revision + 1, updated_at = datetime('now')
		WHERE station_uuid = (SELECT station_uuid FROM tracks WHERE id = NEW.track_id);
	END;

CREATE TRIGGER trg_track_assets_catalog_revision_delete
	AFTER DELETE ON track_assets
	BEGIN
		UPDATE catalog_state SET revision = revision + 1, updated_at = datetime('now')
		WHERE station_uuid = (SELECT station_uuid FROM tracks WHERE id = OLD.track_id);
	END;
`

type Store struct {
	db *sql.DB
}

type CatalogRevision struct {
	Revision  int64
	UpdatedAt string
}

type Station struct {
	UUID      string    `json:"uuid"`
	Alias     string    `json:"alias"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type CatalogTrack struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Artist     string `json:"artist"`
	Album      string `json:"album,omitempty"`
	DurationMs int64  `json:"durationMs"`
	CoverURL   string `json:"coverUrl,omitempty"`
}

type Track struct {
	ID            string         `json:"id"`
	StationUUID   string         `json:"stationUuid"`
	ContentHash   string         `json:"contentHash"`
	SourcePath    string         `json:"sourcePath"`
	SourceSize    int64          `json:"sourceSize"`
	SourceModUnix int64          `json:"sourceModUnix"`
	CachePath     string         `json:"cachePath"`
	Title         string         `json:"title"`
	Artist        string         `json:"artist"`
	Album         string         `json:"album,omitempty"`
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
	StationUUID string         `json:"stationUuid"`
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
	hasGoose, err := s.hasTable(ctx, "goose_db_version")
	if err != nil {
		return err
	}
	if hasGoose {
		return errors.New("database uses legacy goose migrations; remove or recreate the SQLite database before starting this version")
	}
	tables, err := s.userTables(ctx)
	if err != nil {
		return err
	}
	if len(tables) == 0 {
		if _, err := s.db.ExecContext(ctx, currentSchemaSQL); err != nil {
			return err
		}
		return s.validateCurrentSchema(ctx)
	}
	if err := validateKnownTables(tables); err != nil {
		return err
	}
	return s.validateCurrentSchema(ctx)
}

func (s *Store) hasTable(ctx context.Context, name string) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&count)
	return count != 0, err
}

func (s *Store) userTables(ctx context.Context) (map[string]struct{}, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]struct{}{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		if strings.HasPrefix(name, "sqlite_") {
			continue
		}
		out[name] = struct{}{}
	}
	return out, rows.Err()
}

var currentTables = map[string]struct{}{
	"stations":       {},
	"tracks":         {},
	"track_assets":   {},
	"schedule_slots": {},
	"catalog_state":  {},
}

var currentIndexes = []string{
	"idx_tracks_source_path_lookup",
	"idx_tracks_title_nocase",
	"idx_tracks_status_title_nocase",
	"idx_schedule_slots_time",
	"idx_schedule_slots_end",
}

var currentTriggers = []string{
	"trg_tracks_catalog_revision_insert",
	"trg_tracks_catalog_revision_update",
	"trg_tracks_catalog_revision_delete",
	"trg_track_assets_catalog_revision_insert",
	"trg_track_assets_catalog_revision_update",
	"trg_track_assets_catalog_revision_delete",
}

func validateKnownTables(tables map[string]struct{}) error {
	if len(tables) != len(currentTables) {
		return errors.New("database has an unsupported existing schema; remove or recreate the SQLite database before starting this version")
	}
	for name := range tables {
		if _, ok := currentTables[name]; !ok {
			return errors.New("database has an unsupported existing schema; remove or recreate the SQLite database before starting this version")
		}
	}
	return nil
}

func (s *Store) validateCurrentSchema(ctx context.Context) error {
	for name := range currentTables {
		ok, err := s.hasTable(ctx, name)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("database schema missing table %s; remove or recreate the SQLite database before starting this version", name)
		}
	}
	for _, name := range currentIndexes {
		ok, err := s.hasObject(ctx, "index", name)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("database schema missing index %s; remove or recreate the SQLite database before starting this version", name)
		}
	}
	for _, name := range currentTriggers {
		ok, err := s.hasObject(ctx, "trigger", name)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("database schema missing trigger %s; remove or recreate the SQLite database before starting this version", name)
		}
	}
	return nil
}

func (s *Store) hasObject(ctx context.Context, typ, name string) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type=? AND name=?`, typ, name).Scan(&count)
	return count != 0, err
}

func (s *Store) UpsertStation(ctx context.Context, st Station) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx, `INSERT INTO stations(uuid, alias, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(uuid) DO UPDATE SET alias=excluded.alias, enabled=excluded.enabled, updated_at=excluded.updated_at
		WHERE stations.alias IS NOT excluded.alias OR stations.enabled IS NOT excluded.enabled`,
		st.UUID, st.Alias, boolInt(st.Enabled), now, now)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT OR IGNORE INTO catalog_state(station_uuid, revision, updated_at)
		VALUES (?, 0, datetime('now'))`, st.UUID)
	return err
}

func (s *Store) ListStations(ctx context.Context) ([]Station, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT uuid, alias, enabled, created_at, updated_at
		FROM stations ORDER BY alias COLLATE NOCASE, uuid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Station
	for rows.Next() {
		var st Station
		var enabled int
		var created, updated string
		if err := rows.Scan(&st.UUID, &st.Alias, &enabled, &created, &updated); err != nil {
			return nil, err
		}
		st.Enabled = enabled != 0
		st.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		st.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		out = append(out, st)
	}
	return out, rows.Err()
}

func (s *Store) UpsertTrack(ctx context.Context, t Track) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if t.Status == "" {
		t.Status = TrackStatusActive
	}
	if t.FrameSize == 0 {
		t.FrameSize = 576
	}
	if t.ContentHash == "" {
		t.ContentHash = t.ID
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE tracks SET status=?, updated_at=?
		WHERE station_uuid=? AND source_path=? AND id<>? AND status<>?`,
		TrackStatusMissing, now, t.StationUUID, t.SourcePath, t.ID, TrackStatusMissing); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO tracks (
		id, station_uuid, content_hash, source_path, source_size, source_mod_unix, cache_path, title, artist, album,
		duration_ms, frame_count, frame_size, bitrate, sample_rate, channels, status, error,
		created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(id) DO UPDATE SET
		station_uuid=excluded.station_uuid,
		content_hash=excluded.content_hash,
		source_path=excluded.source_path,
		source_size=excluded.source_size,
		source_mod_unix=excluded.source_mod_unix,
		cache_path=excluded.cache_path,
		title=excluded.title,
		artist=excluded.artist,
		album=excluded.album,
		duration_ms=excluded.duration_ms,
		frame_count=excluded.frame_count,
		frame_size=excluded.frame_size,
		bitrate=excluded.bitrate,
		sample_rate=excluded.sample_rate,
		channels=excluded.channels,
		status=excluded.status,
		error=excluded.error,
		updated_at=excluded.updated_at
	WHERE tracks.station_uuid IS NOT excluded.station_uuid
		OR tracks.content_hash IS NOT excluded.content_hash
		OR tracks.source_path IS NOT excluded.source_path
		OR tracks.source_size IS NOT excluded.source_size
		OR tracks.source_mod_unix IS NOT excluded.source_mod_unix
		OR tracks.cache_path IS NOT excluded.cache_path
		OR tracks.title IS NOT excluded.title
		OR tracks.artist IS NOT excluded.artist
		OR tracks.album IS NOT excluded.album
		OR tracks.duration_ms IS NOT excluded.duration_ms
		OR tracks.frame_count IS NOT excluded.frame_count
		OR tracks.frame_size IS NOT excluded.frame_size
		OR tracks.bitrate IS NOT excluded.bitrate
		OR tracks.sample_rate IS NOT excluded.sample_rate
		OR tracks.channels IS NOT excluded.channels
		OR tracks.status IS NOT excluded.status
		OR tracks.error IS NOT excluded.error`,
		t.ID, t.StationUUID, t.ContentHash, t.SourcePath, t.SourceSize, t.SourceModUnix, t.CachePath, t.Title, t.Artist, t.Album,
		t.DurationMs, t.FrameCount, t.FrameSize, t.Bitrate, t.SampleRate, t.Channels, t.Status, t.Error,
		now, now)
	return err
}

func (s *Store) UpsertAsset(ctx context.Context, a Asset) error {
	if a.Kind != "cover" {
		return fmt.Errorf("unsupported asset kind %q", a.Kind)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO track_assets(track_id, kind, path, mime, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(track_id, kind) DO UPDATE SET path=excluded.path, mime=excluded.mime, updated_at=excluded.updated_at
		WHERE track_assets.path IS NOT excluded.path OR track_assets.mime IS NOT excluded.mime`,
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

func (s *Store) Asset(ctx context.Context, stationUUID, trackID, kind string) (Asset, error) {
	var a Asset
	err := s.db.QueryRowContext(ctx, `SELECT a.track_id, a.kind, a.path, a.mime
		FROM track_assets a
		JOIN tracks t ON t.id=a.track_id
		WHERE t.station_uuid=? AND a.track_id=? AND a.kind=?`, stationUUID, trackID, kind).
		Scan(&a.TrackID, &a.Kind, &a.Path, &a.MIME)
	if err != nil {
		return Asset{}, err
	}
	return a, nil
}

func (s *Store) CatalogRevision(ctx context.Context, stationUUID string) (CatalogRevision, error) {
	var rev CatalogRevision
	err := s.db.QueryRowContext(ctx, `SELECT revision, updated_at FROM catalog_state WHERE station_uuid=?`, stationUUID).
		Scan(&rev.Revision, &rev.UpdatedAt)
	if err != nil {
		return CatalogRevision{}, err
	}
	return rev, nil
}

func (s *Store) SetTrackStatus(ctx context.Context, id, status string, errText sql.NullString) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tracks SET status=?, error=?, updated_at=? WHERE id=? AND (status IS NOT ? OR error IS NOT ?)`,
		status, errText, time.Now().UTC().Format(time.RFC3339Nano), id, status, errText)
	return err
}

func (s *Store) MarkMissingExcept(ctx context.Context, stationUUID string, seen map[string]struct{}) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, source_path FROM tracks WHERE station_uuid=? AND status=?`, stationUUID, TrackStatusActive)
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

func (s *Store) ListActiveTracks(ctx context.Context, stationUUID string) ([]Track, error) {
	return s.listTracks(ctx, `WHERE station_uuid=? AND status='active' ORDER BY title COLLATE NOCASE, id`, stationUUID)
}

func (s *Store) ListTracks(ctx context.Context) ([]Track, error) {
	return s.listTracks(ctx, `ORDER BY station_uuid, title COLLATE NOCASE, id`)
}

func (s *Store) ListCatalogPage(ctx context.Context, stationUUID, afterTitle, afterID string, limit int) ([]CatalogTrack, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `WITH page AS (
			SELECT t.id, t.title, t.artist, t.album, t.duration_ms
			FROM tracks t
			WHERE t.station_uuid=?
				AND t.status=?
				AND ((?='' AND ?='') OR t.title COLLATE NOCASE > ? COLLATE NOCASE
					OR (t.title COLLATE NOCASE = ? COLLATE NOCASE AND t.id > ?))
			ORDER BY t.title COLLATE NOCASE, t.id
			LIMIT ?
		)
		SELECT
			p.id, p.title, p.artist, p.album, p.duration_ms,
			MAX(CASE WHEN a.kind='cover' THEN 1 ELSE 0 END)
		FROM page p
		LEFT JOIN track_assets a ON a.track_id=p.id AND a.kind='cover'
		GROUP BY p.id, p.title, p.artist, p.album, p.duration_ms
		ORDER BY p.title COLLATE NOCASE, p.id`,
		stationUUID, TrackStatusActive, afterTitle, afterID, afterTitle, afterTitle, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CatalogTrack
	for rows.Next() {
		var t CatalogTrack
		var hasCover int
		if err := rows.Scan(&t.ID, &t.Title, &t.Artist, &t.Album, &t.DurationMs, &hasCover); err != nil {
			return nil, err
		}
		if hasCover != 0 {
			t.CoverURL = "/radio/" + stationUUID + "/covers/" + t.ID
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

func (s *Store) TrackBySource(ctx context.Context, stationUUID, sourcePath string) (Track, error) {
	rows, err := s.db.QueryContext(ctx, selectTracksSQL()+` WHERE station_uuid=? AND source_path=? AND status<>? ORDER BY updated_at DESC LIMIT 1`,
		stationUUID, sourcePath, TrackStatusMissing)
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

func (s *Store) listTracks(ctx context.Context, suffix string, args ...any) ([]Track, error) {
	rows, err := s.db.QueryContext(ctx, selectTracksSQL()+` `+suffix, args...)
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
	return `SELECT id, station_uuid, content_hash, source_path, source_size, source_mod_unix, cache_path, title, artist, album,
		duration_ms, frame_count, frame_size, bitrate, sample_rate, channels, status, error, created_at, updated_at FROM tracks`
}

func scanTrack(rows interface {
	Scan(dest ...any) error
}) (Track, error) {
	var t Track
	var created, updated string
	if err := rows.Scan(&t.ID, &t.StationUUID, &t.ContentHash, &t.SourcePath, &t.SourceSize, &t.SourceModUnix, &t.CachePath, &t.Title, &t.Artist, &t.Album,
		&t.DurationMs, &t.FrameCount, &t.FrameSize, &t.Bitrate, &t.SampleRate, &t.Channels, &t.Status, &t.Error, &created, &updated); err != nil {
		return Track{}, err
	}
	t.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	t.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return t, nil
}

func (s *Store) DeleteSlotsBefore(ctx context.Context, stationUUID string, cutoffMs int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM schedule_slots WHERE station_uuid=? AND end_unix_ms < ?`, stationUUID, cutoffMs)
	return err
}

func (s *Store) DeleteFutureSlots(ctx context.Context, stationUUID string, cutoffMs int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM schedule_slots WHERE station_uuid=? AND start_unix_ms > ?`, stationUUID, cutoffMs)
	return err
}

func (s *Store) UpsertSlot(ctx context.Context, sl Slot) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO schedule_slots(id, station_uuid, start_unix_ms, end_unix_ms, track_id, is_silence, frame_count, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			station_uuid=excluded.station_uuid,
			start_unix_ms=excluded.start_unix_ms,
			end_unix_ms=excluded.end_unix_ms,
			track_id=excluded.track_id,
			is_silence=excluded.is_silence,
			frame_count=excluded.frame_count`,
		sl.ID, sl.StationUUID, sl.StartUnixMs, sl.EndUnixMs, nullableString(sl.TrackID), boolInt(sl.IsSilence), sl.FrameCount, time.Now().UTC().Format(time.RFC3339Nano))
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
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO schedule_slots(id, station_uuid, start_unix_ms, end_unix_ms, track_id, is_silence, frame_count, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			station_uuid=excluded.station_uuid,
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
		if _, err := stmt.ExecContext(ctx, sl.ID, sl.StationUUID, sl.StartUnixMs, sl.EndUnixMs, nullableString(sl.TrackID), boolInt(sl.IsSilence), sl.FrameCount, now); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) SlotsEndingAfter(ctx context.Context, stationUUID string, unixMs int64) ([]Slot, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, station_uuid, start_unix_ms, end_unix_ms, track_id, is_silence, frame_count
		FROM schedule_slots WHERE station_uuid=? AND end_unix_ms > ? ORDER BY end_unix_ms ASC`, stationUUID, unixMs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Slot
	for rows.Next() {
		var sl Slot
		var isSilence int
		if err := rows.Scan(&sl.ID, &sl.StationUUID, &sl.StartUnixMs, &sl.EndUnixMs, &sl.TrackID, &isSilence, &sl.FrameCount); err != nil {
			return nil, err
		}
		sl.IsSilence = isSilence != 0
		out = append(out, sl)
	}
	return out, rows.Err()
}

func (s *Store) SlotAt(ctx context.Context, stationUUID string, unixMs int64) (Slot, error) {
	var sl Slot
	var isSilence int
	err := s.db.QueryRowContext(ctx, `SELECT id, station_uuid, start_unix_ms, end_unix_ms, track_id, is_silence, frame_count
		FROM schedule_slots WHERE station_uuid=? AND start_unix_ms <= ? AND end_unix_ms > ? ORDER BY start_unix_ms DESC LIMIT 1`, stationUUID, unixMs, unixMs).
		Scan(&sl.ID, &sl.StationUUID, &sl.StartUnixMs, &sl.EndUnixMs, &sl.TrackID, &isSilence, &sl.FrameCount)
	if err != nil {
		return Slot{}, err
	}
	sl.IsSilence = isSilence != 0
	return sl, nil
}

func (s *Store) LastSlot(ctx context.Context, stationUUID string) (Slot, error) {
	var sl Slot
	var isSilence int
	err := s.db.QueryRowContext(ctx, `SELECT id, station_uuid, start_unix_ms, end_unix_ms, track_id, is_silence, frame_count
		FROM schedule_slots WHERE station_uuid=? ORDER BY end_unix_ms DESC LIMIT 1`, stationUUID).
		Scan(&sl.ID, &sl.StationUUID, &sl.StartUnixMs, &sl.EndUnixMs, &sl.TrackID, &isSilence, &sl.FrameCount)
	if err != nil {
		return Slot{}, err
	}
	sl.IsSilence = isSilence != 0
	return sl, nil
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
