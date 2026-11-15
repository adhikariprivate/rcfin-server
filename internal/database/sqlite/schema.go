package sqlite

import (
	"log"

	"github.com/jmoiron/sqlx"
)

func dbInitSchema(d *sqlx.DB) error {
	schema := []string{
		// This is needed to improve concurrent reads and writes.
		`PRAGMA journal_mode = WAL;`,
		// Without this foreign key constraints won't be enforced and cascade deletes won't happen.
		`PRAGMA foreign_keys = ON;`,

		`CREATE TABLE IF NOT EXISTS items (
id TEXT NOT NULL PRIMARY KEY,
name TEXT NOT NULL,
votes INTEGER,
year INTEGER,
genre TEXT NOT NULL,
rating REAL,
nfotime INTEGER NOT NULL,
firstvideo INTEGER NOT NULL,
lastvideo INTEGER NOT NULL);`,

		`CREATE INDEX IF NOT EXISTS items_name_idx ON items (name);`,

		`CREATE TABLE IF NOT EXISTS users (
id TEXT NOT NULL PRIMARY KEY,
username TEXT NOT NULL,
password TEXT NOT NULL,
created DATETIME,
lastlogin DATETIME,
lastused DATETIME);`,

		`CREATE UNIQUE INDEX IF NOT EXISTS users_name_idx ON users (username);`,

		`CREATE TABLE IF NOT EXISTS user_properties (
userid INTEGER NOT NULL,
key TEXT NOT NULL,
value TEXT,
PRIMARY KEY (userid, key),
FOREIGN KEY (userid) REFERENCES users(id) ON DELETE CASCADE
);`,

		`CREATE TABLE IF NOT EXISTS accesstokens (
userid TEXT NOT NULL,
token TEXT NOT NULL,
deviceid TEXT,
devicename TEXT,
applicationname TEXT,
applicationversion TEXT,
remoteaddress TEXT,
created DATETIME,
lastused DATETIME);`,

		`CREATE UNIQUE INDEX IF NOT EXISTS accesstokens_idx ON accesstokens (userid, token);`,

		`CREATE TABLE IF NOT EXISTS quickconnect (
userid TEXT NOT NULL,
deviceid TEXT NOT NULL,
secret TEXT NOT NULL,
authorized BOOLEAN NOT NULL,
code TEXT NOT NULL,
created DATETIME NOT NULL,
PRIMARY KEY(deviceid, secret));`,

		`CREATE TABLE IF NOT EXISTS playstate (
userid TEXT NOT NULL,
itemid TEXT NOT NULL,
position INTEGER,
playedpercentage INTEGER,
played BOOLEAN,
playcount INTEGER,
favorite BOOLEAN,
timestamp DATETIME);`,

		`CREATE UNIQUE INDEX IF NOT EXISTS userid_itemid_idx ON playstate (userid, itemid);`,

		`CREATE TABLE IF NOT EXISTS playlists (
id TEXT NOT NULL PRIMARY KEY,
name TEXT NOT NULL,
userid TEXT NOT NULL,
created DATETIME,
lastupdated DATETIME);`,

		`CREATE TABLE IF NOT EXISTS playlist_items (
playlistid TEXT NOT NULL,
itemid TEXT NOT NULL,
itemorder INTEGER NOT NULL,
created DATETIME,
lastupdated DATETIME,
PRIMARY KEY (playlistid, itemid),
FOREIGN KEY (playlistid) REFERENCES playlists(id));`,

		`CREATE TABLE IF NOT EXISTS images (
itemid TEXT NOT NULL,
type TEXT NOT NULL,
mimetype TEXT NOT NULL,
etag TEXT NOT NULL,
updated DATETIME NOT NULL,
filesize INTEGER NOT NULL,
data BLOB NOT NULL);`,

		`CREATE UNIQUE INDEX IF NOT EXISTS images_idx ON images (itemid, type)`,
		`CREATE TABLE IF NOT EXISTS media_operations (
id TEXT NOT NULL PRIMARY KEY,
collection_id TEXT NOT NULL,
item_key TEXT NOT NULL,
source_path TEXT NOT NULL,
destination_path TEXT NOT NULL,
action TEXT NOT NULL,
status TEXT NOT NULL,
provider_id TEXT NOT NULL,
confidence REAL NOT NULL,
evidence TEXT NOT NULL,
attempts INTEGER NOT NULL,
next_attempt DATETIME NOT NULL,
lease_owner TEXT NOT NULL,
lease_until DATETIME NOT NULL,
bytes INTEGER NOT NULL,
checksum TEXT NOT NULL,
error TEXT NOT NULL,
created_at DATETIME NOT NULL,
updated_at DATETIME NOT NULL,
UNIQUE(collection_id,item_key,action));`,
		`CREATE INDEX IF NOT EXISTS media_operations_status_idx ON media_operations (status,next_attempt,lease_until)`,
		`CREATE INDEX IF NOT EXISTS media_operations_collection_updated_idx ON media_operations (collection_id,updated_at DESC,id DESC)`,
		`CREATE TABLE IF NOT EXISTS library_inventory_snapshots (
collection_id TEXT NOT NULL PRIMARY KEY,
generation INTEGER NOT NULL,
completed_at DATETIME NOT NULL,
payload TEXT NOT NULL);`,

		`CREATE TABLE IF NOT EXISTS schema_migrations (
version INTEGER NOT NULL PRIMARY KEY,
applied DATETIME NOT NULL);`,
	}

	for _, query := range schema {
		if _, err := d.Exec(query); err != nil {
			log.Printf("dbInitSchema error: %s\n", err)
			return err
		}
	}
	return nil
}
