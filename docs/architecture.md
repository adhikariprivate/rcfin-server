# Storage architecture

The executable lives in `cmd/rcfin-server`. Application packages live under `internal/`, where Go restricts their use to this module. RCFin does not currently expose a public Go library package.

RCFin uses the Rclone Go SDK rather than shelling out to `rclone` or mounting a filesystem.

1. Startup configures Rclone's global config-file path and local cache root.
2. For each collection, RCFin calls `fs.NewFs` with that collection's Rclone path and creates a dedicated `vfs.VFS` with the collection's VFS options.
3. The collection scanner calls VFS `Stat`/`ReadDirAll` to enumerate movie/show/season folders. Rclone directory caching reduces repeat remote listings.
4. NFO sidecars are opened from that same VFS and parsed in-process. Parsed library objects and user state remain in RCFin's local process/SQLite database.
5. Artwork and video HTTP handlers open files through the library VFS. The VFS handle provides `Read`, `ReadAt`, and `Seek`, so HTTP range responses support media playback and seeking without a FUSE mount.
6. Artwork resize outputs, SQLite, and Rclone's VFS cache remain local. Rclone backend registrations are linked into the RCFin binary at build time.

The application rescans collections every `rescan_interval` (default 30 minutes) instead of continuously. VFS directory cache lifetime and optional Rclone change polling are independently configurable per library. This avoids continuous full-library listing against cloud APIs while allowing short refresh intervals when a deployment needs them.

Each library's VFS is set read-only by default. RCFin does not expose file-management or upload operations on media storage.
