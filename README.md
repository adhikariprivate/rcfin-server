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

### Automatic metadata and subtitle refresh

Metadata and subtitle refresh are independent per-library settings. When enabled, RCFin checks existing library items during its refresh cycle and can write missing NFO and artwork sidecars or download configured external subtitles. Existing NFO fields, artwork, and subtitles are protected by default. Inbox items are enriched separately from the normal library scan.

Current providers:

- **Metadata and artwork:** TMDb.
- **Subtitles:** OpenSubtitles.com.

Provider credentials are supplied through environment variables or an optional local `.env` file. See [provider configuration and media automation](docs/media-layout.md#library-inventory-metadata-subtitles-and-intake) and [`rcfin.example.yaml`](rcfin.example.yaml). Provider terms, quotas, and API credentials apply.

### Jellyfin-compatible playback

RCFin supports account authentication, library and item browsing, search, movie and TV metadata, artwork, playback information, direct video streaming, external subtitle tracks, and user playback state. API coverage is intentionally partial and focused on tested client workflows.

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
