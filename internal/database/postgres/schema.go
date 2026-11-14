package postgres

import "github.com/jmoiron/sqlx"

func dbInitSchema(db *sqlx.DB) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS items (id TEXT PRIMARY KEY, name TEXT NOT NULL, votes INTEGER, year INTEGER, genre TEXT NOT NULL, rating REAL, nfotime BIGINT NOT NULL, firstvideo BIGINT NOT NULL, lastvideo BIGINT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS items_name_idx ON items (name)`,
		`CREATE TABLE IF NOT EXISTS users (id TEXT PRIMARY KEY, username TEXT NOT NULL UNIQUE, password TEXT NOT NULL, created TIMESTAMPTZ, lastlogin TIMESTAMPTZ, lastused TIMESTAMPTZ)`,
		`CREATE TABLE IF NOT EXISTS user_properties (userid TEXT NOT NULL, key TEXT NOT NULL, value TEXT, PRIMARY KEY (userid, key), FOREIGN KEY (userid) REFERENCES users(id) ON DELETE CASCADE)`,
		`CREATE TABLE IF NOT EXISTS accesstokens (userid TEXT NOT NULL, token TEXT NOT NULL, deviceid TEXT, devicename TEXT, applicationname TEXT, applicationversion TEXT, remoteaddress TEXT, created TIMESTAMPTZ, lastused TIMESTAMPTZ)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS accesstokens_idx ON accesstokens (userid, token)`,
		`CREATE INDEX IF NOT EXISTS accesstokens_deviceid_idx ON accesstokens (deviceid)`,
		`CREATE TABLE IF NOT EXISTS quickconnect (userid TEXT NOT NULL, deviceid TEXT NOT NULL, secret TEXT NOT NULL, authorized BOOLEAN NOT NULL, code TEXT NOT NULL, created TIMESTAMPTZ NOT NULL, PRIMARY KEY (deviceid, secret))`,
		`CREATE INDEX IF NOT EXISTS quickconnect_code_idx ON quickconnect (code)`,
		`CREATE TABLE IF NOT EXISTS playstate (userid TEXT NOT NULL, itemid TEXT NOT NULL, position BIGINT, playedpercentage INTEGER, played BOOLEAN, playcount INTEGER, favorite BOOLEAN, timestamp TIMESTAMPTZ, PRIMARY KEY (userid, itemid))`,
		`CREATE TABLE IF NOT EXISTS playlists (id TEXT PRIMARY KEY, name TEXT NOT NULL, userid TEXT NOT NULL, created TIMESTAMPTZ, lastupdated TIMESTAMPTZ)`,
		`CREATE TABLE IF NOT EXISTS playlist_items (playlistid TEXT NOT NULL REFERENCES playlists(id) ON DELETE CASCADE, itemid TEXT NOT NULL, itemorder INTEGER NOT NULL, created TIMESTAMPTZ, lastupdated TIMESTAMPTZ, PRIMARY KEY (playlistid, itemid))`,
		`CREATE TABLE IF NOT EXISTS images (itemid TEXT NOT NULL, type TEXT NOT NULL, mimetype TEXT NOT NULL, etag TEXT NOT NULL, updated TIMESTAMPTZ NOT NULL, filesize BIGINT NOT NULL, data BYTEA NOT NULL, PRIMARY KEY (itemid, type))`,
		`CREATE TABLE IF NOT EXISTS persons (id TEXT PRIMARY KEY, name TEXT NOT NULL, date_of_birth TIMESTAMPTZ, place_of_birth TEXT, profile_path TEXT, biography TEXT)`,
		`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied TIMESTAMPTZ NOT NULL)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}
