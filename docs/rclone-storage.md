# Rclone-backed media storage

RCFin embeds Rclone v1.76.0 and imports Rclone's `backend/all` registry. The resulting binary supports the upstreams included in that release, with no external `rclone` process or FUSE mount. Rclone config remains global; credentials and backend-specific settings are configured once in the Rclone config file.

## One remote per library

Use an ordinary Rclone remote path as the library `remote`:

```yaml
rclone:
  config: /etc/rcfin/rclone.conf
  cache_dir: /var/cache/rcfin/rclone

collections:
  - id: films
    name: Films
    type: movies
    remote: drive:Media/Films
    vfs:
      dir_cache_time: 30m
      vfs_cache_mode: full
      vfs_cache_max_size: 20GiB
      vfs_cache_max_age: 24h
  - id: series
    name: Series
    type: shows
    remote: s3archive:video/Series
    vfs:
      dir_cache_time: 1h
      vfs_cache_mode: full
      vfs_cache_max_size: 20GiB
      vfs_cache_max_age: 24h
```

Each library creates its own `fs.Fs` and VFS rooted at the configured remote path. VFS settings are isolated by library, so a slow object store can use a different directory cache, read chunk size, or local cache policy from a low-latency drive remote. The global config still owns the remote definitions and authentication.

For inventory, RCFin uses the backend's advertised recursive `ListR` operation when available and otherwise walks directories through the VFS. `rcfin library scan` and `rcfin library status` report the listing method, fingerprint mode, and how many entries included a backend object ID. IDs are retained for diagnostics but are excluded from routine change and rename fingerprints because their stability varies by backend. Same-path detection uses relative paths, object roles, sizes, and modification times; it does not read full media contents to generate routine checksums. A standalone file move is inferred only when those attributes also have matching parsed identity evidence. Folder moves use a unique relative subtree manifest. If evidence is insufficient or ambiguous, use `rcfin library item adopt-move` after reviewing the scan. A replacement that preserves all observed attributes can require an explicit administrator refresh/recheck.

The part before `:` must match a section in the Rclone config file. Use `remote:path` syntax even for the Rclone local backend, for example `local:/srv/media/Films`. Relative paths passed to Rclone's local backend also work when supported by Rclone. The YAML key is `remote`; `directory` is not recognized.

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
- `scan_interval: 30m` (global default, optionally overridden by `collections[].media.scan_interval`) controls the single per-library inventory and media-work cycle independently of VFS directory caching.

For providers that support efficient range reads and deployments prioritizing disk conservation, `vfs_cache_mode: off` streams ranges directly and avoids read caching. `minimal` only adds caching for reads opened for both reading and writing; it does not cache ordinary read-only media playback. `full` caches the ranges actually read as sparse files and is the recommended default for varied Jellyfin clients and remotes. Configure `vfs_cache_max_size` and `vfs_cache_max_age` to bound local disk use.

Rclone may use backend-specific chunks/ranges underneath the VFS. VFS chunk options control Rclone's read stream; they do not change the media files or enable transcoding.

## Version and upstream coverage

The SDK version is pinned at Rclone v1.76.0. RCFin registers all backends compiled into that upstream release. Provider authentication, optional features, listing semantics, and range-read efficiency still vary by backend. A registered backend does not guarantee every provider feature is supported by every remote account.

RCFin serves original files. It does not transcode, remux, or create HLS output.

## Troubleshooting

- `didn't find section in config file`: check that `rclone.config` points to the actual INI file including its filename, then verify the file contains a section matching the part before `:` in the collection's `remote` value.
- `remote not found in config`: verify that the section name before `:` matches exactly.
- Backend credentials or token refresh failures: repair/refresh credentials with normal Rclone tools, then restart RCFin so it constructs fresh remotes.
- Library changes appear late: lower `scan_interval` and/or `dir_cache_time`; enable `poll_interval` only if the backend supports it.
- Seeks are slow: check the backend's range-read support, then tune `vfs_read_chunk_size`, `vfs_read_chunk_size_limit`, and `vfs_cache_mode` for that library.
- Cache disk fills: use `off`, or set per-library `vfs_cache_max_size`/`vfs_cache_max_age` and a suitable global `rclone.cache_dir`.
