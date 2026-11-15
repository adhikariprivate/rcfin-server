package collection

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/adhikariprivate/rcfin-server/internal/collection/metadata"
)

// InventoryRole describes a file's role without assuming that it belongs to a
// playable movie or episode. Inventory is deliberately read-only.
type InventoryRole string

const (
	InventoryVideo    InventoryRole = "video"
	InventorySubtitle InventoryRole = "subtitle"
	InventoryArtwork  InventoryRole = "artwork"
	InventoryNFO      InventoryRole = "nfo"
	InventoryTrailer  InventoryRole = "trailer"
	InventoryExtra    InventoryRole = "extra"
	InventoryDisc     InventoryRole = "disc_content"
	InventoryOther    InventoryRole = "other"
)

type InventoryEntry struct {
	Path               string            `json:"path"`
	Size               int64             `json:"size"`
	ModTime            int64             `json:"mod_time,omitempty"`
	IsDir              bool              `json:"is_dir"`
	Role               InventoryRole     `json:"role"`
	Extension          string            `json:"extension,omitempty"`
	Title              string            `json:"title,omitempty"`
	Year               int               `json:"year,omitempty"`
	Season             int               `json:"season,omitempty"`
	Episodes           []int             `json:"episodes,omitempty"`
	AirDate            string            `json:"air_date,omitempty"`
	ProviderIDs        map[string]string `json:"provider_ids,omitempty"`
	EpisodeProviderIDs map[string]string `json:"episode_provider_ids,omitempty"`
	IdentityConflict   bool              `json:"identity_conflict,omitempty"`
	ReleaseTags        []string          `json:"release_tags,omitempty"`
	Quality            string            `json:"quality,omitempty"`
	Resolution         string            `json:"resolution,omitempty"`
	Version            string            `json:"version,omitempty"`
	Edition            string            `json:"edition,omitempty"`
	ReleaseGroup       string            `json:"release_group,omitempty"`
	DiscFormat         string            `json:"disc_format,omitempty"`
	DiscRoot           string            `json:"disc_root,omitempty"`
	Reason             string            `json:"reason,omitempty"`
}

var videoExtensionNames = []string{
	"3g2", "3gp", "avi", "divx", "flv", "m1v", "m2ts", "m2v", "m4u", "m4v", "mkv", "mov",
	"mp4", "mpe", "mpeg", "mpeg2", "mpg", "mts", "ogm", "ogv", "ts", "vob", "webm", "wmv",
}

var inventoryVideoExtensions = func() map[string]bool {
	result := make(map[string]bool, len(videoExtensionNames))
	for _, extension := range videoExtensionNames {
		result["."+extension] = true
	}
	return result
}()
var inventoryEpisode = regexp.MustCompile(`(?i)(?:^|[ ._-])s(\d{1,2})e(\d{1,3})`)
var inventoryCrossEpisode = regexp.MustCompile(`(?i)(?:^|[ ._-])(\d{1,2})x(\d{2,3})(?:[ ._-]|$)`)
var inventoryNumericEpisode = regexp.MustCompile(`(?:^|[ ._-])(\d{1,2})(\d{2})(?:[ ._-]|$)`)
var inventoryDateEpisode = regexp.MustCompile(`(?:^|[ ._-])(\d{4})[.-](\d{2})[.-](\d{2})(?:[ ._-]|$)`)
var inventoryYear = regexp.MustCompile(`(?:^|[ (])(19\d{2}|20\d{2})(?:[) ._-]|$)`)
var inventoryProviderID = regexp.MustCompile(`(?i)(tmdb|tvdb|imdb)id[-_ ]?([a-z0-9]+)`)
var inventorySeasonDirectory = regexp.MustCompile(`(?i)^(?:season\s*0*\d+|s0*\d+|specials)$`)
var inventoryDVDTitleSetIFO = regexp.MustCompile(`^vts_[0-9]{2}_0\.ifo$`)
var inventoryReleaseQuality = regexp.MustCompile(`(?i)(2160p|1440p|1080p|720p|576p|480p|blu[ .-]?ray|bdrip|web[ .-]?dl|webrip|hdtv|hdrip|remux|director'?s[ ._-]?cut|final[ ._-]?cut|(?:special|collector'?s|ultimate|anniversary|limited)[ ._-]?edition|criterion(?:[ ._-]?collection)?|imax|extended|theatrical|uncut|remastered)`)
var inventoryBracketedReleaseGroup = regexp.MustCompile(`\[([A-Za-z0-9][A-Za-z0-9._-]{1,31})\]$`)
var inventorySceneReleaseGroup = regexp.MustCompile(`-([A-Za-z0-9][A-Za-z0-9._-]{1,31})$`)
var inventoryProviderReleaseID = regexp.MustCompile(`(?i)^(?:tmdb|tvdb|imdb)id[-_ ]?[a-z0-9]+$`)

type ReleaseInfo struct {
	Tags         []string
	Quality      string
	Resolution   string
	Version      string
	Edition      string
	ReleaseGroup string
}

// ParseReleaseInfo extracts only release labels that can be recognized from a
// filename. It does not infer missing quality, resolution, version, or group.
func ParseReleaseInfo(filename string) ReleaseInfo {
	stem := strings.TrimSuffix(path.Base(filename), path.Ext(filename))
	var info ReleaseInfo
	seen := map[string]bool{}
	for _, tag := range inventoryReleaseQuality.FindAllString(stem, -1) {
		normalized := strings.ToLower(strings.Join(strings.Fields(strings.NewReplacer(".", " ", "_", " ").Replace(tag)), " "))
		if !seen[normalized] {
			seen[normalized] = true
			info.Tags = append(info.Tags, strings.TrimSpace(tag))
		}
	}
	sort.Strings(info.Tags)
	var qualities []string
	for _, tag := range info.Tags {
		lower := strings.ToLower(tag)
		if strings.HasSuffix(lower, "p") && len(lower) >= 4 {
			info.Resolution = tag
		} else if strings.Contains(lower, "edition") || strings.HasPrefix(lower, "criterion") || lower == "imax" {
			info.Edition = joinReleaseValue(info.Edition, tag)
		} else if strings.Contains(lower, "cut") || strings.Contains(lower, "extended") || strings.Contains(lower, "theatrical") || strings.Contains(lower, "uncut") || strings.Contains(lower, "remastered") {
			info.Version = joinReleaseValue(info.Version, tag)
		} else {
			qualities = append(qualities, tag)
		}
	}
	info.Quality = strings.Join(qualities, "-")
	match := inventoryBracketedReleaseGroup.FindStringSubmatch(stem)
	if len(match) == 2 {
		candidate := strings.TrimSpace(match[1])
		if !inventoryReleaseQuality.MatchString(candidate) && !inventoryProviderReleaseID.MatchString(candidate) {
			info.ReleaseGroup = candidate
		}
	}
	if info.ReleaseGroup == "" && len(info.Tags) > 0 {
		match = inventorySceneReleaseGroup.FindStringSubmatch(stem)
		if len(match) == 2 {
			candidate := strings.TrimSpace(match[1])
			switch strings.ToLower(candidate) {
			case "xvid", "x264", "x265", "h264", "h265", "av1", "hevc", "aac", "ac3", "eac3", "ddp", "dts", "truehd", "flac", "proper", "repack":
			default:
				info.ReleaseGroup = candidate
			}
		}
	}
	return info
}

func joinReleaseValue(existing, next string) string {
	if existing == "" {
		return next
	}
	return existing + "-" + next
}

// InventoryCollection walks a library once and returns a deterministic,
// read-only inventory. An Inbox is excluded from normal scans and is included
// only when inboxOnly is true.
func InventoryCollection(ctx context.Context, coll *Collection, inbox string, inboxOnly bool) ([]InventoryEntry, error) {
	if coll == nil || coll.Storage == nil {
		return nil, fmt.Errorf("collection storage is required")
	}
	inbox = strings.Trim(strings.TrimSpace(inbox), "/")
	var result []InventoryEntry
	var walk func(string) error
	walk = func(dir string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := coll.Storage.ReadDir(dir)
		if err != nil {
			return fmt.Errorf("list %q: %w", dir, err)
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, fi := range entries {
			name := fi.Name()
			rel := path.Join(dir, name)
			insideInbox := inbox != "" && (rel == inbox || strings.HasPrefix(rel, inbox+"/"))
			// Inbox-only scans must descend through ancestors of a nested Inbox,
			// without treating those ancestor directories as Inbox inventory.
			leadingToInbox := inboxOnly && inbox != "" && strings.HasPrefix(inbox, rel+"/")
			isInboxRoot := inboxOnly && rel == inbox
			if name == "" || (strings.HasPrefix(name, ".") && !isInboxRoot && !leadingToInbox) || strings.HasPrefix(name, "+ ") {
				continue
			}
			if fi.IsDir() {
				if (inboxOnly && !insideInbox && !leadingToInbox) || (!inboxOnly && insideInbox) {
					continue
				}
				if insideInbox {
					if format := discDirectoryFormat(name); format != "" && !inventoryExtraPath(rel) {
						pathIDs, pathConflict := providerIDsInPathWithConflict(rel)
						disc := InventoryEntry{Path: rel, IsDir: true, Role: InventoryDisc, DiscFormat: format, DiscRoot: rel, ProviderIDs: pathIDs, IdentityConflict: pathConflict, Reason: "disc structure recognized and inventoried separately; playback is not enabled"}
						movieNFO := path.Join(path.Dir(rel), "movie.nfo")
						if data, readErr := coll.Storage.ReadFile(movieNFO); readErr == nil {
							var conflict bool
							disc.ProviderIDs, conflict = mergeProviderIDsWithConflict(disc.ProviderIDs, metadata.NewNfoFromData(movieNFO, data).ProviderIDs())
							disc.IdentityConflict = disc.IdentityConflict || conflict
						}
						result = append(result, disc)
					}
				}
				if err := walk(rel); err != nil {
					return err
				}
				continue
			}
			if insideInbox != inboxOnly {
				continue
			}
			entry := classifyInventoryEntry(rel, fi.Size())
			entry.ModTime = fi.ModTime().UTC().UnixNano()
			if entry.Role == InventoryVideo {
				if coll.Type == CollectionTypeShows {
					filenameIDs := entry.ProviderIDs
					var pathConflict bool
					entry.ProviderIDs, pathConflict = providerIDsInPathWithConflict(path.Dir(rel))
					entry.IdentityConflict = entry.IdentityConflict || pathConflict
					entry.ProviderIDs, pathConflict = mergeProviderIDsWithConflict(entry.ProviderIDs, filenameIDs)
					entry.IdentityConflict = entry.IdentityConflict || pathConflict
				} else {
					var pathConflict bool
					entry.ProviderIDs, pathConflict = mergeProviderIDsWithConflict(entry.ProviderIDs, providerIDsInPath(rel))
					entry.IdentityConflict = entry.IdentityConflict || pathConflict
				}
				nfoPath := path.Join(path.Dir(rel), strings.TrimSuffix(path.Base(rel), path.Ext(rel))+".nfo")
				if data, readErr := coll.Storage.ReadFile(nfoPath); readErr == nil {
					ids := metadata.NewNfoFromData(nfoPath, data).ProviderIDs()
					if coll.Type == CollectionTypeShows {
						var conflict bool
						entry.EpisodeProviderIDs, conflict = mergeProviderIDsWithConflict(entry.EpisodeProviderIDs, ids)
						entry.IdentityConflict = entry.IdentityConflict || conflict
					} else {
						var conflict bool
						entry.ProviderIDs, conflict = mergeProviderIDsWithConflict(entry.ProviderIDs, ids)
						entry.IdentityConflict = entry.IdentityConflict || conflict
					}
				}
				videoDir := path.Dir(rel)
				if inventorySeasonDirectory.MatchString(path.Base(videoDir)) {
					showNFO := path.Join(path.Dir(videoDir), "tvshow.nfo")
					if data, readErr := coll.Storage.ReadFile(showNFO); readErr == nil {
						var conflict bool
						entry.ProviderIDs, conflict = mergeProviderIDsWithConflict(entry.ProviderIDs, metadata.NewNfoFromData(showNFO, data).ProviderIDs())
						entry.IdentityConflict = entry.IdentityConflict || conflict
					}
				}
			}
			result = append(result, entry)
		}
		return nil
	}
	if err := walk(""); err != nil {
		return nil, err
	}
	return result, nil
}

func inventoryExtraPath(name string) bool {
	for _, component := range strings.Split(name, "/") {
		switch strings.ToLower(component) {
		case "extras", "featurettes", "samples", "trailers":
			return true
		}
	}
	return false
}

func providerIDsInPath(name string) map[string]string {
	ids, _ := providerIDsInPathWithConflict(name)
	return ids
}

func providerIDsInPathWithConflict(name string) (map[string]string, bool) {
	ids := map[string]string{}
	conflict := false
	for _, component := range strings.Split(name, "/") {
		for _, match := range inventoryProviderID.FindAllStringSubmatch(component, -1) {
			key, value := strings.ToLower(match[1]), match[2]
			if prior := ids[key]; prior != "" && prior != value {
				conflict = true
			} else if prior == "" {
				ids[key] = value
			}
		}
	}
	return ids, conflict
}

func mergeProviderIDs(target, source map[string]string) map[string]string {
	if len(source) == 0 {
		return target
	}
	if target == nil {
		target = map[string]string{}
	}
	for provider, id := range source {
		if id != "" {
			target[strings.ToLower(provider)] = id
		}
	}
	return target
}

func mergeProviderIDsWithConflict(target, source map[string]string) (map[string]string, bool) {
	conflict := false
	for key, id := range source {
		if prior := target[strings.ToLower(key)]; prior != "" && id != "" && prior != id {
			conflict = true
		}
	}
	return mergeProviderIDs(target, source), conflict
}

func classifyInventoryEntry(name string, size int64) InventoryEntry {
	base := path.Base(name)
	ext := strings.ToLower(path.Ext(base))
	stem := strings.TrimSuffix(base, path.Ext(base))
	e := InventoryEntry{Path: name, Size: size, Extension: strings.TrimPrefix(ext, "."), Role: InventoryOther}
	components := strings.Split(name, "/")
	for i, component := range components {
		switch strings.ToLower(component) {
		case "extras", "featurettes", "samples", "trailers":
			e.Role = InventoryExtra
			e.Reason = "file is inside an extras directory"
			return e
		}
		if format := discDirectoryFormat(component); format != "" {
			e.Role = InventoryDisc
			e.DiscFormat = format
			e.DiscRoot = path.Join(components[:i+1]...)
			e.Reason = "disc structure recognized and inventoried separately; playback is not enabled"
			return e
		}
	}
	switch ext {
	case ".srt", ".vtt", ".ass", ".ssa", ".sub":
		e.Role = InventorySubtitle
		e.Reason = "recognized subtitle sidecar extension"
	case ".jpg", ".jpeg", ".png", ".tbn", ".webp":
		e.Role = InventoryArtwork
		e.Reason = "recognized artwork extension"
	case ".nfo":
		e.Role = InventoryNFO
		e.Reason = "recognized NFO metadata sidecar"
	default:
		if inventoryVideoExtensions[ext] {
			lower := strings.ToLower(stem)
			switch {
			case strings.Contains(lower, "trailer"):
				e.Role = InventoryTrailer
				e.Reason = "video filename contains trailer marker"
			case strings.Contains(lower, "featurette"), strings.Contains(lower, "behind the scenes"), strings.Contains(lower, "sample"):
				e.Role = InventoryExtra
				e.Reason = "video filename contains sample or featurette marker"
			default:
				e.Role = InventoryVideo
				e.Reason = "extension is on the supported video allowlist"
			}
		}
	}
	if e.Role != InventoryVideo {
		if e.Role == InventoryOther {
			e.Reason = "file extension or path does not match a recognized media role"
		}
		return e
	}
	release := ParseReleaseInfo(name)
	e.ReleaseTags, e.Quality, e.Resolution, e.Version, e.Edition, e.ReleaseGroup = release.Tags, release.Quality, release.Resolution, release.Version, release.Edition, release.ReleaseGroup
	title := stem
	if m := inventoryEpisode.FindStringSubmatchIndex(stem); len(m) > 0 {
		e.Season = atoi(stem[m[2]:m[3]])
		e.Episodes = append(e.Episodes, atoi(stem[m[4]:m[5]]))
		// Accept explicit E tokens (S01E01E02 and S01E01-E02-E03).
		// A bounded count and strictly increasing sequence keep malformed
		// release suffixes from being mistaken for episode numbers.
		rest := stem[m[1]:]
		validSequence := true
		// Common shorthand ranges omit the repeated E: S01E01-03 means
		// episodes 1, 2, and 3. Expand only increasing ranges of at most
		// eight total episodes; leave larger or malformed ranges for review.
		if strings.HasPrefix(rest, "-") {
			rangeValue := rest[1:]
			digits := 0
			for digits < len(rangeValue) && rangeValue[digits] >= '0' && rangeValue[digits] <= '9' && digits < 3 {
				digits++
			}
			if digits > 0 {
				if digits == 3 && digits < len(rangeValue) && rangeValue[digits] >= '0' && rangeValue[digits] <= '9' {
					validSequence = false
				} else {
					last, end := e.Episodes[len(e.Episodes)-1], atoi(rangeValue[:digits])
					if end <= last || end-last+len(e.Episodes) > 8 {
						validSequence = false
					} else {
						for episode := last + 1; episode <= end; episode++ {
							e.Episodes = append(e.Episodes, episode)
						}
						rest = rangeValue[digits:]
					}
				}
			}
		}
		for len(e.Episodes) < 8 {
			if !validSequence {
				break
			}
			if strings.HasPrefix(strings.ToLower(rest), "-e") {
				rest = rest[2:]
			} else if strings.HasPrefix(strings.ToLower(rest), "e") {
				rest = rest[1:]
			} else {
				break
			}
			n := 0
			for n < len(rest) && rest[n] >= '0' && rest[n] <= '9' && n < 3 {
				n++
			}
			if n == 0 {
				e.Episodes = nil
				break
			}
			e.Episodes = append(e.Episodes, atoi(rest[:n]))
			rest = rest[n:]
		}
		unparsedRange := len(rest) > 1 && rest[0] == '-' && rest[1] >= '0' && rest[1] <= '9'
		if !validSequence || unparsedRange || strings.HasPrefix(strings.ToLower(rest), "e") || strings.HasPrefix(strings.ToLower(rest), "-e") {
			e.Episodes = nil
		}
		valid := len(e.Episodes) > 0
		for i, episode := range e.Episodes {
			if episode <= 0 || (i > 0 && episode <= e.Episodes[i-1]) {
				valid = false
			}
		}
		if !valid {
			e.Episodes = nil
		}
		loc := inventoryEpisode.FindStringIndex(stem)
		if loc != nil {
			title = strings.Trim(strings.TrimSpace(stem[:loc[0]]), " ._-")
		}
		if len(e.Episodes) > 0 {
			e.Reason = "episode numbering parsed from filename"
		} else {
			e.Reason = "episode-like token is malformed or has an unsupported sequence"
		}
	} else if m := inventoryCrossEpisode.FindStringSubmatch(stem); len(m) > 0 {
		e.Season = atoi(m[1])
		if episode := atoi(m[2]); episode > 0 {
			e.Episodes = []int{episode}
			e.Reason = "season x episode numbering parsed from filename"
		} else {
			e.Reason = "season x episode token has an invalid episode number"
		}
		if loc := inventoryCrossEpisode.FindStringIndex(stem); loc != nil {
			title = strings.Trim(stem[:loc[0]], " ._-")
		}
	} else if m := inventoryDateEpisode.FindStringSubmatch(stem); len(m) > 0 {
		date := m[1] + "-" + m[2] + "-" + m[3]
		if _, err := time.Parse("2006-01-02", date); err == nil {
			e.Reason = "daily episode date parsed from filename"
			e.AirDate = date
			loc := inventoryDateEpisode.FindStringIndex(stem)
			if loc != nil {
				title = strings.Trim(stem[:loc[0]], " ._-")
			}
		} else {
			e.Reason = "date-like episode token is not a valid calendar date"
		}
	} else if m := inventoryNumericEpisode.FindStringSubmatch(stem); len(m) > 0 {
		loc := inventoryNumericEpisode.FindStringIndex(stem)
		yearLoc := inventoryYear.FindStringIndex(stem)
		if loc != nil && (yearLoc == nil || loc[0] != yearLoc[0] || loc[1] != yearLoc[1]) {
			if episode := atoi(m[2]); episode > 0 {
				e.Season = atoi(m[1])
				e.Episodes = []int{episode}
				e.Reason = "season and episode digits parsed from filename"
			} else {
				e.Reason = "season and episode token has an invalid episode number"
			}
			title = strings.Trim(stem[:loc[0]], " ._-")
		} else {
			if yearLoc != nil {
				e.Year = atoi(inventoryYear.FindStringSubmatch(stem)[1])
				if e.Year > 0 {
					titleLoc := inventoryYear.FindStringIndex(stem)
					if titleLoc != nil {
						title = strings.Trim(stem[:titleLoc[0]], " ._-()")
					}
				}
			}
			e.Reason = "supported video extension; title is filename-derived"
		}
	} else {
		if m := inventoryYear.FindStringSubmatch(stem); len(m) > 0 {
			e.Year = atoi(m[1])
			loc := inventoryYear.FindStringIndex(stem)
			if loc != nil {
				title = strings.Trim(stem[:loc[0]], " ._-()")
			}
		}
		if e.Year > 0 {
			e.Reason = "supported video extension; title and year parsed from filename"
		} else {
			e.Reason = "supported video extension; title is filename-derived"
		}
	}
	ids := map[string]string{}
	for _, m := range inventoryProviderID.FindAllStringSubmatch(stem, -1) {
		key, value := strings.ToLower(m[1]), m[2]
		if prior := ids[key]; prior != "" && prior != value {
			e.IdentityConflict = true
		} else if prior == "" {
			ids[key] = value
		}
	}
	if len(ids) > 0 {
		e.ProviderIDs = ids
	}
	e.Title = strings.Join(strings.Fields(strings.NewReplacer(".", " ", "_", " ").Replace(title)), " ")
	return e
}

func discDirectoryFormat(name string) string {
	switch {
	case strings.EqualFold(name, "VIDEO_TS"):
		return "dvd_video"
	case strings.EqualFold(name, "BDMV"):
		return "blu_ray"
	default:
		return ""
	}
}

// IsMovieDiscStructure reports whether a disc root contains the markers RCFin
// requires before treating the opaque tree as a movie item for metadata or
// organization. It does not make disc contents playable.
func IsMovieDiscStructure(root InventoryEntry, entries []InventoryEntry) bool {
	if root.Role != InventoryDisc || !root.IsDir {
		return false
	}
	rootPath := strings.ToLower(strings.TrimSuffix(root.Path, "/"))
	var dvdIFO, dvdTitleSet, blurayIndex, blurayPlaylist, blurayStream bool
	for _, entry := range entries {
		name := strings.ToLower(entry.Path)
		if !strings.HasPrefix(name, rootPath+"/") || entry.IsDir {
			continue
		}
		relative := strings.TrimPrefix(name, rootPath+"/")
		switch root.DiscFormat {
		case "dvd_video":
			if relative == "video_ts.ifo" {
				dvdIFO = true
			}
			if inventoryDVDTitleSetIFO.MatchString(relative) {
				dvdTitleSet = true
			}
		case "blu_ray":
			if relative == "index.bdmv" {
				blurayIndex = true
			}
			if strings.HasPrefix(relative, "playlist/") && strings.HasSuffix(relative, ".mpls") {
				blurayPlaylist = true
			}
			if strings.HasPrefix(relative, "stream/") && strings.HasSuffix(relative, ".m2ts") {
				blurayStream = true
			}
		}
	}
	switch root.DiscFormat {
	case "dvd_video":
		return dvdIFO && dvdTitleSet
	case "blu_ray":
		return blurayIndex && blurayPlaylist && blurayStream
	default:
		return false
	}
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}
