# RCFin

RCFin is a lightweight media server that exposes a partial Jellyfin-compatible API. It scans movie and TV-show libraries, reads Kodi-style NFO metadata, stores user and library state in SQLite or PostgreSQL, and serves original media files to compatible clients.

RCFin is a fork of [Jellofin Server](https://github.com/erikbos/jellofin-server). RCFin continues that project as a separately named fork and uses Jellyfin as its client-facing API.

## Features and compatibility

- **Jellyfin API compatibility:** Partial. Implemented areas include authentication and user management, Quick Connect, library and item browsing, movie and TV metadata, search, artwork, playback information, direct video streaming, and user playback state. The implementation returns Jellyfin-shaped responses for these supported routes; it does not implement the complete Jellyfin API. Unsupported operations include transcoding, remuxing, and Jellyfin features or endpoints not implemented by RCFin.
- **Media storage:** Media files are accessed through an embedded Rclone VFS; no FUSE mount or external Rclone process is required. Rclone v1.76.0 upstreams are compiled into the binary. Each library selects its own remote from a global Rclone config file and can set independent VFS options. SQLite is local; PostgreSQL can be remote. Application caches remain local and rebuildable. See [`docs/`](docs/index.md) for setup, supported options, caching, and tuning.
- **Transcoding:** Not supported. RCFin serves original media files and does not transcode or remux them.
- **Notflix:** Not supported. Notflix API compatibility has been removed.

### Compatible clients

These clients were reported to work with the original Jellofin Server. They have not all been re-verified against the current RCFin fork:

| Client | Reported status |
| --- | --- |
| [Infuse](https://firecore.com/infuse) | Full player functionality upstream |
| [SenPlayer](https://apps.apple.com/ca/app/senplayer-video-media-player/id6443975850) | Full player functionality upstream |
| [Streamyfin](https://streamyfin.app/) | Full player functionality upstream |
| [VidHub](https://okaapps.com/product/1659622164) | Full player functionality upstream |

## Quick start

Install the pinned Go version and run the development build with [mise](https://mise.jdx.dev/):

```sh
mise install
mise run build
cp rcfin.example.yaml rcfin.yaml
./bin/rcfin --config rcfin.yaml
```

Edit `rcfin.yaml` before starting the server. Choose SQLite or PostgreSQL under `database`; PostgreSQL connection details can be supplied using `RCFIN_POSTGRES_URL`. Set the global Rclone config/cache paths. `rclone.config` must name the actual Rclone INI file, including its filename. Set each collection's `remote` to a configured `remote:path` such as `files:HTPC/Movies`. Caches remain local. The default HTTP port is `8096`.

## Configuration

- `listen`: bind address, port, optional TLS certificate and key, and optional IP allow list.
- `database.driver`: `sqlite` (default for existing configs) or `postgres`.
- `database.sqlite.filename`: local SQLite database path.
- `database.postgres.url`: PostgreSQL connection URL; `RCFIN_POSTGRES_URL` is used when this is empty.
- `cachedir`: local image cache path.
- `rclone`: global config-file path and local VFS cache root.
- `rescan_interval`: application library refresh interval (default `30m`).
- `collections`: required stable ID, display name, type (`movies` or `shows`), `remote` Rclone path, and per-library `vfs` options. User library grants refer to the ID, so keep it unchanged if you rename a library.
- `jellyfin`: server name and supported user / Quick Connect settings.
- `logfile`: log destination; use `stdout` (default), `stderr`, `none`, or a writable file path.

See [`rcfin.example.yaml`](rcfin.example.yaml) for a complete example and [`docs/configuration.md`](docs/configuration.md) for field definitions.

See [`docs/database.md`](docs/database.md) for PostgreSQL credentials, container persistence, and database migration behavior.

## User accounts and sign-in

RCFin stores manually managed accounts in its selected database. There is no sign-up flow and no default account. Create the first administrator and manage users and library grants with the CLI commands documented in [Account administration](docs/accounts.md). New regular users start with no library access; administrators can access every library. Existing non-admin accounts lose their previous all-library access on the `v1.1.0` database migration, and must be granted access explicitly. Users can change their own passwords in compatible clients. Quick Connect is disabled unless `jellyfin.quick_connect` is enabled.

## Library layout

Movie libraries have one directory per movie, containing the video and optional NFO and artwork files:

```text
movies/
├── Movie 1 (1984)/
│   ├── Movie 1 (1984).mp4
│   ├── movie.nfo
│   └── poster.jpg
└── Movie 2 (2001)/
    └── Movie 2 (2001).mp4
```

TV libraries have a directory per show and `S<number>` season directories. Episode filenames must include a separator and text after the season/episode number; for example, `S01E01 - Pilot.mp4` (a bare `S01E01.mp4` is not recognized by the current parser):

```text
tv/
└── Show Name/
    ├── tvshow.nfo
    └── S01/
        ├── Show Name S01E01 - Pilot.mp4
        └── Show Name S01E02 - Second Episode.mp4
```

See [TV library naming and sidecar files](docs/media-layout.md) for supported video extensions, episode patterns, subtitles, NFO files, and artwork.

Nested movie grouping directories and mixed movie/show layouts in one collection are not supported. Add each root as its own collection.

## Development and contributions

See [CONTRIBUTING.md](CONTRIBUTING.md) for setup, development commands, and contribution guidelines. RCFin is licensed under the MIT License; see [LICENSE](LICENSE).

See [docs/releases.md](docs/releases.md) for supported binary targets and the automated release process.
