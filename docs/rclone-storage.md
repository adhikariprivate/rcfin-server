# Rclone-backed media storage

RCFin embeds Rclone v1.76.0 and imports Rclone's `backend/all` registry. The resulting binary supports the upstreams included in that release, with no external `rclone` process or FUSE mount. Rclone config remains global; credentials and backend-specific settings are configured once in the Rclone config file.

## One remote per library

Use an ordinary Rclone remote path as the library `directory`:

```yaml
rclone:
  config: /etc/rcfin/rclone.conf
  cache_dir: /var/cache/rcfin/rclone

collections:
  - id: films
    name: Films
    type: movies
    directory: drive:Media/Films
    vfs:
      dir_cache_time: 30m
      vfs_cache_mode: full
      vfs_cache_max_size: 20GiB
      vfs_cache_max_age: 24h
  - id: series
    name: Series
    type: shows
    directory: s3archive:video/Series
    vfs:
      dir_cache_time: 1h
      vfs_cache_mode: full
      vfs_cache_max_size: 20GiB
      vfs_cache_max_age: 24h
```

Each library creates its own `fs.Fs` and VFS rooted at the configured remote path. VFS settings are isolated by library, so a slow object store can use a different directory cache, read chunk size, or local cache policy from a low-latency drive remote. The global config still owns the remote definitions and authentication.

Use `remote:path` syntax even for the Rclone local backend, for example `local:/srv/media/Films`. Relative paths passed to Rclone's local backend also work when supported by Rclone.

## How clients access media

Library scans list the collection root, movie/show directories, and season directories through that library's VFS. NFO files are read through the VFS and parsed in-process. Artwork and original video files are opened through the same VFS. The video handler uses Rclone VFS file handles, which implement seeking and support HTTP range requests used by Jellyfin clients for playback and seeking.

RCFin does not mount the VFS into the operating system. The VFS is an in-process layer over Rclone `fs.Fs` and supplies Rclone's directory/file caching and seek behavior to the existing Jellyfin handlers.

## Client-oriented defaults

RCFin starts each VFS with these defaults; a library's `vfs` map overrides any of them:

- `dir_cache_time: 30m` keeps repeated browse/list requests from re-listing cloud directories on every client request.
- `poll_interval: 0s` avoids background polling traffic by default. Use polling only for a backend that supports change notifications and where near-real-time library refresh is needed.
- `read_only: true` prevents writes to media backends.
- `vfs_cache_mode: full` stores the ranges clients read as sparse local files, so repeated reads and seeks can use the local cache without requiring the backend to provide filesystem-like seek semantics. It does not eagerly download an entire movie when playback starts.
- `vfs_cache_max_size: 20GiB` and `vfs_cache_max_age: 24h` bound the default per-library cache.
- `vfs_read_chunk_size: 32M` and `vfs_read_chunk_size_limit: 256M` let sequential playback grow read chunks while placing a bound on speculative reads.
- `vfs_handle_caching: 1m` reuses active remote handles briefly across adjacent range requests.
- `rescan_interval: 30m` controls the application-level library scan independently of VFS directory caching.

For providers that support efficient range reads and deployments prioritizing disk conservation, `vfs_cache_mode: off` streams ranges directly and avoids read caching. `minimal` only adds caching for reads opened for both reading and writing; it does not cache ordinary read-only media playback. `full` caches the ranges actually read as sparse files and is the recommended default for varied Jellyfin clients and remotes. Configure `vfs_cache_max_size` and `vfs_cache_max_age` to bound local disk use.

Rclone may use backend-specific chunks/ranges underneath the VFS. VFS chunk options control Rclone's read stream; they do not change the media files or enable transcoding.

## Version and upstream coverage

The SDK version is pinned at Rclone v1.76.0. RCFin registers all backends compiled into that upstream release. Provider authentication, optional features, listing semantics, and range-read efficiency still vary by backend. A registered backend does not guarantee every provider feature is supported by every remote account.

RCFin serves original files. It does not transcode, remux, or create HLS output.

## Troubleshooting

- `remote not found in config`: check that `rclone.config` points to the intended INI file and that the section name before `:` matches exactly.
- Backend credentials or token refresh failures: repair/refresh credentials with normal Rclone tools, then restart RCFin so it constructs fresh remotes.
- Library changes appear late: lower `rescan_interval` and/or `dir_cache_time`; enable `poll_interval` only if the backend supports it.
- Seeks are slow: check the backend's range-read support, then tune `vfs_read_chunk_size`, `vfs_read_chunk_size_limit`, and `vfs_cache_mode` for that library.
- Cache disk fills: use `off`, or set per-library `vfs_cache_max_size`/`vfs_cache_max_age` and a suitable global `rclone.cache_dir`.
