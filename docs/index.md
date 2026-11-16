# RCFin documentation

- [Configuration](configuration.md): exhaustive application settings, defaults, environment variables, and validation notes.
- [Networking and access](networking.md): public listener, IP allow list, TLS, Tailscale/tsnet, Headscale, and DNS.
- [Database backends](database.md): SQLite and PostgreSQL setup, durable state, local caches, playback write behavior, and container deployment.
- [Account administration](accounts.md): CLI user management, password changes, stable library IDs, access grants, and upgrade behavior.
- [Virtual WebDAV](webdav.md): read-only virtual library views, Basic authentication, per-library access control, NFO/artwork sidecars, and configuration.
- [Media layout](media-layout.md): supported movie and TV folders, filenames, NFO sidecars, subtitles, and artwork.
- [Rclone storage](rclone-storage.md): remote setup, per-library VFS instances, streaming and seek behavior, caching, and troubleshooting.
- [VFS options reference](vfs-options.md): every Rclone VFS option accepted under each library's `vfs` block.
- [Architecture](architecture.md): the scan, metadata, image, and playback paths through the embedded Rclone SDK.

RCFin embeds the Rclone Go SDK at **v1.76.0**. Every backend compiled into that Rclone release is registered in the RCFin binary. The backend is initialized from the global Rclone config file; no Rclone executable, FUSE mount, or separately running storage process is needed.

Start with the [README](../README.md) for the product overview, [the starter config](../config.example.yaml) for a concise setup, or [the full config](../config.example.full.yaml) for every setting in one annotated YAML file.
