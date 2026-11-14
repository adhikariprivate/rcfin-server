# Media folder and filename layout

This document describes what RCFin's current scanner recognizes. It is intentionally narrower than Kodi or Jellyfin naming support: unsupported names and extensions are skipped rather than inferred.

## TV shows

Put each show directly under the configured shows collection root. Put episodes in season directories named `S` followed by the season number. `S1` and `S01` are recognized for season 1. Put specials in a `Specials` directory.

The episode filename must contain a season/episode token bounded by a space, dot, or underscore **on both sides**. The common form is `<show> S01E01 - <episode title>.<extension>`:

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
    │   ├── The Fixers S01E01 - The Pilot.en.srt
    │   ├── The Fixers S01E01 - The Pilot.fr.srt
    │   ├── The Fixers S01E01 - The Pilot-thumb.jpg
    │   ├── The Fixers S01E01 - The Pilot.nfo
    │   ├── The Fixers S01E02 - A New Day.mp4
    │   └── poster.jpg
    └── S02/
        └── The Fixers S02E01 - Another Year.mp4
```

`The Fixers S01E01.mp4` is **not recognized**: after the episode number, the filename stem ends immediately. Add a separator and title, such as `The Fixers S01E01 - The Pilot.mp4`. The video file's extension is not part of the stem checked by the episode parser.

### Episode number patterns

The scanner recognizes these patterns in the video filename stem:

- `Show S01E02 - Title.mp4`
- `Show 1x02 - Title.mp4`
- `Show 102 - Title.mp4` (season 1, episode 02; a space/dot delimiter is required before the digits)
- `Show 2015.03.08 Title.mp4` (date-style episode token)
- Multi-episode `S01E02-E03` form is parsed as a double episode.

The season/episode token must have a space, dot, or underscore before and after it. A season directory supplies the season hint for date-style episode names. Use the `SxxEyy` form for the most predictable results.

### Supported video extensions

The current scanner recognizes `.divx`, `.mov`, `.mp4`, `.MP4`, `.m4u`, and `.m4v` for episodes. Other extensions such as `.mkv` are not currently recognized by this scanner.

## TV show sidecars and artwork

Sidecars must be in the show root or the corresponding season directory, and episode sidecars must use the full video stem as their base name.

| File | Location and naming | Scanner behavior |
| --- | --- | --- |
| Show NFO | `Show/tvshow.nfo` | Read as show metadata. |
| Episode NFO | Beside video, `<video stem>.nfo` | Read as episode metadata when the video episode was recognized. |
| SRT subtitle | Beside video, `<video stem>.en.srt` (or another alphabetic language suffix) | Detected and associated with the episode. A suffixless `.srt` is also accepted but treated as unspecified. `.forced`, `.sdh`, and `.default` flags are not interpreted. |
| VTT subtitle | `.vtt` sidecar | Not currently detected by the TV scanner's filename matcher, despite a later VTT handling branch. Use `.srt` if relying on current RCFin behavior. |
| Episode thumbnail | `<video stem>-thumb.jpg`, `<video stem>.thumb.jpg`, or equivalent `.jpeg`, `.png`, `.tbn` | Attached as the episode thumbnail. |
| Show artwork | `poster`, `fanart`, `banner`, `folder`, or `clearlogo` with `.jpg`, `.jpeg`, `.png`, or `.tbn` | Recognized in the show root. For example, `poster.jpg` and `clearlogo.png`. |
| Season poster/banner | `poster.jpg` / `banner.jpg` inside a season folder, or `season01-poster.jpg` / `season01-banner.jpg` in the show or season folder | Recognized as season artwork. |

The scanner only attaches a thumbnail to an episode; this does not guarantee every Jellyfin client displays it as an episode primary poster. External subtitles are indexed by RCFin, but subtitle track advertisement/delivery to Jellyfin clients is not currently implemented.

## Movie sidecars

Use one folder per movie. RCFin looks for a supported video file directly in that folder. The current movie scanner recognizes `.divx`, `.mov`, `.mp4`, `.MP4`, `.m4u`, and `.m4v`.

```text
Movies/
└── Example Movie (2020)/
    ├── Example Movie.mp4
    ├── movie.nfo
    ├── poster.jpg
    ├── fanart.jpg
    ├── banner.jpg
    └── Example Movie.en.srt
```

Movie artwork recognizes `banner`, `fanart`, `folder`, and `poster` names with `.jpg`, `.jpeg`, `.png`, or `.tbn`. Movie SRT sidecars use the same filename stem as the video, optionally followed by a language suffix. Movie NFO metadata is read from an NFO sidecar in the movie folder.

## NFO metadata

NFO files are Kodi-style XML metadata. `tvshow.nfo` is the show-level filename; episode and movie NFO sidecars should match their associated video stem. NFO metadata describes titles, plots, dates, genres, people, ratings, and provider IDs. It does not configure subtitle tracks.

## Important limitations

- Keep show folders directly under a shows collection root and season folders directly under each show. Nested grouping folders are not scanned as shows.
- The scanner's supported extensions and filename rules above reflect its current implementation, not all formats supported by Jellyfin.
- A library rescan is needed after adding or renaming files; the automatic scan interval defaults to 30 minutes.
