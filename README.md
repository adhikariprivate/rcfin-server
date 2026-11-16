# RCFin

RCFin is a Jellyfin-compatible media server for people who store movies and TV shows on Rclone remotes.

- RCFin implements part of the Jellyfin API for compatible clients. It is not a full Jellyfin server.
- RCFin embeds Rclone v1.76.0, so you do not need a separate Rclone process or a FUSE mount.
- RCFin serves original media files. It does not transcode, remux, or generate HLS.
- Release builds target Linux, Windows, FreeBSD, OpenBSD, and NetBSD. Some architecture combinations are excluded.
- RCFin stores application data in SQLite or PostgreSQL.

Build RCFin from source with the repository's mise tasks. RCFin does not have a package installer.

```bash
mise install
mise run build
```
```bash
cp config.example.yaml rcfin.yaml
$EDITOR rcfin.yaml
./bin/rcfin --config rcfin.yaml
# RCFin listens on port 8096 unless you change it.
```

## Where to start

Run `./bin/rcfin --config rcfin.yaml` first. It starts the server with the configured libraries. On an empty database, create the first administrator with `./bin/rcfin --config rcfin.yaml user add --username admin --admin`. RCFin does not create a default account.

## Getting started

1. Install the Go version selected by `mise.toml` with `mise install`.
2. Create an Rclone config file with a section for each named remote. RCFin reads this file directly.
3. Copy [`config.example.yaml`](config.example.yaml) to `rcfin.yaml`. Set the Rclone config path, library remote paths, and persistent local paths.
4. Make sure the service account can read the config and Rclone credentials. It must be able to write the database, cache directories, and tsnet state directory when Tailscale is enabled.
5. Build RCFin with `mise run build`, then start `./bin/rcfin --config rcfin.yaml`.
6. In another terminal, create an administrator with `./bin/rcfin --config rcfin.yaml user add --username admin --admin`.
7. Add the server address to a Jellyfin-compatible client and sign in with that account.

The default public HTTP port is `8096`. Use [`config.example.full.yaml`](config.example.full.yaml) for every application setting and VFS option. The [configuration reference](docs/configuration.md) explains the fields.

## Features

### Native Rclone support

RCFin links the Rclone Go SDK into the server. It reads remote definitions and credentials from an Rclone config file. Each collection uses an Rclone `remote:path`, such as `media:Movies`. RCFin includes the backends compiled into Rclone v1.76.0.

### Direct playback from remote storage

RCFin reads media through Rclone's VFS and serves byte ranges to clients. Compatible clients can seek without downloading the whole library first. Playback speed depends on the storage backend and its range-read support.

### Separate storage settings for each library

Each collection has its own Rclone filesystem and VFS settings. Tune directory caching, read chunks, and local cache limits separately for each library.

### Jellyfin-compatible client API

RCFin implements selected Jellyfin API endpoints for media clients. Client support depends on the workflow each app uses. RCFin does not implement the full Jellyfin API.

### Jellyfin client sign-in

Clients can sign in with RCFin accounts and use authenticated API tokens for supported requests.

### Library browsing

Compatible clients can browse configured libraries, movies, shows, seasons, and episodes.

### Search

Clients can search the media catalog by supported item fields.

### Playback state

RCFin stores user playback progress and resume state in the selected database.

### External subtitle tracks

RCFin advertises SRT and VTT sidecar subtitles for supported items and serves them through authenticated Jellyfin-compatible URLs. It does not inspect subtitles embedded in media files.

### Quick Connect

Quick Connect is available when `jellyfin.quick_connect` is enabled. It is disabled by default.

### Movie collections

Add movie libraries as collections with a stable ID, display name, Rclone path, and optional VFS or media settings.

### TV collections

Add show libraries as collections. RCFin recognizes supported season and episode layouts described in the [media layout guide](docs/media-layout.md).

One server can host multiple movie and TV collections.

### Account management

Create, disable, delete, and administer accounts from the RCFin CLI. There is no public registration flow. Passwords are entered through hidden terminal prompts, not command-line arguments.

### Library access grants

Regular accounts start with no library access. Grant access by collection ID. Administrators can access every configured collection. Jellyfin routes and WebDAV both enforce these grants.

### Public HTTP listener

RCFin listens on the configured public address and port. The default port is `8096`. Bind to a specific interface, such as `127.0.0.1`, when a local reverse proxy should accept public traffic.

### TLS with your certificate

Set `listen.tls_cert` and `listen.tls_key` to enable HTTPS. RCFin does not request or renew certificates automatically. The configured certificate is also used by the Tailscale listener when TLS is enabled.

### Public IP allow list

Set `listen.ip_acl` to a comma-separated list of IP addresses or CIDR ranges. It is an allow list. An empty value allows all source IPs at the middleware level. The rule applies to the public listener only.

### Native Tailscale support

Enable `listen.tailscale` to run RCFin as a tsnet node and accept connections from the tailnet. Configure the node hostname, auth key environment variable, and persistent state directory. Keep the state directory across restarts to reuse the same node identity. The Tailscale listener bypasses the public IP allow list.

### Headscale control server

Set `listen.tailscale.control_url` to your Headscale URL to join a self-hosted control server. Headscale's DNS settings determine the full device name. RCFin sets the machine hostname, such as `handler`, but does not configure the DNS suffix.

### Read-only WebDAV

Enable WebDAV to publish virtual views of your libraries. It can expose All, Genres, Actors, Directors, Years, and Studios views, with optional NFO and artwork sidecars. WebDAV does not create or move media files. Users see only collections their account can access.

### Scheduled library scans

RCFin scans each library on a configurable interval. Directory listing cache expiry is a separate Rclone VFS setting. A scan can discover changes after the VFS directory cache has refreshed.

### Inbox upload readiness

Each collection can have an Inbox path excluded from normal playback browsing. RCFin can wait for a completion marker or for files to stay unchanged for a configured quiet period before processing an upload.

### TMDb metadata lookup

The optional metadata stage uses TMDb to find movie and show details. It preserves existing metadata by default. Set a TMDb credential through the environment or a protected `.env` file.

### NFO sidecars

When enabled, metadata refresh writes Kodi-style NFO files beside movies, shows, or episodes. Existing NFO fields remain unless replacement is configured.

### Artwork downloads

The metadata stage can download poster and backdrop images from TMDb. It preserves existing artwork by default. Artwork size and format limits are configurable.

### Subtitle downloads

The optional subtitle stage searches OpenSubtitles.com and saves matching SRT or VTT sidecars. Configure languages, forced and hearing-impaired preferences, confidence, file size, and overwrite behavior. Provider credentials belong in environment variables or a protected `.env` file.

### Dry-run media organization

RCFin can propose normalized movie and TV paths for ready Inbox items. Organization is disabled by default and dry-run is on by default. Preview plans before enabling writes. Unknown files, uncertain matches, and destination conflicts are left for review.

### Existing-path normalization

An optional separate workflow can plan normalized paths for existing library items outside the Inbox. It is disabled by default and has its own dry-run setting.

### SQLite storage

SQLite is the default database for account and application state. Keep its file on persistent storage.

### PostgreSQL storage

PostgreSQL stores account and application state in a server outside the RCFin process. Artwork and Rclone VFS caches still use local storage.

### Artwork resize cache

RCFin caches resized artwork in `cachedir`. The cache is local and can be rebuilt.

### Rclone media cache

Rclone's VFS can cache media ranges on local storage. Configure cache mode, size, and age separately for each collection. The cache can be rebuilt. Keep the database and tsnet state directory on persistent storage when you need to retain their data and identity.

## Limits

- RCFin serves original files. It does not transcode, remux, or create HLS streams.
- A client must support the media's codecs and container for direct playback.
- RCFin implements selected Jellyfin-compatible routes, not the full Jellyfin server API.
- Subtitle automation checks sidecar files. It does not inspect embedded subtitle streams.
- Storage behavior and seek performance vary by Rclone backend.

## Documentation

- [Configuration reference](docs/configuration.md)
- [Networking and access](docs/networking.md)
- [Rclone storage](docs/rclone-storage.md)
- [VFS options](docs/vfs-options.md)
- [Account administration](docs/accounts.md)
- [Virtual WebDAV](docs/webdav.md)
- [Media layout and automation](docs/media-layout.md)
- [Database backends](docs/database.md)
- [Architecture](docs/architecture.md)
- [Release targets](docs/releases.md)

## Help and maintenance

The project is maintained in this public repository. Ask questions or report bugs in [GitHub Issues](https://github.com/adhikariprivate/rcfin-server/issues).

See [Contributing](CONTRIBUTING.md) for local development instructions and [LICENSE](LICENSE) for the MIT License. The repository does not currently contain a `CHANGELOG.md`; release information is in [docs/releases.md](docs/releases.md) and [GitHub Releases](https://github.com/adhikariprivate/rcfin-server/releases).

RCFin is a separately maintained fork of [Jellofin Server](https://github.com/erikbos/jellofin-server).
