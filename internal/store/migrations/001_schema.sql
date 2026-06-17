-- +goose Up
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

CREATE TABLE settings (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

-- +goose StatementBegin
CREATE TRIGGER trg_tracks_catalog_revision_insert
	AFTER INSERT ON tracks
	BEGIN
		UPDATE catalog_state SET revision = revision + 1, updated_at = datetime('now') WHERE station_uuid = NEW.station_uuid;
	END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER trg_tracks_catalog_revision_update
	AFTER UPDATE ON tracks
	BEGIN
		UPDATE catalog_state SET revision = revision + 1, updated_at = datetime('now') WHERE station_uuid = NEW.station_uuid;
	END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER trg_tracks_catalog_revision_delete
	AFTER DELETE ON tracks
	BEGIN
		UPDATE catalog_state SET revision = revision + 1, updated_at = datetime('now') WHERE station_uuid = OLD.station_uuid;
	END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER trg_track_assets_catalog_revision_insert
	AFTER INSERT ON track_assets
	BEGIN
		UPDATE catalog_state SET revision = revision + 1, updated_at = datetime('now')
		WHERE station_uuid = (SELECT station_uuid FROM tracks WHERE id = NEW.track_id);
	END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER trg_track_assets_catalog_revision_update
	AFTER UPDATE ON track_assets
	BEGIN
		UPDATE catalog_state SET revision = revision + 1, updated_at = datetime('now')
		WHERE station_uuid = (SELECT station_uuid FROM tracks WHERE id = NEW.track_id);
	END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER trg_track_assets_catalog_revision_delete
	AFTER DELETE ON track_assets
	BEGIN
		UPDATE catalog_state SET revision = revision + 1, updated_at = datetime('now')
		WHERE station_uuid = (SELECT station_uuid FROM tracks WHERE id = OLD.track_id);
	END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER IF EXISTS trg_track_assets_catalog_revision_delete;
DROP TRIGGER IF EXISTS trg_track_assets_catalog_revision_update;
DROP TRIGGER IF EXISTS trg_track_assets_catalog_revision_insert;
DROP TRIGGER IF EXISTS trg_tracks_catalog_revision_delete;
DROP TRIGGER IF EXISTS trg_tracks_catalog_revision_update;
DROP TRIGGER IF EXISTS trg_tracks_catalog_revision_insert;
DROP TABLE IF EXISTS settings;
DROP TABLE IF EXISTS catalog_state;
DROP TABLE IF EXISTS schedule_slots;
DROP TABLE IF EXISTS track_assets;
DROP TABLE IF EXISTS tracks;
DROP TABLE IF EXISTS stations;
