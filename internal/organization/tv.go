package organization

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/adhikariprivate/rcfin-server/internal/collection"
	"github.com/adhikariprivate/rcfin-server/internal/collection/metadata"
	"github.com/adhikariprivate/rcfin-server/internal/metadata/provider"
	"github.com/adhikariprivate/rcfin-server/internal/rclonestore"
)

var seasonDirectory = regexp.MustCompile(`(?i)^(?:season\s*0*(\d+)|s0*(\d+))$`)

// PlanInboxShows plans TV show directory moves and confident episode naming.
// Episode identities come from filenames or unique provider-confirmed air-date
// mappings; unsupported numbering remains in review.
func PlanInboxShows(ctx context.Context, coll *collection.Collection, p provider.Provider, opts Options) (Report, error) {
	return planInboxShows(ctx, coll, p, opts, nil)
}

func planInboxShows(ctx context.Context, coll *collection.Collection, p provider.Provider, opts Options, ready map[string]bool) (Report, error) {
	return planShowScope(ctx, coll, p, opts, ready, true)
}

// PlanExistingShows plans show and episode normalization outside the Inbox.
func PlanExistingShows(ctx context.Context, coll *collection.Collection, p provider.Provider, opts Options) (Report, error) {
	return planShowScope(ctx, coll, p, opts, nil, false)
}

func planExistingShows(ctx context.Context, coll *collection.Collection, p provider.Provider, opts Options, _ map[string]bool) (Report, error) {
	return planShowScope(ctx, coll, p, opts, nil, false)
}

func planShowScope(ctx context.Context, coll *collection.Collection, p provider.Provider, opts Options, ready map[string]bool, inboxOnly bool) (Report, error) {
	report := Report{CollectionID: "", DryRun: true}
	if coll == nil || coll.Storage == nil {
		return report, fmt.Errorf("collection storage is required")
	}
	report.CollectionID = coll.ID
	if coll.Type != collection.CollectionTypeShows {
		return report, fmt.Errorf("TV planning requires a shows collection")
	}
	e, err := scopedInventory(ctx, coll, opts, inboxOnly)
	if err != nil {
		return report, err
	}
	existing := e
	if inboxOnly {
		existing, err = scopedInventory(ctx, coll, opts, false)
		if err != nil {
			return report, err
		}
	}
	occupied := make(map[string]bool, len(existing))
	for _, entry := range existing {
		occupied[strings.ToLower(entry.Path)] = true
	}

	groups := map[string][]collection.InventoryEntry{}
	seasonPaths := map[string]map[string]int{}
	invalidLayout := map[string]bool{}
	unsupported := map[string][]string{}
	inboxRoot := strings.Trim(coll.InboxPath, "/")
	for _, entry := range e {
		if inboxOnly && entry.Role == collection.InventoryOther && !entry.IsDir {
			source := InboxItemRoot(entry.Path, coll.Type)
			if source == inboxRoot || source == "." {
				source = entry.Path
			}
			unsupported[source] = append(unsupported[source], entry.Path)
			continue
		}
		if entry.Role != collection.InventoryVideo {
			continue
		}
		videoDir := path.Dir(entry.Path)
		seasonDir := path.Base(videoDir)
		season, isSeason := parseSeasonDirectory(seasonDir)
		showDir := videoDir
		if isSeason {
			showDir = path.Dir(videoDir)
		}
		if showDir == "." || inboxOnly && (showDir == inboxRoot || !strings.HasPrefix(showDir, inboxRoot+"/")) {
			continue
		}
		groups[showDir] = append(groups[showDir], entry)
		if seasonPaths[showDir] == nil {
			seasonPaths[showDir] = map[string]int{}
		}
		if !isSeason || entry.Season > 0 && entry.Season != season {
			invalidLayout[showDir] = true
		} else {
			seasonPaths[showDir][videoDir] = season
		}
	}
	directories := make([]string, 0, len(groups))
	directorySet := map[string]bool{}
	for dir := range groups {
		directories = append(directories, dir)
		directorySet[dir] = true
	}
	for dir := range unsupported {
		if !directorySet[dir] {
			directories = append(directories, dir)
		}
	}
	sort.Strings(directories)
	plannedDestinations := map[string]string{}
	plannedFiles := map[string]string{}
	for _, source := range directories {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		item := ItemPlan{Source: source, StableItemID: stableItemIDAtPath(coll, source)}
		if paths := unsupported[source]; len(paths) > 0 {
			sort.Strings(paths)
			item.State = "needs_review"
			item.Reason = "Inbox item contains unrecognized files that must be reviewed: " + strings.Join(paths, ", ")
			report.Items = append(report.Items, item)
			continue
		}
		if ready != nil && !ready[source] {
			continue
		}
		if invalidLayout[source] {
			item.State, item.Reason = "needs_review", "TV files must use a recognized season directory and consistent season numbering"
			report.Items = append(report.Items, item)
			continue
		}
		videos := groups[source]
		sort.Slice(videos, func(i, j int) bool { return videos[i].Path < videos[j].Path })
		title, year := movieFolderIdentity(path.Base(source))
		ids := providerIDsInName(source)
		conflicting := false
		for _, video := range videos {
			for providerName, id := range video.ProviderIDs {
				if ids[providerName] != "" && ids[providerName] != id {
					conflicting = true
				}
				if id != "" {
					ids[providerName] = id
				}
			}
			if video.Title != "" && title == "" {
				title = video.Title
			}
			if video.Year != 0 && year == 0 {
				year = video.Year
			}
		}
		if data, readErr := coll.Storage.ReadFile(path.Join(source, "tvshow.nfo")); readErr == nil {
			ids = mergeIDs(ids, metadata.NewNfoFromData("tvshow.nfo", data).ProviderIDs(), &conflicting)
		}
		if conflicting {
			item.State, item.Reason = "needs_review", "show directory contains conflicting provider IDs"
			report.Items = append(report.Items, item)
			continue
		}
		if item.StableItemID == "" {
			item.StableItemID = coll.StableItemID(source, ids)
		}
		resolution, resolveErr := provider.Resolve(ctx, p, provider.Show, title, year, ids, opts.Language, opts.MinimumConfidence, opts.TieMargin)
		if resolution.Match != nil {
			item.ProviderID, item.Confidence = resolution.Match.Candidate.ID, resolution.Match.Confidence
		}
		item.Evidence = resolution.Evidence
		if resolveErr != nil || resolution.NeedsReview {
			item.State = "needs_review"
			if resolveErr != nil {
				item.Reason = resolveErr.Error()
			} else {
				item.Reason = "show identity is ambiguous or below the confidence threshold"
			}
			report.Items = append(report.Items, item)
			continue
		}
		if title == "" {
			title = resolution.Metadata.Title
		}
		if year == 0 {
			year = resolution.Metadata.Year
		}
		seasonProvider, ok := p.(provider.SeasonEpisodeProvider)
		if !ok {
			item.State, item.Reason = "needs_review", "metadata provider does not provide authoritative season episode names"
			report.Items = append(report.Items, item)
			continue
		}
		seriesFolder, err := renderFolderName(title, year, resolution.Metadata.ID, opts)
		if err != nil {
			item.State, item.Reason = "needs_review", err.Error()
			report.Items = append(report.Items, item)
			continue
		}
		grouping := path.Dir(source)
		if inboxOnly {
			relative := strings.TrimPrefix(source, inboxRoot+"/")
			grouping = path.Dir(relative)
		}
		if opts.FlattenGroupingDirectories {
			grouping = "."
		}
		destination := seriesFolder
		if grouping != "." {
			destination = path.Join(grouping, seriesFolder)
		}
		item.Destination = destination
		if prior, ok := plannedDestinations[strings.ToLower(destination)]; ok {
			item.State, item.Reason = "conflict", "destination also planned by "+prior
			report.Items = append(report.Items, item)
			continue
		}
		if _, statErr := coll.Storage.Stat(destination); statErr == nil {
			if destination != source {
				item.State, item.Reason = "conflict", "normalized show destination already exists"
				report.Items = append(report.Items, item)
				continue
			}
		} else if !rclonestore.IsNotFound(statErr) {
			item.State, item.Reason = "failed", "check destination: "+statErr.Error()
			report.Items = append(report.Items, item)
			continue
		}
		capabilities := coll.Storage.Capabilities()
		item.Capabilities = capabilities
		if capabilities.Local {
			item.Operation = "local_dir_move"
		} else {
			item.Operation = "staged_copy_verify_dir_move"
		}
		if !capabilities.DirMove {
			item.Operation = "unsupported"
			item.State, item.Reason = "needs_review", "backend must advertise DirMove for TV directory normalization"
			report.Items = append(report.Items, item)
			continue
		}
		if !capabilities.Local && !capabilities.Copy {
			item.Operation = "unsupported"
			item.State, item.Reason = "needs_review", "remote directory moves require Copy for staging and destination verification"
			item.PossibleCopyDelete = true
			report.Items = append(report.Items, item)
			continue
		}
		seasons := map[int][]provider.EpisodeMetadata{}
		seasonNumbers := map[int]bool{}
		for _, season := range seasonPaths[source] {
			seasonNumbers[season] = true
		}
		seasonList := make([]int, 0, len(seasonNumbers))
		for season := range seasonNumbers {
			seasonList = append(seasonList, season)
		}
		sort.Ints(seasonList)
		for _, season := range seasonList {
			rows, fetchErr := getSeasonEpisodes(ctx, seasonProvider, resolution.Metadata.ID, season, opts.Language, opts.FallbackLanguages)
			if fetchErr != nil {
				item.State, item.Reason = "needs_review", "load provider episode map: "+fetchErr.Error()
				break
			}
			seasons[season] = rows
		}
		if item.State == "needs_review" {
			report.Items = append(report.Items, item)
			continue
		}
		var treeBytes int64
		for _, entry := range e {
			if entry.Path == source || strings.HasPrefix(entry.Path, source+"/") {
				treeBytes += entry.Size
			}
		}
		if source != destination {
			item.Files = append(item.Files, FilePlan{Action: "move_directory:root", Source: source, Destination: destination, ExpectedBytes: treeBytes, Reason: "move the complete show tree after identity and destination checks"})
		}
		parent := path.Dir(destination)
		if parent != "." {
			if _, statErr := coll.Storage.Stat(parent); rclonestore.IsNotFound(statErr) {
				item.Files = append(item.Files, FilePlan{Action: "create_directory", Destination: parent, Reason: "preserve the Inbox grouping path"})
			} else if statErr != nil {
				item.State, item.Reason = "failed", "check destination parent: "+statErr.Error()
				report.Items = append(report.Items, item)
				continue
			}
		}
		seasonDestination := map[string]string{}
		seasonDirs := make([]string, 0, len(seasonPaths[source]))
		for seasonPath := range seasonPaths[source] {
			seasonDirs = append(seasonDirs, seasonPath)
		}
		sort.Strings(seasonDirs)
		for _, seasonPath := range seasonDirs {
			season := seasonPaths[source][seasonPath]
			newSeason := renderSeasonFolder(season, opts.SeasonFormat)
			newPath := path.Join(destination, newSeason)
			seasonDestination[seasonPath] = newPath
			if path.Base(seasonPath) != newSeason {
				var seasonBytes int64
				for _, entry := range e {
					if entry.Path == seasonPath || strings.HasPrefix(entry.Path, seasonPath+"/") {
						seasonBytes += entry.Size
					}
				}
				item.Files = append(item.Files, FilePlan{Action: "move_directory:season:" + path.Base(seasonPath), Source: path.Join(destination, path.Base(seasonPath)), Destination: newPath, ExpectedBytes: seasonBytes, Reason: "normalize season directory"})
			}
		}
		videoTargets := map[string]string{}
		localFileTargets := map[string]bool{}
		failed := false
		for _, video := range videos {
			season := seasonPaths[source][path.Dir(video.Path)]
			numbers := append([]int(nil), video.Episodes...)
			if len(numbers) == 0 && video.AirDate != "" {
				for _, episode := range seasons[season] {
					if episode.AirDate == video.AirDate {
						numbers = append(numbers, episode.EpisodeNumber)
					}
				}
				if len(numbers) != 1 {
					item.State, item.Reason = "needs_review", "air date does not map to exactly one provider episode in its season"
					failed = true
					break
				}
			}
			if len(numbers) == 0 || len(numbers) > 8 {
				item.State, item.Reason = "needs_review", "episode number is missing or uses an unsupported multi-episode pattern"
				failed = true
				break
			}
			validNumbers := true
			for i, number := range numbers {
				if number <= 0 || (i > 0 && number <= numbers[i-1]) {
					validNumbers = false
				}
			}
			if !validNumbers {
				item.State, item.Reason = "needs_review", "multi-episode numbering must be a strictly increasing list of at most eight episodes"
				failed = true
				break
			}
			if video.Season > 0 && video.Season != season {
				item.State, item.Reason = "needs_review", "filename season conflicts with its season directory"
				failed = true
				break
			}
			nameByNumber := map[int]string{}
			for _, episode := range seasons[season] {
				if episode.SeasonNumber == season {
					nameByNumber[episode.EpisodeNumber] = episode.Name
				}
			}
			var titles []string
			for _, number := range numbers {
				name := strings.TrimSpace(nameByNumber[number])
				if name == "" {
					item.State, item.Reason = "needs_review", fmt.Sprintf("provider has no authoritative title for season %d episode %d", season, number)
					failed = true
					break
				}
				titles = append(titles, name)
			}
			if failed {
				break
			}
			release := collection.ReleaseInfo{Tags: video.ReleaseTags, Quality: video.Quality, Resolution: video.Resolution, Version: video.Version, Edition: video.Edition, ReleaseGroup: video.ReleaseGroup}
			newBase, nameErr := renderEpisodeName(title, year, season, numbers, strings.Join(titles, " & "), path.Ext(video.Path), release, opts.EpisodeFormat)
			if nameErr != nil {
				item.State, item.Reason = "needs_review", nameErr.Error()
				failed = true
				break
			}
			destinationDir := seasonDestination[path.Dir(video.Path)]
			newVideoPath := path.Join(destinationDir, newBase)
			if localFileTargets[strings.ToLower(newVideoPath)] {
				item.State, item.Reason = "conflict", "multiple source videos normalize to the same episode filename"
				failed = true
				break
			}
			localFileTargets[strings.ToLower(newVideoPath)] = true
			videoTargets[video.Path] = newVideoPath
			currentPath := path.Join(destination, path.Base(path.Dir(video.Path)), path.Base(video.Path))
			if currentPath != newVideoPath {
				item.Files = append(item.Files, FilePlan{Action: "move_file", Source: currentPath, Destination: newVideoPath, Role: video.Role, ExpectedBytes: video.Size, Reason: "write authoritative season/episode numbering and provider episode title into filename"})
			}
		}
		if failed {
			report.Items = append(report.Items, item)
			continue
		}
		videoPaths := make([]string, 0, len(videoTargets))
		for original := range videoTargets {
			videoPaths = append(videoPaths, original)
		}
		sort.Strings(videoPaths)
		for _, sidecar := range e {
			if sidecar.IsDir || sidecar.Role != collection.InventorySubtitle && sidecar.Role != collection.InventoryNFO && sidecar.Role != collection.InventoryArtwork {
				continue
			}
			if path.Dir(path.Dir(sidecar.Path)) != source {
				continue
			}
			videoTarget := ""
			var sourceStem string
			for _, original := range videoPaths {
				target := videoTargets[original]
				originalStem := strings.TrimSuffix(path.Base(original), path.Ext(original))
				if path.Dir(original) == path.Dir(sidecar.Path) && strings.HasPrefix(path.Base(sidecar.Path), originalStem+".") {
					videoTarget, sourceStem = target, originalStem
					break
				}
			}
			if videoTarget == "" {
				continue
			}
			targetStem := strings.TrimSuffix(path.Base(videoTarget), path.Ext(videoTarget))
			suffix := strings.TrimPrefix(path.Base(sidecar.Path), sourceStem)
			newPath := path.Join(path.Dir(videoTarget), targetStem+suffix)
			currentPath := path.Join(destination, path.Base(path.Dir(sidecar.Path)), path.Base(sidecar.Path))
			if currentPath != newPath {
				item.Files = append(item.Files, FilePlan{Action: "move_file", Source: currentPath, Destination: newPath, Role: sidecar.Role, ExpectedBytes: sidecar.Size, Reason: "keep sidecar associated with renamed episode"})
			}
		}
		usesFileMove := false
		for _, file := range item.Files {
			if file.Action == "move_file" {
				usesFileMove = true
				break
			}
		}
		if usesFileMove && (!capabilities.Move || (!capabilities.Copy && !capabilities.Local)) {
			item.Operation = "unsupported"
			item.State, item.Reason = "needs_review", "file moves require Move and, for remote backends, Copy for verified staging"
			report.Items = append(report.Items, item)
			continue
		}
		collision := false
		plannedActionTargets := map[string]bool{}
		for _, file := range item.Files {
			if file.Action == "create_directory" || strings.HasPrefix(file.Action, "move_directory:") {
				continue
			}
			key := strings.ToLower(file.Destination)
			if plannedActionTargets[key] {
				collision = true
				item.Reason = "multiple planned files target the same path"
				break
			}
			plannedActionTargets[key] = true
			if localFileTargets[key] && file.Role != collection.InventoryVideo {
				// Sidecars must not overwrite a video or another sidecar target.
				collision = true
				item.Reason = "normalized sidecar destination collides with another planned file"
				break
			}
			if occupied[key] {
				collision = true
				item.Reason = "a destination file already exists"
				break
			}
			if prior, ok := plannedFiles[key]; ok && prior != source {
				collision = true
				item.Reason = "a destination file collides with another planned item"
				break
			}
			plannedFiles[key] = source
		}
		seasonDirs = seasonDirs[:0]
		for seasonPath := range seasonDestination {
			seasonDirs = append(seasonDirs, seasonPath)
		}
		sort.Strings(seasonDirs)
		for _, seasonPath := range seasonDirs {
			target := seasonDestination[seasonPath]
			if _, exists := coll.Storage.Stat(target); exists == nil && target != seasonPath {
				collision = true
				item.Reason = "normalized season destination already exists"
				break
			}
		}
		if collision {
			item.State = "conflict"
		} else if len(item.Files) == 0 {
			item.State, item.Reason = "unchanged", "show and episode paths already match the normalized layout"
		} else {
			item.State, item.Reason = "planned", "confident show and episode matches; dry-run plan only"
		}
		for _, file := range item.Files {
			if file.Action == "move_directory" || file.Action == "move_file" || strings.HasPrefix(file.Action, "move_directory:") {
				item.PossibleCopyDelete = !capabilities.Local
				break
			}
		}
		plannedDestinations[strings.ToLower(destination)] = source
		report.Items = append(report.Items, item)
	}
	return report, nil
}

func getSeasonEpisodes(ctx context.Context, seasonProvider provider.SeasonEpisodeProvider, showID string, season int, primary string, fallbacks []string) ([]provider.EpisodeMetadata, error) {
	if primary == "" {
		primary = "en-US"
	}
	languages := append([]string{primary}, fallbacks...)
	var merged []provider.EpisodeMetadata
	var lastErr error
	seenLanguages := map[string]bool{}
	for _, language := range languages {
		language = strings.TrimSpace(language)
		if language == "" || seenLanguages[strings.ToLower(language)] {
			continue
		}
		seenLanguages[strings.ToLower(language)] = true
		rows, err := seasonProvider.GetSeasonEpisodes(ctx, showID, season, language)
		if err != nil {
			lastErr = err
			continue
		}
		if len(merged) == 0 {
			merged = append(merged, rows...)
			continue
		}
		byNumber := make(map[int]int, len(merged))
		for i := range merged {
			byNumber[merged[i].EpisodeNumber] = i
		}
		for _, row := range rows {
			if i, ok := byNumber[row.EpisodeNumber]; ok {
				if merged[i].Name == "" {
					merged[i].Name = row.Name
				}
				if merged[i].ID == "" {
					merged[i].ID = row.ID
				}
				if merged[i].AirDate == "" {
					merged[i].AirDate = row.AirDate
				}
				if merged[i].Overview == "" {
					merged[i].Overview = row.Overview
				}
			} else {
				merged = append(merged, row)
			}
		}
	}
	if len(merged) == 0 && lastErr != nil {
		return nil, lastErr
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].EpisodeNumber < merged[j].EpisodeNumber })
	return merged, nil
}

func parseSeasonDirectory(name string) (int, bool) {
	if strings.EqualFold(name, "specials") {
		return 0, true
	}
	match := seasonDirectory.FindStringSubmatch(name)
	if len(match) == 0 {
		return 0, false
	}
	value := match[1]
	if value == "" {
		value = match[2]
	}
	season, err := strconv.Atoi(value)
	return season, err == nil
}

func providerIDsInName(name string) map[string]string {
	ids := map[string]string{}
	for _, match := range inventoryProviderIDRegexp.FindAllStringSubmatch(name, -1) {
		ids[strings.ToLower(match[1])] = match[2]
	}
	return ids
}

var inventoryProviderIDRegexp = regexp.MustCompile(`(?i)(tmdb|tvdb|imdb)id[-_ ]?([a-z0-9]+)`)

func mergeIDs(target, incoming map[string]string, conflict *bool) map[string]string {
	if target == nil {
		target = map[string]string{}
	}
	for key, value := range incoming {
		key = strings.ToLower(key)
		if target[key] != "" && value != "" && target[key] != value {
			*conflict = true
		}
		if target[key] == "" && value != "" {
			target[key] = value
		}
	}
	return target
}

func renderSeasonFolder(season int, format string) string {
	if strings.EqualFold(format, "specials") && season == 0 {
		return "Specials"
	}
	if season == 0 {
		return "Specials"
	}
	if format == "" {
		return fmt.Sprintf("Season %02d", season)
	}
	result, err := renderTemplate(format, map[string]string{"season": fmt.Sprintf("%02d", season), "season_number": strconv.Itoa(season)})
	if err != nil {
		return fmt.Sprintf("Season %02d", season)
	}
	return safeSegment(result)
}

func renderEpisodeName(series string, year, season int, episodes []int, title, extension string, release collection.ReleaseInfo, format string) (string, error) {
	seasonEpisode := fmt.Sprintf("S%02dE%02d", season, episodes[0])
	for _, episode := range episodes[1:] {
		seasonEpisode += fmt.Sprintf("-E%02d", episode)
	}
	releaseSuffix := ""
	if len(release.Tags) > 0 {
		cleanLabels := make([]string, 0, len(release.Tags))
		for _, label := range release.Tags {
			cleanLabels = append(cleanLabels, safeSegment(label))
		}
		sort.Strings(cleanLabels)
		releaseSuffix = " [" + strings.Join(cleanLabels, "-") + "]"
	}
	if release.ReleaseGroup != "" {
		releaseSuffix += " [" + safeSegment(release.ReleaseGroup) + "]"
	}
	if format == "" {
		return safeSegment(series) + " - " + seasonEpisode + " - " + safeSegment(title) + releaseSuffix + strings.ToLower(extension), nil
	}
	formatted, err := renderTemplate(format, map[string]string{"series": series, "year": strconv.Itoa(year), "season": fmt.Sprintf("%02d", season), "episode": seasonEpisode, "episode_title": title, "quality": release.Quality, "resolution": release.Resolution, "version": release.Version, "edition": release.Edition, "release_group": release.ReleaseGroup, "ext": strings.TrimPrefix(strings.ToLower(extension), ".")})
	if err != nil {
		return "", err
	}
	formatted = strings.ReplaceAll(formatted, "-]", "]")
	formatted = strings.ReplaceAll(formatted, "[-", "[")
	formatted = strings.ReplaceAll(formatted, "[]", "")
	return safeSegment(formatted), nil
}
