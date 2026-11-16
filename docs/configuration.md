# Configuration reference

RCFin reads a YAML file from `rcfin.yaml` in the working directory unless `--config PATH` is supplied. The complete field-by-field example is [`config.example.full.yaml`](../config.example.full.yaml); [`config.example.yaml`](../config.example.yaml) is a shorter starting point. Configuration is unmarshaled by Viper. Unknown non-VFS YAML keys are not guaranteed to fail startup, so use the names in this reference.

```sh
cp config.example.yaml rcfin.yaml
${EDITOR:-vi} rcfin.yaml
./rcfin --config rcfin.yaml
```

Create the parent directories and ensure the service account can read or write them as appropriate. Keep credentials in environment variables, a secret manager, or a protected `.env` file; do not commit secrets in YAML.

Application duration fields use Go duration syntax (`10s`, `5m`, `24h`). Use hours for day-scale intervals (`168h` for seven days); a `d` unit is not accepted by Go's duration parser. Rclone VFS values follow Rclone's own syntax, documented in the [VFS reference](vfs-options.md).

## Top-level settings

| YAML key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `listen` | object | — | Public listener, access rules, and optional Tailscale listener. |
| `logfile` | string | `stdout` | `stdout`, `stderr`, `none`, or a writable file path. |
| `cachedir` | path | empty | Local artwork-resize cache root. |
| `scan_interval` | duration | `30m` | Default interval for each collection inventory and media-work cycle. Must be positive when set. |
| `rescan_interval` | duration | unused | Legacy alias used only when `scan_interval` is empty. New configs should use `scan_interval`. |
| `database` | object | SQLite | Durable account and application state backend. |
| `rclone` | object | Rclone defaults | Global Rclone config file and local VFS cache root. |
| `jellyfin` | object | built-in defaults | Jellyfin-compatible server identity and selected client behavior. |
| `webdav` | object | disabled | Global read-only WebDAV endpoint and directory cache. |
| `collections` | list | required for libraries | Library definitions. Each has an independent Rclone filesystem/VFS. |

## Listener and network settings

See [Networking and access](networking.md) for address binding, TLS, IP allow-list syntax, Tailscale/Headscale setup, MagicDNS, and security boundaries.

| YAML key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `listen.address` | string | empty | Public bind address. Empty lets Go bind its default wildcard address; use `127.0.0.1` to bind locally behind a proxy. |
| `listen.port` | string | `8096` | TCP port shared by the public listener and optional Tailscale listener. |
| `listen.tls_cert` | path | empty | TLS certificate. HTTPS is enabled only when both certificate and key are set. |
| `listen.tls_key` | path | empty | TLS private key. |
| `listen.ip_acl` | comma-separated IP/CIDR list | empty | Public-listener allow list. Empty allows all source IPs. Tailscale requests bypass this ACL. |
| `listen.tailscale.enabled` | boolean | `false` | Start an additional private tsnet listener. |
| `listen.tailscale.hostname` | string | — | Required when enabled. Device hostname presented to the control server (for example, `handler`). |
| `listen.tailscale.state_dir` | path | — | Required when enabled. Persistent writable directory for the node identity and state. |
| `listen.tailscale.auth_key_env` | environment variable name | `RCFIN_TAILSCALE_AUTH_KEY` | Environment variable containing the enrollment auth key. When Tailscale is enabled, the selected variable must be non-empty. |
| `listen.tailscale.control_url` | URL | empty | Optional custom control server URL, such as `https://headscale.example.com`; empty uses the default Tailscale control server. |

## Logging and caches

| YAML key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `logfile` | string | `stdout` | Log destination. File paths are opened for append/create. |
| `cachedir` | path | empty | Local resized-artwork cache. Rebuildable; the process must be able to create/write it. |
| `rclone.cache_dir` | path | Rclone default | Root for local per-library VFS caches, including cached media ranges. |

## Database

| YAML key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `database.driver` | `sqlite` or `postgres` | `sqlite` | Selects the durable state backend. |
| `database.sqlite.filename` | path | required when driver is `sqlite` | SQLite database file. Its parent directory must exist or be creatable by the process. |
| `database.postgres.url` | URL | empty | PostgreSQL connection URL. When empty, `RCFIN_POSTGRES_URL` is used. |

See [Database backends](database.md) for what is persisted, container volumes, backup guidance, and limitations when changing drivers.

## Rclone

| YAML key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `rclone.config` | path | Rclone SDK default | Full path to the Rclone INI file, including its filename. It must contain a section for every named remote used by a collection. |
| `rclone.cache_dir` | path | Rclone default | Local root for VFS cache data. |

The collection `remote` uses `section:path`, such as `media:Movies`. RCFin embeds Rclone v1.76.0 and registers its compiled backends; it does not shell out to an `rclone` command or require a FUSE mount. See [Rclone storage](rclone-storage.md).

## Jellyfin-compatible behavior

| YAML key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `jellyfin.server_id` | string | generated | Optional stable server ID. Set it if the server identity must survive rebuilding or moving the deployment. |
| `jellyfin.server_name` | string | `RCFin` | Display name presented to clients. |
| `jellyfin.quick_connect` | boolean | `false` | Enables the Quick Connect workflow where supported. |
| `jellyfin.image_quality_poster` | integer | `40` | Poster image output quality setting. |

Older configurations may use `jellyfin.servername` and `jellyfin.imagequalityposter`; new files should use the underscored names above.

## WebDAV

| YAML key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `webdav.enabled` | boolean | `false` | Enables the global read-only virtual DAV endpoint. |
| `webdav.path` | URL path | `/dav/` | Absolute non-root endpoint path, without dot segments. |
| `webdav.cache.directory_ttl` | duration | `1m` | Lifetime of cached virtual directory trees. |

See [WebDAV](webdav.md) for behavior and per-collection views and labels.

## Collections

Each collection is one Jellyfin-visible library backed by an independent Rclone filesystem and VFS.

| YAML key | Type | Required/default | Meaning |
| --- | --- | --- | --- |
| `collections[].id` | string | required | Stable ID using letters, digits, `_` or `-`. Account grants use this ID; keep it stable when renaming the display name. |
| `collections[].name` | string | required | Display name. |
| `collections[].type` | enum | required | `movies` or `shows`. |
| `collections[].remote` | Rclone path | required | Configured remote and path, e.g. `media:Films`. |
| `collections[].vfs` | map | Rclone defaults plus RCFin overrides | Per-library VFS options. See [all accepted VFS keys and types](vfs-options.md). |
| `collections[].media` | object | optional | Inventory cadence, Inbox policy, and optional processing stages. |
| `collections[].webdav` | object | optional | Per-library WebDAV presentation options; only used when global WebDAV is enabled. |

### Collection media and Inbox

| YAML key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `collections[].media.scan_interval` | duration | global `scan_interval` | Per-library interval overriding the global inventory/media-work cycle. |
| `collections[].media.missing_grace` | duration | `168h` (7 days) | How long a missing item remains in inventory before its tombstone expires. |
| `collections[].media.inbox_path` | relative path | `.Inbox` | Path under the library root excluded from normal playback inventory and used for intake. |
| `collections[].media.inbox.stable_for` | duration | `10m` | Quiet interval used to decide an Inbox tree is complete when no marker is configured. |
| `collections[].media.inbox.completion_marker` | filename | empty | Optional file created only after upload completion. |
| `collections[].media.max_parallel_jobs` | integer | `1` | Maximum simultaneous media jobs per library; zero uses one. Accepted range is `0..32`. |

### Existing-path normalization

| YAML key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `collections[].media.normalize_existing.enabled` | boolean | `false` | Enable the normalization workflow for existing paths outside Inbox. |
| `collections[].media.normalize_existing.dry_run` | boolean | `true` | Keep normalization plans non-writing. Writes require explicit dry-run disablement and CLI apply confirmation. |

### Organization

| YAML key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `collections[].media.organize.enabled` | boolean | `false` | Allow automatic Inbox organization during the library cycle. |
| `collections[].media.organize.dry_run` | boolean | `true` | Produce plans without moves by default. |
| `collections[].media.organize.disc_folders` | boolean | `false` | Permit supported disc-folder movie items in organization. |
| `collections[].media.organize.preserve_grouping_directories` | boolean | `true` | Retain recognized parent grouping directories when constructing destinations. |
| `collections[].media.organize.conflict_policy` | string | `skip` | Destination collision behavior; `skip` leaves conflicts for review. |
| `collections[].media.organize.allow_cross_backend_copy_delete` | boolean | `false` | Reserved safety switch. Cross-backend copy/delete organization is currently unsupported; setting this to `true` fails config validation. |
| `collections[].media.organize.include_provider_id` | boolean | `true` | Include provider IDs in generated folder names when a reliable ID exists. |
| `collections[].media.organize.minimum_confidence` | float | `0.82` effective provider threshold when unset/zero | Minimum provider identity confidence for automated organization; range `0..1`. |
| `collections[].media.organize.folder_format` | template | built-in | Movie folder naming template. |
| `collections[].media.organize.file_format` | template | built-in | Movie filename template. |
| `collections[].media.organize.series_format` | template | built-in | TV series folder template. |
| `collections[].media.organize.season_format` | template | built-in | Season folder template. |
| `collections[].media.organize.episode_format` | template | built-in | Episode filename template. |
| `collections[].media.organize.episode_fallback_languages` | language list | empty | Preferred fallback languages when provider episode titles are unavailable. |
| `collections[].media.organize.allowed_extensions` | extension list | built-in video extensions | Video extensions eligible for organization. |
| `collections[].media.organize.stable_for` | duration | Inbox stable interval | Quiet period required before an Inbox item can be moved. |
| `collections[].media.organize.completion_marker` | filename | Inbox marker | Marker required before an Inbox item can be moved, when configured. |

Only `skip` is currently supported for `conflict_policy`. Templates support the tokens documented in [Media layout and automation](media-layout.md). Preview with `library organize plan` before enabling writes.

### Metadata and artwork

| YAML key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `collections[].media.metadata.enabled` | boolean | `false` | Enable metadata enrichment. |
| `collections[].media.metadata.provider` | string | `tmdb` | Supported provider: `tmdb`. |
| `collections[].media.metadata.api_key_env` | environment variable name | `RCFIN_TMDB_API_KEY` | Variable holding the TMDb API key/read token. |
| `collections[].media.metadata.language` | language tag | `en-US` | Preferred metadata language. |
| `collections[].media.metadata.fallback_languages` | language list | `[en]` | Metadata lookup fallbacks. |
| `collections[].media.metadata.artwork_max_bytes` | integer bytes | `12582912` | Maximum downloaded artwork size. |
| `collections[].media.metadata.artwork_max_width` | integer pixels | `5000` | Maximum accepted artwork width. |
| `collections[].media.metadata.artwork_max_height` | integer pixels | `5000` | Maximum accepted artwork height. |
| `collections[].media.metadata.artwork_formats` | string list | `[jpeg, png]` | Accepted artwork formats. |
| `collections[].media.metadata.artwork_poster_size` | string | `w500` | TMDb poster image size. |
| `collections[].media.metadata.artwork_backdrop_size` | string | `w780` | TMDb backdrop image size. |
| `collections[].media.metadata.request_timeout` | duration | `10s` | Per-request timeout. |
| `collections[].media.metadata.retry_limit` | integer | `3` | Bounded retry count for transient provider failures. |
| `collections[].media.metadata.fetch_artwork` | boolean | `true` | Write poster/backdrop artwork when available. |
| `collections[].media.metadata.fetch_nfo` | boolean | `true` | Write metadata NFO sidecars when available. |
| `collections[].media.metadata.overwrite_existing` | boolean | `false` | Replace existing NFO metadata fields. |
| `collections[].media.metadata.overwrite_artwork` | boolean | follows `overwrite_existing` if omitted | Replace existing artwork files. |
| `collections[].media.metadata.minimum_confidence` | float | `0.82` effective provider threshold when unset/zero | Minimum confidence for metadata matching; range `0..1`. |
| `collections[].media.metadata.refresh_existing_after` | duration | disabled | Reconsider existing items after this interval. |

### Subtitles

| YAML key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `collections[].media.subtitles.enabled` | boolean | `false` | Enable subtitle search/download. |
| `collections[].media.subtitles.provider` | string | `opensubtitles` | Supported provider: `opensubtitles`. |
| `collections[].media.subtitles.api_key_env` | environment variable name | `RCFIN_OPENSUBTITLES_API_KEY` | OpenSubtitles application API key variable. |
| `collections[].media.subtitles.username_env` | environment variable name | `RCFIN_OPENSUBTITLES_USERNAME` | Optional account username variable. |
| `collections[].media.subtitles.password_env` | environment variable name | `RCFIN_OPENSUBTITLES_PASSWORD` | Optional account password variable. |
| `collections[].media.subtitles.languages` | language list | `[en]` | Requested subtitle languages. |
| `collections[].media.subtitles.hearing_impaired` | boolean | `false` | Prefer hearing-impaired/SDH tracks. |
| `collections[].media.subtitles.forced` | boolean | `false` | Request forced subtitle tracks. |
| `collections[].media.subtitles.formats` | string list | `[srt]` | Allowed download formats (supported outputs include SRT/VTT). |
| `collections[].media.subtitles.overwrite_existing` | boolean | `false` | Replace matching existing subtitle sidecars. |
| `collections[].media.subtitles.minimum_confidence` | float | `0.84` | Minimum candidate confidence; range `0..1`. |
| `collections[].media.subtitles.request_timeout` | duration | `15s` | Provider request timeout. |
| `collections[].media.subtitles.retry_limit` | integer | `3` | Bounded transient retry count. |
| `collections[].media.subtitles.max_download_bytes` | integer bytes | `20971520` | Maximum subtitle download size. |

Language tags accept bounded BCP-47-style values such as `en`, `pt-BR`, and `zh-Hans-CN`. See [Media layout and automation](media-layout.md) for provider behavior, order of operations, CLI planning, and overwrite safeguards.

### Per-collection WebDAV presentation

These fields apply when global `webdav.enabled` is true.

| YAML key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `collections[].webdav.enabled` | boolean | `true` | Include this collection in WebDAV. |
| `collections[].webdav.views` | list | `[all, genres, actors, directors, years]` | Views to publish. Supported values: `all`, `genres`, `actors`, `directors`, `years`, `studios`. |
| `collections[].webdav.labels` | string map | built-in labels | Display labels for view names. |
| `collections[].webdav.file_name` | template | built-in | Movie file naming template. |
| `collections[].webdav.title_folder` | template | built-in | Movie title-folder template. |
| `collections[].webdav.episode_file_name` | template | built-in | TV episode filename template. |
| `collections[].webdav.nfo` | boolean | `true` | Expose NFO sidecars or generated read-only NFO responses. |
| `collections[].webdav.artwork` | boolean | `true` | Expose available artwork sidecars. |

## Environment and dotenv

At startup RCFin loads `.env` from the working directory if present. Set `RCFIN_ENV_FILE` to use another file. Existing process environment values take precedence. Provider credential aliases are also accepted: `TMDB_READ_ACCESS_TOKEN` or `TMDB_API_KEY`, and `OPENSUBTITLES_API_KEY`, `OPENSUBTITLES_USERNAME`, `OPENSUBTITLES_PASSWORD`. Explicit `RCFIN_*` values win. Tailscale uses the exact environment variable named by `listen.tailscale.auth_key_env` (default `RCFIN_TAILSCALE_AUTH_KEY`).

PostgreSQL uses `RCFIN_POSTGRES_URL` only when its YAML URL is empty. See [Networking and access](networking.md) for Tailscale secret guidance.

## Troubleshooting

- **Remote section not found:** `rclone.config` is a filename, not a directory; confirm the section matches the part before `:` in `remote`.
- **No library changes appear:** consider the collection/global `scan_interval` and the VFS `dir_cache_time`; they control different caches.
- **Tailscale identity changes after restart:** persist and reuse `listen.tailscale.state_dir`.
- **Media writes fail:** metadata, subtitles, and organization require a writable VFS. VFS is read-only by default; review [the VFS reference](vfs-options.md).
- **Existing sidecars stay unchanged:** replacement is off by default. Set the stage-specific overwrite setting and review a plan before refreshing.
