# Configuration

RCFin reads YAML from `rcfin.yaml` in the current working directory by default. Select another file with `--config /path/to/file.yaml`. Start from the complete [example configuration](../rcfin.example.yaml), then replace its paths and Rclone remote paths with values for your host.

## Complete example

The example file is the canonical full configuration. It includes optional listen/TLS/access-control fields, logging, local database and cache paths, global Rclone settings, Jellyfin settings, two library definitions, and per-library VFS tuning.

```sh
cp rcfin.example.yaml rcfin.yaml
${EDITOR:-vi} rcfin.yaml
./rcfin --config rcfin.yaml
```

Create the parent directories for the SQLite database and local caches before startup, and ensure the RCFin process user can write to them. For example:

```sh
install -d -o rcfin -g rcfin /var/lib/rcfin /var/cache/rcfin/images /var/cache/rcfin/rclone
```

## Rclone config path and library remote

`rclone.config` is the path to the Rclone INI **file**, including its filename. It is not the directory containing the file. For example, if the file on disk is `/home/rcfin/apps/rcfin/rclone.config`, use:

```yaml
rclone:
  config: /home/rcfin/apps/rcfin/rclone.config
  cache_dir: /home/rcfin/apps/rcfin/vfs
```

If `rclone.config` is omitted or empty, the embedded Rclone SDK uses its normal default configuration behavior. The configured file must be readable by the RCFin process and contain a section for each named remote referenced by a library. Keep credentials private and do not paste them into issue reports.

Each collection uses the key `remote`, with a value such as `files:HTPC/Movies`. The part before the colon (`files`) must exactly match a section name such as `[files]` in the Rclone config file. The part after the colon is the path within that remote. A remote can be reused by multiple collections, and different collections can select different remotes. Rclone config and credentials are global; VFS options are per collection.

Example Rclone INI structure (provider options and credentials vary by backend):

```ini
[files]
type = crypt
remote = upstream:/path/to/encrypted/data
# Add the provider-specific and crypt options created for your setup.
```

Use the normal Rclone tooling to create and verify remote sections. RCFin embeds Rclone and does not need a separate Rclone executable or FUSE mount at runtime.

## Configuration fields

| YAML key | Meaning |
| --- | --- |
| `listen.address` | Optional bind address. Empty uses the server's default interface. |
| `listen.port` | HTTP port; defaults to `8096`. |
| `listen.tls_cert`, `listen.tls_key` | Set both paths to enable TLS. |
| `listen.ip_acl` | Optional comma-separated client IP allow list; empty allows all clients. |
| `logfile` | `stdout` (default), `stderr`, `none`, or a writable log file path. |
| `cachedir` | Local artwork resize cache directory. |
| `rescan_interval` | Positive Go duration for periodic library rescans; default `30m`. |
| `database.sqlite.filename` | Local SQLite database file path. |
| `rclone.config` | Optional full path to the Rclone INI config file. |
| `rclone.cache_dir` | Local root directory for VFS cache data. |
| `jellyfin.server_id` | Optional stable server ID. Leave empty to let RCFin generate one. |
| `jellyfin.server_name` | Display name; defaults to `RCFin`. |
| `jellyfin.auto_register` | Whether RCFin automatically registers users. |
| `jellyfin.quick_connect` | Whether Quick Connect is enabled. |
| `jellyfin.image_quality_poster` | Poster image quality setting. |
| `collections[].id` | Optional stable collection ID. |
| `collections[].name` | Collection display name. |
| `collections[].type` | `movies` or `shows`. |
| `collections[].remote` | Rclone remote path, e.g. `files:HTPC/Movies`. This key is named `remote`, not `directory`. |
| `collections[].vfs` | Per-collection Rclone VFS options. See the [VFS reference](vfs-options.md). |

The SQLite database, artwork cache, and Rclone VFS cache are local data. The media itself is accessed through Rclone. The selected VFS cache mode and limits determine how much local disk can be used for media reads.

For compatibility, existing configs may continue using the compact Jellyfin keys `servername`, `autoregister`, and `imagequalityposter`. New configs should use `server_name`, `auto_register`, and `image_quality_poster` as shown in the example.

## Troubleshooting

- `rclone remote path is required`: a collection is missing `remote` or has an empty value. `directory` is not the supported YAML key.
- `didn't find section in config file ("files")`: verify `rclone.config` points to the actual readable INI file, including its filename, and that the file has a `[files]` section. A directory path or a differently named file will not load it.
- RCFin `v1.0.0` did not install Rclone's file-backed config loader, so custom config files were ignored even with the correct path. Upgrade to a build containing the embedded config loader fix.
- `remote not found in config`: check that the remote name before `:` matches the INI section name exactly.
- Log file cannot be opened: select `stdout` or `stderr`, or ensure the RCFin user can create/append the configured file.
- Library changes appear late: reduce `rescan_interval` and/or `dir_cache_time`; enable `poll_interval` only when the backend supports polling.
- Seeks are slow: check backend range-read support and tune the collection's read-chunk and cache settings.
