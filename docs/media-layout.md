# Media folder and filename layout

This document describes what RCFin's current scanner recognizes. It is intentionally narrower than Kodi or Jellyfin naming support: unsupported names and extensions are skipped rather than inferred.

See [media-fixtures.md](media-fixtures.md) for sanitized movie, TV, sidecar,
conflict, extras, and disc layout examples with expected review outcomes.

## TV shows

Shows may be nested below grouping directories; grouping paths are preserved. Put episodes in season directories named `S` followed by the season number or `Season` followed by the number. `S1`, `S01`, `Season 1`, and `Season 01` are recognized for season 1. Put specials in a `Specials` directory.

The episode filename should contain a season/episode token; season directories and daily date tokens are also inventoried for conservative matching. The common playback form is `<show> S01E01 - <episode title>.<extension>`:

```text
TV/
└── The Fixers (2026)/
    ├── tvshow.nfo
    ├── poster.jpg
    ├── fanart.jpg
    ├── banner.jpg
    ├── clearlogo.png
    ├── S01/
    │   ├── The Fixers S01E01 - The Pilot.mp4
    │   ├── The Fixers S01E01 - The Pilot.en.default.srt
    │   ├── The Fixers S01E01 - The Pilot.fr.vtt
    │   ├── The Fixers S01E01 - The Pilot-thumb.jpg
    │   ├── The Fixers S01E01 - The Pilot.nfo
    │   ├── The Fixers S01E02 - A New Day.mp4
    │   └── poster.jpg
    └── S02/
        └── The Fixers S02E01 - Another Year.mp4
```

`The Fixers S01E01.mp4` is **not recognized**: after the episode number, the filename stem ends immediately. Add a separator and title, such as `The Fixers S01E01 - The Pilot.mp4`. The video file's extension is not part of the stem checked by the episode parser.

### Episode number patterns

The library scanner and media inventory recognize these patterns in the video filename stem:

- `Show S01E02 - Title.mp4`
- `Show 1x02 - Title.mp4`
- `Show 102 - Title.mp4` (season 1, episode 02; a space/dot delimiter is required before the digits)
- `Show 2015.03.08 Title.mp4` (date-style episode token)
- Multi-episode `S01E02-E03` form is parsed as a double episode.
- Shorthand ranges such as `S01E02-04` expand to episodes 2, 3, and 4; ranges above eight episodes or with invalid ordering remain unrecognized for review.

The season/episode token must have a space, dot, or underscore before and after it. A season directory supplies the season hint for date-style episode names. Use the `SxxEyy` form for the most predictable results.

### Supported video extensions

The library scanner and inventory recognize `.3g2`, `.3gp`, `.avi`, `.divx`, `.flv`, `.m1v`, `.m2ts`, `.m2v`, `.m4u`, `.m4v`, `.mkv`, `.mov`, `.mp4`, `.mpe`, `.mpeg`, `.mpeg2`, `.mpg`, `.mts`, `.ogm`, `.ogv`, `.ts`, `.vob`, `.webm`, and `.wmv` (case-insensitive). Disc trees are inventoried independently; validated movie discs support metadata sidecars and opt-in whole-folder normalization, but not playback.

## TV show sidecars and artwork

Sidecars must be in the show root or the corresponding season directory, and episode sidecars must use the full video stem as their base name.

| File | Location and naming | Scanner behavior |
| --- | --- | --- |
| Show NFO | `Show/tvshow.nfo` | Read as show metadata. |
| Episode NFO | Beside video, `<video stem>.nfo` | Read as episode metadata when the video episode was recognized. |
| SRT subtitle | Beside video, `<video stem>.en.srt` (or another language suffix) | Detected, advertised as an external subtitle track, and served to Jellyfin clients. A suffixless `.srt` is accepted and treated as unspecified language. |
| VTT subtitle | Beside video, `<video stem>.fr.vtt` | Detected, advertised, and served as WebVTT. |
| Episode thumbnail | `<video stem>-thumb.jpg`, `<video stem>.thumb.jpg`, or equivalent `.jpeg`, `.png`, `.tbn` | Attached as the episode thumbnail. |
| Show artwork | `poster`, `fanart`, `banner`, `folder`, or `clearlogo` with `.jpg`, `.jpeg`, `.png`, or `.tbn` | Recognized in the show root. For example, `poster.jpg` and `clearlogo.png`. |
| Season poster/banner | `poster.jpg` / `banner.jpg` inside a season folder, or `season01-poster.jpg` / `season01-banner.jpg` in the show or season folder | Recognized as season artwork. |

The scanner only attaches a thumbnail to an episode; this does not guarantee every Jellyfin client displays it as an episode primary poster. Sidecar subtitle tracks are included in item and playback media-stream information and delivered through authenticated Jellyfin-compatible subtitle URLs. RCFin checks the user's collection permission before serving the track.

### Subtitle filename suffixes

Use the exact video filename stem, then add dot-separated language and optional flags before `.srt` or `.vtt`:

```text
The Fixers S01E01 - The Pilot.en.srt
The Fixers S01E01 - The Pilot.fr.forced.vtt
The Fixers S01E01 - The Pilot.pt-BR.sdh.srt
The Fixers S01E01 - The Pilot.en.foreign.srt
The Fixers S01E01 - The Pilot.en.cc.srt
The Fixers S01E01 - The Pilot.en.default.srt
The Fixers S01E01 - The Pilot.en.commentary.srt
```

RCFin recognizes alphabetic language tags such as `en`, `fr`, and `pt-BR`; `default`; `forced` (also `foreign`); and hearing-impaired markers `sdh` and `cc`. Other suffix text is shown as the track title. If no language tag is present, RCFin reports `und`. SRT tracks are converted to WebVTT when a Jellyfin client requests VTT delivery; the original sidecar is not modified. VTT sidecars are served as VTT. The playback URL supports both Jellyfin forms with and without a start-position component.

## Movie sidecars

Use one folder per movie. RCFin supports movie folders below grouping directories and looks for supported video files directly in each movie folder. The library scanner recognizes `.3g2`, `.3gp`, `.avi`, `.divx`, `.flv`, `.m1v`, `.m2ts`, `.m2v`, `.m4u`, `.m4v`, `.mkv`, `.mov`, `.mp4`, `.mpe`, `.mpeg`, `.mpeg2`, `.mpg`, `.mts`, `.ogm`, `.ogv`, `.ts`, `.vob`, `.webm`, and `.wmv` (case-insensitive). The read-only inventory classifies trailers, samples, featurettes, subtitles, artwork, NFO files, and unrelated files separately; these are not exposed as playable movies.

```text
Movies/
└── Example Movie (2020)/
    ├── Example Movie.mp4
    ├── movie.nfo
    ├── poster.jpg
    ├── fanart.jpg
    ├── banner.jpg
    ├── Example Movie.en.default.srt
    └── Example Movie.es.vtt
```

Movie artwork recognizes `banner`, `fanart`, `folder`, and `poster` names with `.jpg`, `.jpeg`, `.png`, or `.tbn`. Movie SRT and VTT sidecars use the same filename stem as the video, optionally followed by the language and flags described above. Movie NFO metadata is read from an NFO sidecar in the movie folder.

## NFO metadata

NFO files are Kodi-style XML metadata. `tvshow.nfo` is the show-level filename; episode and movie NFO sidecars should match their associated video stem. NFO metadata describes titles, plots, dates, genres, people, ratings, and provider IDs. It does not configure subtitle tracks.

## Important limitations

- Show and movie folders may be nested under grouping directories; the scanner retains those paths. Season folders `S01`, `Season 01`, and `Specials` are recognized.
- The scanner's supported video extensions and filename rules above reflect its current implementation, not all formats supported by Jellyfin.
- A library rescan is needed after adding or renaming files; the automatic scan interval defaults to 30 minutes.
# Library inventory, metadata, subtitles, and intake

Each collection may configure `media.inbox_path` as a relative path from the
collection root, including a nested path such as `Incoming/Movies`. RCFin
excludes that subtree from normal playback inventory;
the default is `.Inbox`. Inventory can inspect either ordinary paths or just
the configured Inbox. The explicitly configured Inbox is scanned even when its
name is hidden; other hidden/system paths remain excluded.

Inventory classifies common video containers, subtitles, artwork, NFOs,
trailers, extras, and other files. It extracts filename title/year, provider
IDs, season/episode tokens, and local NFO IDs. This evidence supports provider
matching and reporting, but uncertain identities remain marked for review.
For recognized video files, inventory also reports parsed release tags,
quality, resolution, cut/version, edition, and a release group only when its
filename suffix is explicit. Organization uses the same parsed values for
destination templates, avoiding separate and potentially inconsistent
filename parsing. The `{edition}` template token can retain labels such as
IMAX, Criterion, or Special Edition.
For TV entries, `provider_ids` identifies the show and
`episode_provider_ids` identifies the episode sidecar. If a path ID conflicts
with its local NFO ID, the inventory records the conflict and enrichment
stops for review instead of making an exact lookup with an arbitrary ID.
Every inventory record includes a role and a classification reason. Inspect a
library without starting the server using `rcfin media inventory LIBRARY_ID`;
pass `--inbox` to inventory only the configured Inbox. The command emits JSON
and performs read-only filesystem operations.
To review provider candidates and proposed metadata sidecars before enabling
metadata writes, run `rcfin media plan metadata LIBRARY_ID`. It emits the same
item-level match evidence, provider-returned metadata fields, and proposed file
actions as refresh, performs provider reads and library scans only, and does
not require `media.metadata.enabled`.
Use `rcfin media plan subtitles LIBRARY_ID` to review subtitle candidates and
proposed sidecar paths before downloads; its JSON includes candidate title,
language, format, flags, match type, and confidence even when a match needs
review. It is also read-only and does not require `media.subtitles.enabled`.

Use `rcfin media plan organization LIBRARY_ID` to preview organization for
stable, ready Inbox items. Add `--existing` to preview normalization of existing
library paths outside the Inbox. The command emits the configured movie or TV
destination plan and provider match evidence; it does not move files or write
media-operation journal entries. It uses the configured organization formats,
provider, and Inbox readiness policy regardless of the organization enable
switch.

The per-library `media.metadata`, `media.subtitles`, and `media.organize`
switches are independent. Metadata refresh writes NFO/artwork sidecars in place
for existing library items. Subtitle refresh can search and download configured
languages. Both routines exclude Inbox items from their regular library scans.
`media.metadata.overwrite_existing` controls NFO fields, while
`media.metadata.overwrite_artwork` controls replacement of local poster and
backdrop images. Artwork overwrite defaults to the legacy
`overwrite_existing` value when omitted, preserving existing configurations;
set it explicitly to choose a different artwork policy.
Subtitle deduplication uses inventoried external sidecars; a base-language
request (for example `en`) treats regional sidecars (for example `en-US`) as
matching, while a region-specific request remains exact. RCFin currently does
not probe embedded media subtitle streams, so those tracks are not considered
by automatic subtitle acquisition.
The unified per-library cycle inventories the playable library and Inbox once.
It waits for the configured completion marker or a stable full-tree interval
before an Inbox item can be classified or enriched. Unfinished items remain
pending while other ready items proceed. For ready Inbox arrivals, enabled
provisioning runs before metadata, and subtitles run after metadata. Existing
library items are not automatically organized. Organization is disabled by
default and `dry_run` defaults to true.
When organization runs, unrecognized files in an Inbox item produce a
`needs_review` report and prevent the containing item from moving. In a
write-enabled run, that review state is also kept in the media operation
journal; correcting or removing the unknown file allows a fresh plan to clear
the review.
Storage sidecar writes additionally
require a writable collection VFS; the VFS remains read-only by default.
Metadata, episode fallback, and subtitle language settings accept bounded
BCP-47-style tags such as `en`, `pt-BR`, or `zh-Hans-CN`; malformed tags and
case-insensitive duplicates in language lists are rejected at startup.
Subtitle candidate acceptance is controlled by
`media.subtitles.minimum_confidence` (range `0..1`, default `0.84`); lowering
it explicitly permits weaker provider matches, while `0` accepts any ranked
candidate that matches the requested language and flags.
Candidates outside the configured subtitle formats are removed before ranking,
so an unsupported high-scoring result cannot hide a lower-scoring supported
match. Search proceeds from file hash to provider IDs to title/year when an
earlier tier has no candidate that clears the configured confidence threshold;
results from unsuccessful tiers are deduplicated before final ranking.
Subtitle text with UTF-8 or BOM-marked UTF-16 is normalized directly; invalid
UTF-8 may be decoded as Windows-1252, then must pass SRT/VTT structure and
control-character validation before it can be saved.

TMDb and OpenSubtitles credentials are read from environment variables named
in the collection configuration. Do not put credentials in YAML. Provider
requests have timeouts, bounded response/download sizes, process-wide rate
controls across configured libraries, and
conservative identity/candidate matching. Temporary OpenSubtitles download
URLs are redacted from reports and operation errors. When the OpenSubtitles
download quota is exhausted, the client recognizes the provider's quota
response and stops further download requests for that run. Items affected by
quota exhaustion are scheduled for retry after 24 hours instead of consuming
the ordinary transient-error retry budget. The operation journal stores
planned and completed organization operations in SQLite or PostgreSQL and
reconciles source/destination state before retrying after interruption.

When a background cycle finds eligible new or changed media but provider
credentials are unavailable, it records retryable metadata/subtitle operations
instead of dropping that work. The next library cycle retries them after the
configured scan interval; inspect the reason with `rcfin library status` and
retry immediately by operation ID when appropriate.

At startup, RCFin optionally loads `.env` from its working directory. Set
`RCFIN_ENV_FILE` to use a different dotenv path. Variables already present in
the process environment take precedence. For the provider credentials shown
in `rcfin.example.yaml`, RCFin accepts either the configured `RCFIN_*` names
or these common unprefixed names: `TMDB_READ_ACCESS_TOKEN` (preferred) or
`TMDB_API_KEY`, `OPENSUBTITLES_API_KEY`, `OPENSUBTITLES_USERNAME`, and
`OPENSUBTITLES_PASSWORD`. The unprefixed variables are mapped to their
`RCFIN_*` counterparts at startup; an explicitly supplied `RCFIN_*` value
wins. Keep `.env` out of version control and restrict its file permissions.

TMDb movie certifications and TV content ratings are selected for the country
region in the configured metadata language. The localized title is used as the
initial sort title; existing local sort titles remain protected by the normal
NFO merge policy.

OpenSubtitles requires an application API key; account username/password are
optional and select the account's download allowance when configured. Limits
vary by account and subscription, so check the current
[consumer subscription documentation](https://opensubtitles.stoplight.io/docs/opensubtitles-api/6a28e448bb3c8-consumer-subscription-prices)
before deploying automated downloads. The provider's current integration guide
also documents key registration, authentication, and daily download allowances
([OpenSubtitles API guide](https://blog.opensubtitles.com/opensubtitles/how-to-build-a-movie-subtitle-finder-cli-with-the-opensubtitles-api)).
Permanent account-authentication failures are cached for the current worker
client and recorded as terminal per-item operations so invalid credentials do
not trigger repeated login attempts on later scan cycles. Correct the
credentials, then queue a manual retry for the affected operations; transient
login/network failures remain eligible for automatic retry.

The public `/About/Credits` page presents TMDb's required attribution and its
approved logo, served from `/About/Credits/TMDB.svg`. The logo is included
unmodified and displayed beneath RCFin's larger product heading. TMDb's API is
free for non-commercial use with attribution; deployments with a commercial
primary purpose must obtain the appropriate license directly from TMDb. See
[TMDb's official FAQ and attribution requirements](https://developer.themoviedb.org/docs/faq).

Administrative media operations are CLI-only. Use `rcfin library scan LIBRARY`
to list and reconcile the library plus Inbox, then `rcfin library status
LIBRARY` to inspect the last saved inventory. Use `rcfin library inbox status
LIBRARY [--item PATH]` for Inbox paths, stable-transfer observations, and
organization review operations. `rcfin library refresh metadata
LIBRARY [--item PATH]` and `rcfin library refresh subtitles LIBRARY [--item
PATH] [--language LANG]` explicitly request provider work. For metadata,
`--replace` permits replacing NFO metadata and `--replace-artwork` separately
permits replacing posters/backdrops. For subtitles, `--replace` permits
replacing local subtitle files. `rcfin library inbox stage LIBRARY ITEM_PATH` queues a
same-library move into Inbox, where ordinary transfer-readiness checks apply.
`rcfin library refresh all LIBRARY [--item PATH]` runs metadata first and
subtitles second from the same inventory. Its replacement flags are separate:
`--replace-metadata`, `--replace-artwork`, and `--replace-subtitles`. `rcfin
library status LIBRARY [--item PATH|--job ID]` reports inventory
classification and media-operation state, or looks up one durable operation ID.
Use `rcfin library organize plan LIBRARY [--item PATH] [--existing]` to review
the proposed paths. After reviewing the report, `rcfin library organize apply
LIBRARY --yes [--item PATH] [--existing]` executes the configured operation;
apply requires the relevant organization feature enabled and its `dry_run`
setting disabled. Existing-path normalization additionally requires
`media.normalize_existing.enabled`. `rcfin library retry LIBRARY --job ID`
retries a failed journal operation by ID; `rcfin library retry LIBRARY
ITEM_KEY ACTION` remains available. The running server recovers queued
operations during its next library cycle. When a unique move cannot be inferred,
review the source and destination with status, then explicitly accept a
fingerprint-matching video file or a directory whose complete descendant tree
matches with `rcfin library item adopt-move LIBRARY --from OLD_PATH --to
NEW_PATH`. Directory adoption carries all matching descendants and their
processing state. This updates catalog identity and journal paths; it does not
move remote files. `rcfin library scan LIBRARY [--full]` always reconciles a
complete remote listing; `--full` is explicit for scripts, and scans do not
imply provider refresh.

Workers renew active media-operation leases during long provider or storage
work. If a repository reports that a worker no longer owns a lease, its
operation context is canceled so it stops issuing further changes. Final
journal writes carry the claimant's worker ID; SQLite and PostgreSQL reject a
write from an older owner while a newer owner is running the operation.
The background library cycle reconciles the final paths after a completed move
and rebuilds the Jellyfin search index. Metadata and subtitle sidecar writes
also refresh the in-memory item metadata and media-stream list.
Sidecar destinations must be strict relative paths beneath the configured
library root. Writes verify temporary and promoted contents before reporting
success. Explicit overwrites stage a verified backup; if a destination race
prevents safe restoration, the error reports where the old sidecar was retained.

On `SIGINT` or `SIGTERM`, RCFin stops accepting HTTP requests, cancels media
provider and organization work, stops collection rescans and cache cleanup,
then waits for those workers before closing Rclone storage and the database.

Movie and TV Inbox normalization is implemented for same-library backends that
advertise `DirMove` and `Move`. TV processing accepts standard season
directories, numbered episodes, specials, and provider-confirmed episode
titles; daily dates are accepted only when exactly one provider episode in the
season matches. Increasing explicit sequences such as `S01E01-E02-E03` and
`S01E01E02E03` are supported up to eight episodes when every episode has
provider metadata; each receives its own NFO using `-S01E01.nfo`,
`-S01E02.nfo`, and subsequent suffixes. Missing, reversed, malformed, or
longer sequences stay in review. This per-episode NFO naming follows Kodi v22's multi-episode format
([episode NFO guide](https://kodi.wiki/view/NFO_files/Episodes)). Plans
reject uncertain matches, unsupported operations, and destination conflicts;
cross-backend copy/delete is not implemented.
Episode NFOs include TMDb episode identity, air date, runtime, rating,
directors, writers, and guest-star roles when the season response supplies
those values.
Capability reports expose the backend's advertised `Move`, `DirMove`,
`PutStream`, and `Copy` operations. `put` reflects whether that library's VFS
is configured writable; it does not predict backend permissions or availability.
When a same-library backend advertises `Move` or `DirMove`, RCFin calls that
operation with the original source and final destination for local and remote
libraries. This applies to movie folders, show and season folders, episode file
renames, sidecars, and explicit existing-item staging into `.Inbox`. RCFin does
not copy media to a temporary location first, and it never falls back to a
copy after an advertised backend move fails. A backend without the required
move capability remains unsupported and is sent for review; RCFin does not
broaden copy/delete behavior for it.

Rclone's generic feature flags say only whether `Move` or `DirMove` is
available. They do not say whether a backend implements it as a server-side
rename or as copy/delete, and they do not guarantee provider-level atomicity.
Local moves are reported as `local_rename` with
`possible_copy_delete: false`. For remote moves, `move_semantics` is
`backend_defined` and `possible_copy_delete` is `null` because Rclone exposes no
generic way to determine the provider's implementation. `same_filesystem`
means both paths are handled by the one configured library backend; it is not
an atomicity guarantee.

The journal records the expected recursive byte total and available checksum
evidence before a directory move. After `DirMove`, RCFin verifies that the
destination exists as a directory, the source is absent, and the destination
has the expected byte total before recording completion. File moves similarly
verify that the destination exists with the expected size and the source is
absent. If a backend returns an error or leaves an ambiguous state, RCFin does
no cleanup and lets journal recovery reconcile the resulting paths. Recovery may remove
a duplicate source only after the destination verifies against the journaled
evidence; mismatched or partial source/destination trees remain for review.
When the backend exposes checksums, RCFin also journals a checksum for a file
or a deterministic checksum over every relative directory path (including
empty folders), file path, and backend hash in a directory;
destination-only recovery must match that evidence before an operation is
marked complete. A temporary error reading destination size or hash evidence
uses retry backoff; an actual mismatch remains in review. Destination-only
recovery can use the recorded byte total when hashes are unavailable. If both
source and destination directories remain after an interrupted move, RCFin
requires matching journaled tree checksums before it removes the source.
For a multi-episode video, subtitle download is attempted only when
OpenSubtitles returns an exact file-hash match with the requested language and
flags. Without that match, the subtitle operation stays in review.
Existing videos are never moved by routine metadata or subtitle refresh.
Metadata refresh writes `season.nfo` and a validated `poster.jpg` inside season
folders when TMDb supplies season details and artwork, along with the existing
show and per-episode metadata files. Season NFOs include provider season
identity, air date, rating, and network names when the provider supplies them.

Multiple movie video files in one movie directory remain one Jellyfin movie
item and are exposed as separate media sources with distinct version labels.
Playback requests using `mediaSourceId` stream the selected file directly;
version-specific external subtitle sidecars are attached to that source.
Movie and episode naming templates can use `{release_group}`. RCFin preserves
an explicit terminal `[GROUP]` suffix, or a scene-style `-GROUP` suffix when
the filename also contains recognized release-quality tags. It leaves the
token empty when the source does not clearly identify a group.

DVD `VIDEO_TS` and Blu-ray `BDMV` trees appear in inventory as separate
`dvd_video` and `blu_ray` structures. Movie metadata refresh can identify a
validated disc tree from its containing movie folder and write movie NFO/artwork
sidecars beside it. Automatic path normalization for these opaque trees is
separately opt-in with `media.organize.disc_folders: true` in a movie library.
RCFin requires DVD `VIDEO_TS.IFO` plus a title-set IFO, or Blu-ray `index.bdmv`,
at least one `.mpls` playlist, and at least one `.m2ts` stream. It rejects
multiple disc roots or a mix of a disc tree with ordinary video files in one
movie folder. When enabled, it moves the containing movie directory as one
unit, preserving the disc files and sidecars without renaming or splitting
them. Disc-folder playback is not implemented; TV disc structures remain
inventory-only.
Metadata can also name ordered `fallback_languages`; RCFin uses them to fill
movie/show provider fields and artwork that are missing from the primary
language while keeping primary-language values when present. Artwork policy
can set `artwork_max_bytes`, `artwork_max_width`, `artwork_max_height`,
`artwork_formats` (`jpeg`, `png`), `artwork_poster_size`, and
`artwork_backdrop_size`. The TMDb client also enforces its hard response and
pixel limits, and rejects artwork when the HTTP `Content-Type` does not match
the decoded JPEG/PNG signature.
TV organization can set `organize.episode_fallback_languages` separately;
season titles and episode titles/overviews fill from those languages only when
the primary-language season response omits those values.
Metadata and subtitle providers accept per-library `request_timeout` and
`retry_limit` settings. Subtitle downloads also accept `max_download_bytes`,
bounded to the provider client's 20 MiB hard limit. Storage sidecar writes are
bounded to 32 MiB, which accommodates that subtitle limit and the configured
artwork maximum.

Existing movie paths have a separate migration setting:

```yaml
media:
  normalize_existing:
    enabled: false
    dry_run: true
```

It scans outside the Inbox and preserves parent grouping directories. It
supports both movie and shows collections. Keep the dry run enabled to inspect
the source, destination, expected bytes, provider match, and backend
capabilities. Execution still requires writable VFS settings.
`organize.preserve_grouping_directories` defaults to true. Setting it to false
opts into destinations at the collection root after a confident match; this
moves only the media item and does not delete any now-empty source directories.

Metadata, subtitle, and organization operations are persisted in the
`media_operations` table in both supported databases. CLI status traverses all
pages with `rcfin media status [LIBRARY_ID]`; the HTTP status route returns a
cursor-paginated page. Force a fresh metadata check with
`rcfin media refresh LIBRARY_ID [ITEM_KEY]`, or retry a terminal/retryable
operation with `rcfin media retry LIBRARY_ID ITEM_KEY ACTION`; review items
remain blocked until the identity or conflict is resolved. The running server
consumes queued refreshes and retries on its next media cycle.
When a movie or show NFO contains a provider ID, RCFin derives its client item
ID from that provider and derives season/episode IDs from the show ID and
numbering. This keeps IDs stable across normalized names and episode filename
changes. Items without provider IDs retain the legacy name-based IDs; completed
journaled folder moves carry the current parent item ID to the new path so a
rescan after normalization or administrator Inbox staging does not invalidate
existing Jellyfin references. If the current NFO/path exposes a different
TMDb ID from the move record, RCFin discards that restored ID to avoid reusing
an old item's identifier for replaced content.
