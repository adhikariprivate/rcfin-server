# RCFin

RCFin is a Jellyfin-compatible media server for people whose libraries live on Rclone remotes. It reads and streams media through Rclone's Go SDK in the server process, so you do not need to mount your remote with FUSE or run a separate Rclone service.

**RCFin does not transcode.** It serves the original media files directly to compatible clients. The client and media format must support direct playback.

RCFin is a separately maintained fork of [Jellofin Server](https://github.com/erikbos/jellofin-server). It implements the Jellyfin API features needed by its supported clients; it is not a full Jellyfin server replacement.

## Tested clients

These Jellyfin clients have been tested with RCFin:

- Fladder
- Swiftfin

Client compatibility can vary by app version and feature. RCFin focuses on library browsing, authentication, direct streaming, metadata, artwork, and external subtitles.

## Why Rclone without FUSE?

RCFin embeds Rclone and accesses each configured remote directly. There is no FUSE mount, kernel module, external Rclone daemon, or mounted filesystem to keep alive. This avoids host-level mount setup and the additional privileges or packages that FUSE deployments can require, which is useful on VPS and managed hosting.

Rclone's VFS provides the file operations RCFin needs for directory scans, artwork, and ranged media reads. RCFin also:

- Keeps a separate VFS and cache policy for each library.
- Caches directory listings so repeated browsing does not have to list the remote on every request.
- Runs application library scans on a configurable interval, independently from VFS directory-cache expiry.
- Uses HTTP range reads and seekable VFS handles for playback, seeking, and compatible client requests.
- Caches the ranges clients actually read, with configurable cache limits, read chunks, and handle reuse.

The selected Rclone backend still determines available features and range-read performance. See [Rclone-backed storage](docs/rclone-storage.md) and the [VFS options](docs/vfs-options.md).

## Features

### Libraries backed by different remotes

Configure each library as a collection with its own stable ID, type, Rclone `remote:path`, and VFS options. A single RCFin server can, for example, serve Movies from one remote, TV from another, and Anime from a third. Library IDs are also used for account access grants; keep them stable when changing display names.

### Accounts managed by CLI

There is no public sign-up flow. An administrator creates accounts and controls which libraries each user can access using the RCFin CLI. Users then sign in from a compatible Jellyfin client. See [Account administration](docs/accounts.md) for commands to create users, reset passwords, enable or disable accounts, and grant library access.

### Inbox intake and organization

Each library can have an excluded `.Inbox` path for new or restaged media. RCFin inventories ready Inbox items, classifies video and sidecar files, checks identity, and can enrich them with metadata and subtitles before proposing a normalized movie or TV layout. Uploads can be held until a completion marker appears or their size and modification time have been stable for a configured interval.

Organization is disabled by default and starts in dry-run mode. Ambiguous matches, unrecognized files, incomplete uploads, and destination conflicts are left for review. Existing library items are not moved by routine metadata or subtitle refresh. See [Media layout and automation](docs/media-layout.md) for Inbox setup, supported layouts, safety controls, and CLI previews.

### One inventory cycle for metadata and subtitles

Each library has one polling interval for the complete background pipeline. RCFin lists the library and `.Inbox` together, reconciles the result with its saved inventory, waits for Inbox transfers to settle, provisions ready Inbox items, then runs eligible metadata before subtitles from that same inventory. Metadata and subtitle stages do not start their own polling scans. Unchanged existing items are left alone; provider work is reserved for new or changed items, relevant processing algorithm upgrades, and explicit CLI refreshes. Existing NFOs, artwork, and subtitles are preserved unless an administrator requests stage-specific replacement.

Inbox readiness uses a completion marker written after upload completion, when configured, or a durable quiet period based on stable file attributes. A changing or newly arriving file resets the quiet period. Organization remains disabled by default and uncertain layouts or collisions stay available for CLI review.

Metadata and artwork use TMDb. Subtitle search and downloads use OpenSubtitles.com. Provider credentials are supplied through environment variables or an optional local `.env` file. See [provider configuration and media automation](docs/media-layout.md#library-inventory-metadata-subtitles-and-intake), [configuration](docs/configuration.md), and [`rcfin.example.yaml`](rcfin.example.yaml).

### CLI library administration

Administrative media actions run through the server CLI; there is no admin API or web UI for these workflows. Common commands:

```sh
./rcfin library scan movies [--full]
./rcfin library status movies [--item 'Movie (2024)']
./rcfin library inbox status movies
./rcfin library refresh metadata movies --item 'Movie (2024)'
./rcfin library refresh subtitles movies --item 'Movie (2024)' --language en
./rcfin library refresh all movies --item 'Movie (2024)'
./rcfin library organize plan movies --item '.Inbox/Movie (2024)'
./rcfin library organize apply movies --item '.Inbox/Movie (2024)' --yes
./rcfin library retry movies --job JOB_ID
```

`scan` reconciles inventory and does not imply provider refresh. A refresh without replacement fills missing configured outputs while preserving files already present. Use the stage-specific `--replace` options only after reviewing the target and report. See [Media layout and automation](docs/media-layout.md) for full command behavior and safety requirements.

Current providers:

- **Metadata and artwork:** TMDb.
- **Subtitles:** OpenSubtitles.com.

### Jellyfin-compatible playback

RCFin supports account authentication, library and item browsing, search, movie and TV metadata, artwork, playback information, direct video streaming, external subtitle tracks, and user playback state. API coverage is intentionally partial and focused on tested client workflows.

### Read-only virtual WebDAV

RCFin can expose libraries through a virtual, read-only WebDAV tree at `/dav/`.
It builds **All**, **Genres**, **Actors**, **Directors**, and **Years** views
from the library catalog, with Kodi-friendly NFO, artwork, and subtitle
sidecars. Virtual paths resolve to the current media paths in Rclone; they do
not create or rename remote files. WebDAV uses RCFin usernames and passwords,
with the same per-library access grants as Jellyfin clients. Every authorized
client sees the same configured views and sidecars. See [Virtual WebDAV](docs/webdav.md)
for setup, configuration, and client mounting details.

## Quick start

Install the Go version pinned by [mise](https://mise.jdx.dev/), prepare a configuration, then build and run:

```sh
mise install
cp rcfin.example.yaml rcfin.yaml
# Edit rcfin.yaml with your database, Rclone config, and library paths.
mise run build
./bin/rcfin --config rcfin.yaml
```

The default HTTP port is `8096`. Rclone's config setting must point to the actual INI file, including its filename. Each collection's `remote` must use a configured Rclone section, for example `media:Movies`. Keep Rclone credentials in the Rclone config or environment, not in the README or a committed YAML file.

See [Configuration](docs/configuration.md) for all settings and [Database backends](docs/database.md) for SQLite and PostgreSQL setup.

## Build and verify

```sh
mise install
mise run build
mise run test
```

See [Contributing](CONTRIBUTING.md) for repository structure and contribution guidance. Browse the [documentation index](docs/index.md) for configuration, storage, media layout, accounts, and architecture guides.

## Project notes

- RCFin serves original media; transcoding, remuxing, and HLS generation are not supported.
- RCFin embeds Rclone v1.76.0. Available backends and capabilities match that pinned SDK build; backend-specific authentication and range-read behavior still vary.
- Media organization and metadata writes require explicit per-library configuration and writable VFS settings. Review dry-run plans before enabling moves.
- User and library state is stored in SQLite by default or PostgreSQL when configured. Artwork and Rclone VFS caches are local and rebuildable.

RCFin is licensed under the MIT License. See [LICENSE](LICENSE).
