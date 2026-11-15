# Media layout review corpus

This sanitized corpus is used to review inventory and dry-run decisions across
the layouts in `PLAN.md`. Names and IDs are synthetic. No fixture contains
media bytes; the listed video paths represent ordinary files with plausible
sizes and modification times.

## Movies and versions

```text
Movies/
├── Collector's Edition (2008)/
│   └── Collector's Edition (2008) [Criterion Collection] [Bluray-1080p]-GroupX.mkv
├── .Inbox/
│   ├── Arrival (2016)/
│   │   ├── Arrival (2016) [Bluray-1080p].mkv
│   │   ├── Arrival (2016) [Bluray-1080p].en.srt
│   │   └── movie.nfo
│   └── Arrival (2016) - Director's Cut/
│       └── Arrival.Directors.Cut.2160p.mkv
└── Sci-Fi/
    └── Arrival (2016) [tmdbid-999001]/
        ├── Arrival (2016) [Bluray-1080p].mkv
        ├── Arrival (2016) [WEB-2160p].mkv
        ├── movie.nfo
        ├── poster.jpg
        └── Arrival (2016).en.srt
```

Expected review points: Inbox arrivals are kept out of normal playback until
promoted; the existing item retains its `Sci-Fi` grouping path; its two cuts
remain one movie with distinct media sources; existing NFO, artwork, and
subtitle files are not overwritten. The unlabelled Inbox cut must not be
merged into the existing movie solely because its title is similar. The
collector's edition entry should expose its edition, quality, resolution, and
explicit scene release group as inventory fields and keep those distinctions
in any configured naming template.

## Television, specials, and daily episodes

```text
TV/
├── .Inbox/
│   └── Example Series/
│       ├── Season 01/
│       │   ├── Example.Series.S01E01-E02.1080p.mkv
│       │   └── Example.Series.2025-01-12.WEB.mkv
│       └── Specials/
│           └── Example.Series.S00E01.Special.mkv
└── Drama/
    └── Example Series (2020) [tmdbid-999002]/
        ├── tvshow.nfo
        ├── Season 01/
        │   ├── season.nfo
        │   └── Example Series - S01E01 - Pilot.mkv
        └── Specials/
            └── Example Series - S00E01 - Reunion.mkv
```

Expected review points: the multi-episode file remains one video and may map
only to the explicit, increasing episode sequence; daily numbering is accepted
only when its date maps uniquely to provider episode data; season zero remains
a special. The existing `Drama` grouping path is preserved.

## Uncertain names and sidecars

```text
Movies/
├── .Inbox/
│   ├── The Film (2023) [tmdbid-999003]/
│   │   ├── The Film (2023).mkv
│   │   ├── The Film (2023).nfo
│   │   ├── poster.png
│   │   └── The Film (2023).fr.srt
│   ├── Untitled.1080p.mkv
│   └── Competing Title (2021) (2022).mkv
└── Action/
    └── Existing Film (2019)/
        ├── Existing Film (2019).mkv
        ├── movie.nfo
        └── Existing Film (2019).en.srt
```

Expected review points: an explicit provider ID takes precedence; existing
sidecars remain intact unless their specific overwrite policy permits a
replacement; an unidentifiable title and a conflicting year must remain
visible as `needs_review` instead of receiving guessed metadata or a move.

## Extras and destination conflicts

```text
Movies/
├── .Inbox/
│   └── Example Movie (2024)/
│       ├── Example.Movie.2024.mkv
│       ├── Extras/
│       │   └── Behind the Scenes/
│       │       └── Interview.mkv
│       └── trailer.mp4
└── Example Movie (2024) [tmdbid-999004]/
    └── Example Movie (2024).mkv
```

Expected review points: extras and trailers are not selected as the primary
movie; an occupied normalized destination is a visible conflict, with no
overwrite, delete, or implicit merge. A dry run performs no storage writes.

## Disc structures

```text
Movies/
└── Example Disc (1999)/
    ├── VIDEO_TS/
    │   ├── VIDEO_TS.IFO
    │   ├── VTS_01_0.IFO
    │   └── VTS_01_1.VOB
    └── Extras/
        └── VIDEO_TS/
            └── VTS_01_1.VOB
```

Expected review points: only the validated movie disc root is considered for
disc metadata or optional whole-directory normalization. The nested disc
under `Extras` is excluded from playable and normalization inventory. Disc
playback is outside this corpus because `PLAN.md` tracks it as a separate
requirement.
