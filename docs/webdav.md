# Read-only virtual WebDAV

RCFin can expose configured libraries as a virtual WebDAV filesystem. The
directory structure is built from the RCFin catalog; no directories or
sidecars are created in Rclone. Video reads resolve to the current media path
in the corresponding library.

Every authorized WebDAV user sees the same layout. RCFin does not attempt to
identify Kodi or other clients from User-Agent headers. NFO and artwork are
available to all clients when enabled for a library.

## Enable the endpoint

Add the following top-level section to `rcfin.yaml`:

```yaml
webdav:
  enabled: true
  path: /dav/
  cache:
    directory_ttl: 5m
```

Each collection can configure its views and naming rules:

```yaml
collections:
  - id: movies
    name: Movies
    type: movies
    remote: files:Movies
    webdav:
      enabled: true
      views: [all, genres, actors, directors, years]
      labels:
        all: All
        genres: Genres
        actors: Actors
        directors: Directors
        years: Years
      title_folder: "{title} ({year})"
      file_name: "{title} ({year}).{ext}"
      nfo: true
      artwork: true
```

TV collections also accept `episode_file_name`. Supported views are `all`,
`genres`, `actors`, `directors`, `years`, and `studios`. If `views` is omitted,
all standard views are enabled. Collections are enabled for WebDAV by default
when the endpoint is enabled; set `webdav.enabled: false` on a collection to
exclude it. NFO and artwork are exposed by default; each can be disabled per
collection.

View labels can be overridden by a `labels` mapping. Filename templates
support `{title}`, `{name}`, `{year}`, `{ext}`, and `{resolution}`. Episode
templates also support `{series}`, `{season:02}`, `{episode:02}`, and
`{episode_title}`. Unknown or absent metadata falls back to the item name or
original extension where possible. A changed metadata value can change the
virtual path; the item remains linked to the same catalog identity and current
storage path.

When templates are omitted, movie and show folders use `{title} ({year})`,
movie files use `{title} ({year}).{ext}`, and episode files use
`{series} - S{season:02}E{episode:02} - {episode_title}.{ext}`. Provider ID
suffixes are removed from display names. Episodes and their NFO files appear
directly in the season folder. Season folders are labeled `Season 01`,
`Season 02`, and so on; season zero is labeled `Specials`. Existing SRT and VTT
subtitles appear beside the corresponding video, with their language and track
suffixes carried onto the virtual video filename so clients can associate them.

## Directory layout

The standard views organize each item into its own title folder so a video
and its sidecars stay together:

```text
/dav/Movies/All/The Example (2024)/
  The Example (2024).mkv
  movie.nfo
  poster.jpg
  fanart.jpg

/dav/Movies/Genres/Science Fiction/The Example (2024)/...

/dav/TV Shows/All/Example Show/
  tvshow.nfo
  poster.jpg
  Season 01/
    season.nfo
    Example Show - S01E01 - Pilot.mkv
    Example Show - S01E01 - Pilot.nfo
```

The **All** view is available even when metadata is missing. Items without a
genre, actor, director, or year are omitted from that corresponding category
view. The same item can appear in several views; each copy resolves to the same
underlying media file.

Existing NFO/artwork sidecars are served from Rclone. If an NFO is missing,
RCFin generates a read-only NFO response from the loaded catalog metadata.
Artwork is exposed when the catalog has a corresponding existing artwork path.
DAV requests do not call metadata providers or download artwork.

## Authentication and access

WebDAV uses HTTP Basic authentication with an RCFin username and password.
Per-library access is checked on every request using the same account grants
as RCFin's Jellyfin API. A user sees only granted libraries. Administrators
can access every configured collection.

Basic authentication sends a reusable credential with each request. Use the
endpoint over HTTPS, either with RCFin's TLS configuration or behind a trusted
TLS-terminating reverse proxy. Do not expose the plain HTTP endpoint to an
untrusted network.

The endpoint is read-only. `OPTIONS`, `PROPFIND`, `GET`, and `HEAD` are
available. Uploads, deletes, moves, directory creation, locks, and property
changes are rejected. Clients should mount `/dav/` with the RCFin account
credentials; there is no client-specific path or User-Agent detection.

## Catalog refresh behavior

Directory trees are cached by user and catalog version. A successful catalog
refresh invalidates them immediately; `directory_ttl` (default one minute) is
the fallback expiry. A remote change can take until the next successful scan
to appear. Reads resolve a virtual item to its current catalog path; if the
remote object disappears before refresh, the read returns the underlying
storage error. Rebuilding the catalog after metadata updates can change
formatted virtual filenames.

## Notes

WebDAV behavior is implemented with Go's `golang.org/x/net/webdav` handler and
a custom read-only virtual filesystem. The endpoint follows standard DAV
directory discovery and file retrieval behavior, but client interoperability
should still be checked with the clients used in a deployment. See
[`config.example.full.yaml`](../config.example.full.yaml) for a complete
configuration example and [the configuration reference](configuration.md) for
all supported WebDAV settings.
