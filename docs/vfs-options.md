# Rclone VFS options

Place VFS options inside each collection's `vfs` map. Keys use Rclone config names (underscores, not CLI hyphens). Values use normal Rclone syntax: Go durations such as `30m`, byte sizes such as `32M` or `2G`, booleans, integers, and enum strings. Invalid values and unknown keys fail startup so misspelled settings do not silently fall back. Quote octal permission values so YAML preserves them as octal strings, for example `file_perms: "0644"` and `umask: "0022"`.

```yaml
collections:
  - name: Films
    type: movies
    remote: drive:Films
    vfs:
      dir_cache_time: 45m
      vfs_cache_mode: full
      vfs_read_chunk_size: 32M
```

Options below are the options exposed by Rclone v1.76.0's `vfscommon.OptionsInfo`. RCFin's media-oriented defaults are shown in **RCFin default**; where shown as “Rclone default”, the value comes from the pinned SDK on the current platform.

| Config key | Type / values | RCFin default | Purpose |
| --- | --- | --- | --- |
| `no_modtime` | boolean | `false` | Avoid modification-time reads/writes. |
| `no_checksum` | boolean | `false` | Disable checksum comparison. |
| `no_seek` | boolean | `false` | Disable VFS seeking; generally unsuitable for media clients. |
| `dir_cache_time` | duration | `30m` | How long directory entries stay cached. |
| `vfs_refresh` | boolean | `false` | Recursively refresh VFS directory cache at startup. |
| `poll_interval` | duration | `0s` | Poll remote changes where supported; zero disables polling. |
| `read_only` | boolean | `true` | Prevent writes through this VFS. |
| `vfs_links` | boolean | `false` | Interpret Rclone link files. |
| `vfs_cache_mode` | `off`, `minimal`, `writes`, `full` | `full` | Cache read ranges sparsely on local disk for client seeks. |
| `vfs_cache_poll_interval` | duration | Rclone default (`1m`) | Interval for stale cache cleanup. |
| `vfs_cache_max_age` | duration | `24h` | Maximum age for cached files. |
| `vfs_cache_max_size` | size | `20GiB` | Maximum total VFS cache size. |
| `vfs_cache_min_free_space` | size | Rclone default (`off`) | Target free space to preserve on the cache disk. |
| `vfs_read_chunk_size` | size | `32M` | Initial remote read chunk. |
| `vfs_read_chunk_size_limit` | size or `off` | `256M` | Upper bound for automatic read-chunk growth. |
| `vfs_read_chunk_streams` | integer | Rclone default (`0`) | Parallel read streams. |
| `dir_perms` | octal mode | Rclone default | VFS directory mode. |
| `file_perms` | octal mode | Rclone default | VFS file mode. |
| `link_perms` | octal mode | Rclone default | VFS link mode. |
| `vfs_case_insensitive` | boolean | platform default | Case-insensitive file lookup. |
| `vfs_block_norm_dupes` | boolean | `false` | Hide duplicate names after Unicode normalization. |
| `vfs_write_wait` | duration | Rclone default (`1s`) | Wait for an in-sequence write. |
| `vfs_read_wait` | duration | Rclone default (`20ms`) | Wait for an in-sequence read before seeking. |
| `vfs_write_back` | duration | Rclone default (`5s`) | Delay writes back when cache mode permits writes. |
| `vfs_read_ahead` | size | Rclone default (`0`) | Read-ahead in full cache mode. |
| `vfs_used_is_size` | boolean | `false` | Use `rclone size`-style usage calculation. |
| `vfs_fast_fingerprint` | boolean | `false` | Use faster, less exact change fingerprints. |
| `vfs_disk_space_total_size` | size or `off` | Rclone default (`off`) | Report a configured total disk size. |
| `umask` | octal mode | process default | Mask file and directory permissions. |
| `uid` | integer | process default | Override VFS UID where supported. |
| `gid` | integer | process default | Override VFS GID where supported. |
| `vfs_handle_caching` | duration | `1m` | Keep handles/downloaders alive after close. |
| `vfs_metadata_extension` | string | empty | Extension used for VFS metadata responses. |

`rclone.cache_dir` is global, outside the per-library `vfs` map. Rclone places each VFS's cache under that local root. Cache modes that retain full media reads can use substantial local disk; set a maximum size and age when enabling them.
