# RCFin

RCFin is a Jellyfin-compatible media server for movie and TV libraries stored on Rclone remotes.

- Play media from Rclone storage without setting up a FUSE mount or separate Rclone process.
- Connect privately with native Tailscale support, including custom Headscale control servers.
- Optionally fetch metadata and artwork from TMDb.
- Optionally find and download subtitles from OpenSubtitles.com.
- Give each account access only to the libraries you choose.

## Get started

Download the archive for your operating system and processor from [GitHub Releases](https://github.com/adhikariprivate/rcfin-server/releases/latest). Release archives include the RCFin binary and an example configuration.

For Linux on x86-64, save the matching archive to an empty folder, extract it, copy its example configuration, and start RCFin:

```sh
tar -xzf rcfin_*_linux_amd64.tar.gz
if test -f config.example.yaml; then cp config.example.yaml rcfin.yaml; else cp rcfin.example.yaml rcfin.yaml; fi
./rcfin --config rcfin.yaml
```

Before starting RCFin, edit `rcfin.yaml` and set the path to your Rclone config file and the `remote:path` values for your libraries. Create an Rclone config section for each named remote. Make sure the service account can read those files and write to the configured database and cache directories.

In a second terminal, create the first administrator:

```sh
./rcfin --config rcfin.yaml user add --username admin --admin
```

Add the server address to a Jellyfin-compatible client and sign in with that account. RCFin listens on port `8096` by default.

Release archives use names such as `rcfin_<version>_linux_amd64.tar.gz`. Choose the archive for your operating system and architecture. Windows archives are ZIP files. See [release targets](docs/releases.md) for the full platform list.

On Windows, extract the ZIP archive, copy its example configuration to `rcfin.yaml`, then run `rcfin.exe --config rcfin.yaml` in PowerShell.

The latest published release is v1.2.1. Native Tailscale support was added after that release, so check the release notes for a newer binary that includes it.

## Library layout and uploads

RCFin expects movie and TV files to follow a recognizable folder structure. You can place files directly in the library in that structure, or upload them to the configured Inbox first. The Inbox is recommended for transfers because RCFin can wait until an upload is complete before processing it.

For movies, use one folder per movie with the video file directly inside:

```text
Movies/
└── Example Movie (2020)/
    ├── Example Movie.mp4
    ├── movie.nfo
    └── poster.jpg
```

For TV, put episodes in season folders and include a season/episode token plus a title in each video filename:

```text
TV/
└── The Fixers/
    └── Season 01/
        └── The Fixers S01E01 - The Pilot.mp4
```

New files can go straight into these library folders. For an upload workflow, put each new movie or show under `.Inbox` (or the path configured with `media.inbox_path`):

```text
.Inbox/
├── Example Movie (2020)/
│   └── Example Movie.mp4
└── The Fixers/
    └── Season 01/
        └── The Fixers S01E01 - The Pilot.mp4
```

RCFin excludes the Inbox from normal library browsing. By default, it waits until an item's files have stopped changing for 10 minutes before treating the transfer as complete. You can change this with `media.inbox.stable_for` or configure a completion marker. On a library scan, ready Inbox items enter the configured processing stages: metadata and artwork can be fetched from TMDb, subtitles can be downloaded, and organization can move and normalize files into the library layout. These stages must be enabled in the configuration. Organization is disabled by default and defaults to dry-run mode; its folder and filename templates are configurable. See the [media layout and automation guide](docs/media-layout.md) for naming rules, details on the processing order, and configuration examples.

## Clients and WebDAV

Fladder is a tested compatible client for RCFin's Jellyfin-compatible playback and library workflows. Other Jellyfin clients may work with the endpoints they use; client support can vary because RCFin does not implement the full Jellyfin server API.

RCFin can also publish a read-only WebDAV view for Kodi and other WebDAV clients. Enable the global WebDAV endpoint and the collection's WebDAV view in the configuration. The default endpoint path is `/dav/`; its virtual layout groups collections by display name and provides views such as `All`, `Genres`, `Actors`, `Directors`, and `Years`.

For example, if the collection is named `Movies`, add `https://<server>:8096/dav/Movies/All/` as a WebDAV source in Kodi to browse the full movie list. You can add a category view such as `https://<server>:8096/dav/Movies/Genres/Science%20Fiction/` to browse one genre. Use the matching path for your server's scheme, address, port, collection name, and configured view labels. Sign in with an RCFin account that has access to that library. WebDAV does not modify media files; see the [WebDAV guide](docs/webdav.md) for its full layout and access behavior.

## Features

### Native Rclone support

RCFin includes the Rclone Go SDK. Configure remotes in an Rclone config file, then point each library to a `remote:path`, such as `media:Movies`. RCFin includes the backends compiled into Rclone v1.76.0.

### Direct playback from remote storage

RCFin streams media from Rclone storage and supports HTTP byte ranges. Compatible clients can seek through a file without downloading the entire library first. Playback speed depends on the storage backend and its range-read support.

### Per-library storage settings

Each library has its own Rclone filesystem and VFS settings. Set directory cache time, read chunk size, and local cache limits for each library.

### Native Tailscale support

Enable the Tailscale listener to run RCFin as a tsnet node. Set a hostname and auth key, then keep the state directory on persistent storage so restarts reuse the same node identity. Set its port to `443` and enable HTTPS to use Tailscale's automatic certificate. Tailnet clients can then open the full MagicDNS HTTPS name without adding `:8096`. The private listener bypasses the public IP allow list.

### Headscale support

Set `listen.tailscale.control_url` to join a custom control server such as Headscale. RCFin sets the node hostname. Headscale controls its DNS suffix and device name. Tailscale's automatic HTTPS certificate support may not be available with Headscale; use HTTP over the encrypted tailnet or a TLS proxy with a valid certificate.

### Public HTTP listener

RCFin listens on port `8096` by default. Set a bind address to control which interface accepts public connections.

### TLS certificates

Provide both a certificate and key to enable HTTPS. RCFin does not request or renew certificates automatically.

### Public IP allow list

Restrict the public listener with comma-separated IP addresses or CIDR ranges. The list is an allow list. Tailscale connections use tailnet policy and do not pass through this public IP check.

### Jellyfin client sign-in

Users sign in to compatible Jellyfin clients with accounts managed by RCFin. The API supports authenticated requests for the implemented client workflows.

### Library browsing

Browse configured movie and TV libraries, seasons, episodes, and supported metadata through Jellyfin-compatible endpoints.

### Search

Search supported catalog fields from compatible Jellyfin clients.

### Playback progress

RCFin saves user playback progress and resume state in the configured database.

### Quick Connect

Enable Quick Connect in the configuration for clients that support it. It is disabled by default.

### Movie libraries

Create a collection with type `movies` and set its Rclone remote path. Each collection has a stable ID used for account permissions.

### TV libraries

Create a collection with type `shows`. RCFin recognizes supported season folders and episode filenames. See the [media layout guide](docs/media-layout.md) for examples.

### Account management

Create, disable, enable, delete, and administer accounts with the RCFin command line. There is no public sign-up form. The server does not create a default account.

### Per-library account permissions

Regular accounts have no library access until you grant it. Administrators can access every configured library. Jellyfin routes and WebDAV use the same library grants.

### Read-only WebDAV

Publish virtual library views through WebDAV. Views can include All, Genres, Actors, Directors, Years, and Studios. RCFin can also expose NFO and artwork sidecars. WebDAV does not move or modify media files.

### Automatic library scans

RCFin scans each library on a configurable interval. You can set a different interval for each collection. Rclone's directory cache has a separate expiration setting.

### Inbox upload readiness

Set an Inbox path for new uploads. RCFin excludes it from normal library browsing and can wait for a completion marker or a quiet period before processing an item.

### Automatic metadata fetching

Enable the TMDb provider to fetch movie and TV metadata. Configure the language and overwrite behavior. RCFin preserves existing NFO files by default.

### Artwork downloads

Enable artwork fetching to download posters and backdrops from TMDb. Configure image size limits and overwrite behavior. RCFin preserves existing artwork files by default.

### Automatic subtitle downloads

Enable OpenSubtitles.com to search for and download SRT or VTT sidecars. Choose languages, forced or hearing-impaired tracks, match confidence, and overwrite behavior.

### Inbox organization plans

RCFin can propose movie and TV destinations for ready Inbox items. Organization is disabled by default and uses dry-run mode by default. Review the plan before enabling file moves.

### Existing-library normalization

Preview normalized paths for existing library items outside the Inbox. This workflow has its own enable and dry-run settings.

### SQLite database

Use SQLite for a single RCFin server with persistent local storage. SQLite is the default database.

### PostgreSQL database

Use PostgreSQL when you want application data stored outside the RCFin host or container. Artwork and Rclone VFS caches remain local.

### Artwork cache

RCFin stores resized artwork in a local cache directory. The cache can be rebuilt if it is removed.

### Rclone VFS cache

Rclone can cache the media ranges clients read. Configure cache mode, maximum size, and age for each library. The cache can be rebuilt if it is removed.

## Playback compatibility

RCFin serves original media. It does not transcode, remux, or create HLS streams. The client must support the media's codecs and container. RCFin implements selected Jellyfin-compatible endpoints, not the full Jellyfin server API. Subtitle playback uses sidecar files; embedded subtitle streams are not inspected by subtitle automation.

## Configuration and support

- [Starter configuration](config.example.yaml)
- [Full configuration example](config.example.full.yaml)
- [Configuration reference](docs/configuration.md)
- [Networking and access](docs/networking.md)
- [Rclone storage](docs/rclone-storage.md)
- [VFS options](docs/vfs-options.md)
- [Account administration](docs/accounts.md)
- [WebDAV](docs/webdav.md)
- [Media layout and automation](docs/media-layout.md)
- [Database backends](docs/database.md)
- [Release targets](docs/releases.md)

RCFin is a separately maintained fork of [Jellofin Server](https://github.com/erikbos/jellofin-server). For help or bug reports, use [GitHub Issues](https://github.com/adhikariprivate/rcfin-server/issues). To contribute, read [CONTRIBUTING.md](CONTRIBUTING.md). RCFin is licensed under the [MIT License](LICENSE).
