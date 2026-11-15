# Storage architecture

The executable lives in `cmd/rcfin-server`. Application packages live under `internal/`, where Go restricts their use to this module. RCFin does not currently expose a public Go library package.

RCFin uses the Rclone Go SDK rather than shelling out to `rclone` or mounting a filesystem.

1. Startup configures Rclone's global config-file path and local cache root.
2. For each collection, RCFin calls `fs.NewFs` with that collection's Rclone path and creates a dedicated `vfs.VFS` with the collection's VFS options.
3. The collection scanner calls VFS `Stat`/`ReadDirAll` to enumerate movie/show/season folders. Rclone directory caching reduces repeat remote listings.
4. NFO sidecars are opened from that same VFS and parsed in-process. The search index and loaded collection objects are rebuilt in memory; durable user and library state is stored in the selected SQLite or PostgreSQL database.
5. Artwork and video HTTP handlers open files through the library VFS. The VFS handle provides `Read`, `ReadAt`, and `Seek`, so HTTP range responses support media playback and seeking without a FUSE mount.
6. Artwork resize outputs and Rclone's VFS cache remain local and can be discarded. Uploaded images and account/playback state are stored in the selected database. Rclone backend registrations are linked into the RCFin binary at build time.

When PostgreSQL is selected, the container can use ephemeral local storage for these caches, while the database remains external. Playback updates are buffered in process and flushed every 10 seconds; see [Database backends](database.md) for the resulting crash-loss window and the current single-instance limitation.

Each library has one inventory cadence: `collections[].media.scan_interval`, falling back to the global `scan_interval` (default 30 minutes). A cycle lists the playable library and `.Inbox`, reconciles the complete snapshot against durable inventory, then handles ready Inbox provisioning, eligible metadata, and subtitles in that order. The stages consume the same snapshot; metadata and subtitles do not start separate remote scans. VFS directory cache lifetime and optional Rclone change polling remain independently configurable because they control listing freshness, not media-work scheduling. A failed/incomplete listing does not commit a scan generation.

The first successful inventory establishes a baseline for existing library files. Later additions and changed fingerprints become automatic enrichment candidates; confirmed moves preserve prior processing state, and absent paths remain missing candidates through a grace period. Inbox files remain pending until their completion marker or configured stable-tree interval proves readiness. Full inventory scans and explicit metadata/subtitle refreshes are separate CLI operations (`rcfin library scan`, `rcfin library refresh ...`); unchanged items do not invoke providers on every cycle.

Each library's VFS is set read-only by default. RCFin does not expose file-management or upload operations on media storage.
