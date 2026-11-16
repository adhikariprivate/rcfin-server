# RCFin

**A Jellyfin-compatible server for media libraries stored on Rclone remotes.** RCFin connects directly to cloud and remote storage through the embedded Rclone Go SDK, then serves your movies and shows to compatible Jellyfin clients. No FUSE mount or separate Rclone daemon is required.

RCFin is designed for people who want to keep media in the storage they already use, control which accounts can see each library, and serve original media without running a transcoding stack.

## Why RCFin

- **Use Rclone remotes directly.** Connect libraries to separately configured Rclone backends from one process. Keep each library's listing, read, and disk-cache settings independent.
- **Stream from remote storage.** Playback uses seekable reads and HTTP range requests, so supported clients can seek without first copying the whole library locally. Backend range-read performance varies.
- **Choose how clients connect.** Serve through a normal HTTP/HTTPS endpoint, an optional private Tailscale network listener, or both. The public IP allow list applies only to the public endpoint; tailnet access is governed by tailnet policy.
- **Give accounts access to selected libraries.** Create accounts from the CLI and grant access by stable library ID. There is no public registration endpoint.
- **Browse with familiar Jellyfin clients.** RCFin implements the Jellyfin-compatible workflows needed for authentication, browsing, search, metadata, artwork, playback state, direct streaming, and external subtitles. Fladder and Swiftfin have been tested; compatibility can vary by client and version.
- **Offer a read-only WebDAV view.** Expose virtual library views with account authentication, per-library permissions, and optional NFO/artwork sidecars. The view maps to current Rclone paths and does not reorganize files.
- **Automate careful media intake.** An excluded Inbox can wait for uploads to finish, inspect items, find metadata or subtitles, and propose normalized paths. Organization is opt-in, starts in dry-run mode, and leaves ambiguous items for review.
- **Keep application state where it fits.** Use local SQLite for a simple single-instance setup or PostgreSQL for durable state outside a container. Artwork and Rclone caches remain local and rebuildable.

## Playback and media behavior

RCFin serves original media files. **It does not transcode, remux, or generate HLS.** A client must support the media's codecs and container for direct playback. External SRT and VTT sidecars are advertised to compatible Jellyfin clients; RCFin does not inspect embedded subtitle streams for automatic subtitle acquisition.

Metadata and artwork enrichment use TMDb. Subtitle search and downloads use OpenSubtitles.com. Both are optional and disabled until configured. Provider credentials are supplied through environment variables or an optional local `.env` file, not committed in YAML. Existing sidecars are preserved unless replacement is explicitly configured.

## Get started

1. Obtain or build the RCFin binary for your platform.
2. Create an Rclone config file containing each remote you plan to use.
3. Copy the starter config and set the remote paths and persistent local directories:

   ```sh
   cp config.example.yaml rcfin.yaml
   ```

4. Start RCFin and create an administrator account:

   ```sh
   ./rcfin --config rcfin.yaml
   ./rcfin --config rcfin.yaml user add --username admin --admin
   ```

5. Add RCFin to a compatible Jellyfin client using the server address and sign in with that account.

The default public HTTP port is `8096`. For a more complete annotated configuration with all available options, start from [`config.example.full.yaml`](config.example.full.yaml). See the [configuration reference](docs/configuration.md) and [Rclone storage guide](docs/rclone-storage.md) before exposing a server.

## Connect clients privately or publicly

The public listener can be bound to a chosen address and protected by TLS and an IP allow list. Tailscale support is optional: RCFin can register as a persistent tsnet node with a configured hostname and auth key, using either Tailscale or a custom Headscale control server. The state directory must survive restarts. Tailnet ACLs/grants remain the access boundary for the private listener; RCFin account authentication and library permissions still apply to application routes.

See [networking and access](docs/networking.md) for listener behavior, TLS, IP ACL syntax, Tailscale setup, Headscale, MagicDNS, and deployment notes.

## Media organization, with review before writes

Each collection can have an Inbox path excluded from normal playback browsing. RCFin can wait for a completion marker or a quiet period, inventory media and sidecars, show provider-match evidence, and produce a destination plan. The preview commands are read-only. Applying organization requires explicit configuration and confirmation; conflicts, unknown files, and uncertain identities are surfaced for operator review.

Start with the [media automation guide](docs/media-layout.md), the [supported layout reference](docs/media-layout.md#supported-library-layouts), and the [fixture outcomes](docs/media-fixtures.md). Organization and provider writes require writable VFS settings even though collections default to read-only.

## Documentation

- [Configuration reference](docs/configuration.md) — every application configuration field, defaults, environment variables, and validation notes.
- [Networking and access](docs/networking.md) — public listener, ACL, TLS, Tailscale/tsnet, and Headscale.
- [Rclone storage](docs/rclone-storage.md) — remote definitions, storage behavior, streaming, and cache tuning.
- [VFS options](docs/vfs-options.md) — every accepted per-library Rclone VFS option.
- [Accounts](docs/accounts.md) — users, passwords, roles, grants, and CLI management.
- [WebDAV](docs/webdav.md) — read-only virtual views, authentication, sidecars, and paths.
- [Media layout and automation](docs/media-layout.md) — Inbox, inventory, metadata, subtitles, organization, and CLI workflows.
- [Database](docs/database.md) — SQLite, PostgreSQL, persistent state, and container notes.
- [Architecture](docs/architecture.md) — how inventory, metadata, playback, and storage fit together.
- [Documentation index](docs/index.md) — all guides.

## Build from source

The repository uses [mise](https://mise.jdx.dev/) to select the Go toolchain and tasks:

```sh
mise install
mise run build
mise run test
```

RCFin is a separately maintained fork of [Jellofin Server](https://github.com/erikbos/jellofin-server) and is licensed under the MIT License. It is not a full Jellyfin server replacement; its Jellyfin API coverage is focused on supported client workflows.
