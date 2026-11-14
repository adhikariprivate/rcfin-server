# Database backends and container deployments

RCFin supports SQLite and PostgreSQL through the same repository interface. Choose one backend with `database.driver`. Existing configuration files that omit `database.driver` continue to use SQLite.

## SQLite

SQLite is the simplest option for a single RCFin instance on a machine with persistent local storage:

```yaml
database:
  driver: sqlite
  sqlite:
    filename: /var/lib/rcfin/rcfin.db
```

The database file is durable only if `/var/lib/rcfin` is on persistent storage. For a container, mount a volume there if the database must survive container replacement.

## PostgreSQL

Use PostgreSQL when application state should live outside the RCFin container:

```yaml
database:
  driver: postgres
  postgres:
    url: ""
```

Supply the connection URL using the `RCFIN_POSTGRES_URL` environment variable, for example through the container platform's secret mechanism. RCFin reads this variable only when `database.postgres.url` is empty. The URL uses PostgreSQL's standard URI form:

```text
postgres://USER:PASSWORD@HOST:5432/DATABASE?sslmode=verify-full
```

For a remote database, configure TLS and certificate verification according to the database provider. Percent-encode reserved characters in URI credentials. Do not commit passwords in `rcfin.yaml` or place them in shell history.

The PostgreSQL database must already exist and be reachable from the RCFin process. At startup, RCFin creates its tables and indexes and applies pending application migrations. The configured database role must be able to connect and own, or create objects in, the schema RCFin uses. The server and management commands such as `user add` and `user list` use this same driver and connection setting.

## What must persist

With PostgreSQL selected, the RCFin container's writable cache directories can be ephemeral. The PostgreSQL database holds account credentials and grants, access tokens, Quick Connect codes, playback state, playlists, uploaded images, and scanned item records. The media files remain on the configured Rclone remotes.

These local files are caches or rebuildable runtime state:

- `cachedir`: resized artwork cache.
- `rclone.cache_dir`: Rclone VFS cache, including locally cached media reads.
- The Bleve search index and loaded collection objects: rebuilt in process from the configured libraries and their remote contents.
- The active Jellyfin session table: rebuilt as clients reconnect. Access tokens themselves are stored in the selected database.

If the container stops writing these paths, RCFin can recreate the caches as they are needed. Keep the configuration file and Rclone credentials available to each replacement container; they are required inputs, not cache data. Set `jellyfin.server_id` explicitly if the server identity must remain stable when the container hostname changes.

Keep regular backups of PostgreSQL. A remote database survives RCFin container replacement, but it does not by itself protect against database loss or provider-side failures.

## Playback write behavior and instance count

Playback updates are held in the RCFin process's memory and flushed to the selected database in batches every 10 seconds. This reduces write frequency, but an abrupt process or container stop can lose playback updates that have not reached the database yet. Access-token creation and deletion are written immediately; token last-used timestamps are buffered as well.

The playback cache and active-session table are process-local and are not coordinated between RCFin instances. Run one RCFin instance against a database unless you have separately accounted for requests reaching different instances and their independent in-memory playback views and sessions. PostgreSQL support makes the database remote; it does not add distributed cache coordination or multi-instance session affinity.

## Moving from SQLite

Changing `database.driver` from `sqlite` to `postgres` selects a separate, initially empty database. RCFin does not currently copy or import the SQLite database. Existing accounts, grants, sessions, playback state, playlists, and uploaded images stay in SQLite until you migrate them separately. Keep a backup of the SQLite file before switching, and create an administrator in PostgreSQL with the normal CLI after configuring the PostgreSQL URL.
