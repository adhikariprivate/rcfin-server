# Configuration

RCFin reads one YAML configuration file, `rcfin.yaml` by default. See [`rcfin.example.yaml`](../rcfin.example.yaml) for a working template.

## Global Rclone settings

```yaml
rclone:
  config: /etc/rcfin/rclone.conf
  cache_dir: /var/cache/rcfin/rclone
```

`rclone.config` is the path to the normal Rclone INI config file. If omitted, Rclone uses its standard config path. Store backend credentials and provider settings in that file; do not duplicate credentials in each library block. `rclone.cache_dir` selects the local root used by Rclone's VFS file cache. If omitted, Rclone's own default cache directory is used.

Rclone config is shared by the process. Each library's `directory` chooses a remote and root within that global config, while that library's `vfs` block controls a separate VFS instance.

## Libraries

Each `collections` entry has:

| Key | Meaning |
| --- | --- |
| `id` | Optional stable library ID. |
| `name` | Display name. |
| `type` | `movies` or `shows`. |
| `directory` | Rclone path, such as `films:Movies` or `archive:TV/Series`. |
| `vfs` | Per-library Rclone VFS settings. See the [full reference](vfs-options.md). |
| `hlsserver` | Optional external HLS URL retained for existing deployments. |

A library may target a different configured Rclone remote from every other library. It may also use a different folder under the same remote. Configure a backend once in the global Rclone INI file, then refer to it by remote name in `directory`.

## Local application data

`database.sqlite.filename` is a local SQLite file. `cachedir` is a separate local cache for resized artwork. Rclone's `cache_dir` is only for Rclone VFS file caching. Keep these on local storage with enough free space for the policies selected in each library's VFS options.

`rescan_interval` controls the periodic collection/index refresh. It accepts Go duration syntax, defaults to `30m`, and must be positive. VFS directory entries are cached separately by `dir_cache_time`.

## Example Rclone config

The global file follows normal Rclone syntax. The following illustrates distinct remotes; use each backend's own setup process to create real credentials and options:

```ini
[films]
type = drive
# backend-specific credentials and options are created by rclone config

[tv]
type = s3
# backend-specific credentials and options are created by rclone config
```

The snippets are illustrative only; actual provider options vary by backend. Keep the file private because it can contain tokens and secrets.
