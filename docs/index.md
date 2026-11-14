# RCFin documentation

- [Configuration](configuration.md): server settings, global Rclone config, libraries, and VFS option syntax.
- [Media layout](media-layout.md): supported movie and TV folders, filenames, NFO sidecars, subtitles, and artwork.
- [Rclone storage](rclone-storage.md): remote setup, per-library VFS instances, streaming and seek behavior, caching, and troubleshooting.
- [VFS options reference](vfs-options.md): every Rclone VFS option accepted under each library's `vfs` block.
- [Architecture](architecture.md): the scan, metadata, image, and playback paths through the embedded Rclone SDK.

RCFin embeds the Rclone Go SDK at **v1.76.0**. Every backend compiled into that Rclone release is registered in the RCFin binary. The backend is initialized from the global Rclone config file; no Rclone executable, FUSE mount, or separately running storage process is needed.
