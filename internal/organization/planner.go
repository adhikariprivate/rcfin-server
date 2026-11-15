// Package organization builds deterministic, read-only normalization plans.
package organization

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/adhikariprivate/rcfin-server/internal/collection"
	"github.com/adhikariprivate/rcfin-server/internal/metadata/provider"
	"github.com/adhikariprivate/rcfin-server/internal/rclonestore"
)

type Options struct {
	Inventory                  []collection.InventoryEntry
	ItemRoots                  []string
	IncludeProviderID          bool
	EnableDiscFolders          bool
	FlattenGroupingDirectories bool
	MinimumConfidence          float64
	TieMargin                  float64
	FolderFormat               string
	FileFormat                 string
	SeasonFormat               string
	EpisodeFormat              string
	Language                   string
	FallbackLanguages          []string
	AllowedExtensions          []string
	StableFor                  time.Duration
	CompletionMarker           string
}

func scopedInventory(ctx context.Context, coll *collection.Collection, opts Options, inboxOnly bool) ([]collection.InventoryEntry, error) {
	var entries []collection.InventoryEntry
	var err error
	if opts.Inventory != nil {
		entries = collection.InventoryScopeEntries(opts.Inventory, inboxOnly)
	} else {
		entries, err = collection.InventoryCollection(ctx, coll, coll.InboxPath, inboxOnly)
		if err != nil {
			return nil, err
		}
	}
	if !inboxOnly || len(opts.ItemRoots) == 0 {
		return entries, nil
	}
	filtered := make([]collection.InventoryEntry, 0, len(entries))
	for _, entry := range entries {
		for _, root := range opts.ItemRoots {
			if entry.Path == root || strings.HasPrefix(entry.Path, root+"/") {
				filtered = append(filtered, entry)
				break
			}
		}
	}
	return filtered, nil
}

type FilePlan struct {
	Action        string                   `json:"action,omitempty"`
	Source        string                   `json:"source"`
	Destination   string                   `json:"destination"`
	Role          collection.InventoryRole `json:"role"`
	ExpectedBytes int64                    `json:"expected_bytes,omitempty"`
	Reason        string                   `json:"reason,omitempty"`
}
type ItemPlan struct {
	Source       string           `json:"source"`
	Destination  string           `json:"destination,omitempty"`
	StableItemID string           `json:"stable_item_id,omitempty"`
	ProviderID   string           `json:"provider_id,omitempty"`
	Confidence   float64          `json:"confidence,omitempty"`
	Evidence     []string         `json:"evidence,omitempty"`
	Operation    string           `json:"operation,omitempty"`
	Capabilities CapabilityReport `json:"capabilities,omitempty"`
	// PossibleCopyDelete is nil when Rclone does not expose whether a backend's
	// Move or DirMove is implemented as server-side rename or copy/delete.
	PossibleCopyDelete *bool      `json:"possible_copy_delete"`
	MoveSemantics      string     `json:"move_semantics,omitempty"`
	State              string     `json:"state"`
	Reason             string     `json:"reason,omitempty"`
	Files              []FilePlan `json:"files,omitempty"`
}
type Report struct {
	CollectionID string     `json:"collection_id"`
	DryRun       bool       `json:"dry_run"`
	Items        []ItemPlan `json:"items"`
}

// PlanInboxMovies resolves each Inbox movie directory and describes its
// normalized location. It does not mutate storage.
func PlanInboxMovies(ctx context.Context, coll *collection.Collection, p provider.Provider, opts Options) (Report, error) {
	return planReadyInboxMovies(ctx, coll, p, opts, nil)
}

func planReadyInboxMovies(ctx context.Context, coll *collection.Collection, p provider.Provider, opts Options, ready map[string]bool) (Report, error) {
	return planMovieScope(ctx, coll, p, opts, ready, true)
}

// PlanExistingMovies plans normalization for library paths outside the Inbox.
// It is intentionally separate from Inbox processing and performs no writes.
func PlanExistingMovies(ctx context.Context, coll *collection.Collection, p provider.Provider, opts Options) (Report, error) {
	return planMovieScope(ctx, coll, p, opts, nil, false)
}

func planExistingMovies(ctx context.Context, coll *collection.Collection, p provider.Provider, opts Options, _ map[string]bool) (Report, error) {
	return planMovieScope(ctx, coll, p, opts, nil, false)
}

func planMovieScope(ctx context.Context, coll *collection.Collection, p provider.Provider, opts Options, ready map[string]bool, inboxOnly bool) (Report, error) {
	report := Report{DryRun: true}
	if coll == nil || coll.Storage == nil {
		return report, fmt.Errorf("collection storage is required")
	}
	report.CollectionID = coll.ID
	if coll.Type != collection.CollectionTypeMovies {
		return report, fmt.Errorf("movie planning requires a movies collection")
	}
	entries, err := scopedInventory(ctx, coll, opts, inboxOnly)
	if err != nil {
		return report, err
	}
	occupied, err := scopedInventory(ctx, coll, opts, false)
	if err != nil {
		return report, err
	}
	occupiedPaths := map[string]bool{}
	for _, e := range occupied {
		occupiedPaths[strings.ToLower(e.Path)] = true
	}
	groups := map[string][]collection.InventoryEntry{}
	discGroups := map[string][]collection.InventoryEntry{}
	unsupported := map[string][]string{}
	inboxRoot := strings.Trim(coll.InboxPath, "/")
	for _, e := range entries {
		if e.Role == collection.InventoryVideo {
			groups[path.Dir(e.Path)] = append(groups[path.Dir(e.Path)], e)
		} else if opts.EnableDiscFolders && e.Role == collection.InventoryDisc && e.IsDir {
			discGroups[path.Dir(e.Path)] = append(discGroups[path.Dir(e.Path)], e)
		} else if inboxOnly && e.Role == collection.InventoryOther && !e.IsDir {
			source := InboxItemRoot(e.Path, coll.Type)
			if source == inboxRoot || source == "." {
				source = e.Path
			}
			unsupported[source] = append(unsupported[source], e.Path)
		}
	}
	directories := make([]string, 0, len(groups)+len(discGroups))
	directorySet := map[string]bool{}
	for dir := range groups {
		directories = append(directories, dir)
		directorySet[dir] = true
	}
	for dir := range discGroups {
		if !directorySet[dir] {
			directories = append(directories, dir)
		}
	}
	for dir := range unsupported {
		if !directorySet[dir] {
			directories = append(directories, dir)
			directorySet[dir] = true
		}
	}
	sort.Strings(directories)
	planned := map[string]string{}
	plannedFiles := map[string]string{}
	for _, sourceDir := range directories {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		videos := groups[sourceDir]
		sort.Slice(videos, func(i, j int) bool { return videos[i].Path < videos[j].Path })
		discs := discGroups[sourceDir]
		sort.Slice(discs, func(i, j int) bool { return discs[i].Path < discs[j].Path })
		plan := ItemPlan{Source: sourceDir, StableItemID: stableItemIDAtPath(coll, sourceDir)}
		if paths := unsupported[sourceDir]; len(paths) > 0 {
			sort.Strings(paths)
			plan.State = "needs_review"
			plan.Reason = "Inbox item contains unrecognized files that must be reviewed: " + strings.Join(paths, ", ")
			report.Items = append(report.Items, plan)
			continue
		}
		if ready != nil && !ready[sourceDir] {
			continue
		}
		if inboxOnly && sourceDir == strings.Trim(coll.InboxPath, "/") || sourceDir == "." {
			plan.State = "needs_review"
			plan.Reason = "Inbox must contain one movie per child directory"
			report.Items = append(report.Items, plan)
			continue
		}
		if len(discs) > 0 && (len(discs) != 1 || len(videos) > 0) {
			plan.State = "needs_review"
			plan.Reason = "movie folder mixes disc structures, has multiple disc roots, or also contains ordinary videos"
			report.Items = append(report.Items, plan)
			continue
		}
		if len(videos) == 0 && len(discs) == 0 {
			continue
		}
		if len(discs) == 1 && !collection.IsMovieDiscStructure(discs[0], entries) {
			plan.State = "needs_review"
			plan.Reason = "disc directory is missing required DVD or Blu-ray structure markers"
			report.Items = append(report.Items, plan)
			continue
		}
		title, year := movieFolderIdentity(path.Base(sourceDir))
		if title == "" && len(videos) > 0 {
			title = videos[0].Title
		}
		if year == 0 && len(videos) > 0 {
			year = videos[0].Year
		}
		ids := map[string]string{}
		conflictingIDs := false
		identityEntries := append(append([]collection.InventoryEntry(nil), videos...), discs...)
		for _, entry := range identityEntries {
			for key, id := range entry.ProviderIDs {
				if prior := ids[key]; prior != "" && prior != id {
					conflictingIDs = true
				} else if id != "" {
					ids[key] = id
				}
			}
		}
		if conflictingIDs {
			plan.State = "needs_review"
			plan.Reason = "movie directory contains files with conflicting provider IDs"
			report.Items = append(report.Items, plan)
			continue
		}
		if plan.StableItemID == "" {
			plan.StableItemID = coll.StableItemID(sourceDir, ids)
		}
		language := opts.Language
		if language == "" {
			language = "en-US"
		}
		resolution, resolveErr := provider.Resolve(ctx, p, provider.Movie, title, year, ids, language, opts.MinimumConfidence, opts.TieMargin)
		if resolution.Match != nil {
			plan.ProviderID = resolution.Match.Candidate.ID
			plan.Confidence = resolution.Match.Confidence
		}
		plan.Evidence = resolution.Evidence
		if resolveErr != nil {
			plan.State = "needs_review"
			plan.Reason = resolveErr.Error()
			report.Items = append(report.Items, plan)
			continue
		}
		if resolution.NeedsReview {
			plan.State = "needs_review"
			plan.Reason = "movie identity is ambiguous or below the confidence threshold"
			report.Items = append(report.Items, plan)
			continue
		}
		if title == "" {
			title = resolution.Metadata.Title
		}
		if year == 0 {
			year = resolution.Metadata.Year
		}
		providerID := resolution.Metadata.ID
		folder, formatErr := renderFolderName(title, year, providerID, opts)
		if formatErr != nil {
			plan.State = "needs_review"
			plan.Reason = formatErr.Error()
			report.Items = append(report.Items, plan)
			continue
		}
		destination := folder
		grouping := path.Dir(sourceDir)
		if inboxOnly {
			inboxRoot := strings.Trim(coll.InboxPath, "/")
			relativeDir := strings.TrimPrefix(sourceDir, inboxRoot+"/")
			grouping = path.Dir(relativeDir)
		}
		if opts.FlattenGroupingDirectories {
			grouping = "."
		}
		if grouping != "." {
			destination = path.Join(grouping, folder)
		}
		plan.Destination = destination
		if _, statErr := coll.Storage.Stat(destination); statErr == nil {
			if destination != sourceDir {
				plan.State = "conflict"
				plan.Reason = "normalized destination directory already exists"
				report.Items = append(report.Items, plan)
				continue
			}
		} else if !rclonestore.IsNotFound(statErr) {
			plan.State = "failed"
			plan.Reason = "check destination: " + statErr.Error()
			report.Items = append(report.Items, plan)
			continue
		}
		if destination != sourceDir && occupiedDestination(occupiedPaths, destination) {
			plan.State = "conflict"
			plan.Reason = "normalized destination already exists"
			report.Items = append(report.Items, plan)
			continue
		}
		if prior, ok := planned[strings.ToLower(destination)]; ok {
			plan.State = "conflict"
			plan.Reason = "destination also planned by " + prior
			report.Items = append(report.Items, plan)
			continue
		}
		planned[strings.ToLower(destination)] = sourceDir
		capabilities := coll.Storage.Capabilities()
		plan.Capabilities = capabilities
		// A directory move preserves sidecars and extras. Video renames are then
		// planned only when parsed quality/version labels keep each target unique.
		if sourceDir != destination {
			var treeBytes int64
			for _, entry := range entries {
				if entry.Path == sourceDir || strings.HasPrefix(entry.Path, sourceDir+"/") {
					treeBytes += entry.Size
				}
			}
			plan.Files = append(plan.Files, FilePlan{Action: "move_directory", Source: sourceDir, Destination: destination, ExpectedBytes: treeBytes, Reason: "move the movie directory as a unit; retain unrelated files and sidecars"})
		}
		parent := path.Dir(destination)
		if parent != "." {
			if _, parentErr := coll.Storage.Stat(parent); rclonestore.IsNotFound(parentErr) {
				plan.Files = append(plan.Files, FilePlan{Action: "create_directory", Destination: parent, Reason: "preserve the source grouping path in the library"})
			} else if parentErr != nil {
				plan.State = "failed"
				plan.Reason = "check destination parent: " + parentErr.Error()
				report.Items = append(report.Items, plan)
				continue
			}
		}
		newNames := map[string]string{}
		if len(videos) > 0 {
			newNames = planVideoNames(videos, folder, opts.FileFormat, opts.AllowedExtensions)
		}
		if newNames == nil {
			plan.State = "needs_review"
			plan.Reason = "video versions cannot be renamed without collisions or invented labels"
			report.Items = append(report.Items, plan)
			continue
		}
		for _, entry := range entries {
			if path.Dir(entry.Path) != sourceDir {
				continue
			}
			newBase, ok := newNames[path.Base(entry.Path)]
			if !ok {
				continue
			}
			newVideoPath := path.Join(destination, newBase)
			if path.Base(entry.Path) != newBase {
				plan.Files = append(plan.Files, FilePlan{Action: "move_file", Source: entry.Path, Destination: newVideoPath, Role: entry.Role, ExpectedBytes: entry.Size, Reason: "normalize video basename while preserving parsed release/version labels"})
			}
			oldStem := strings.TrimSuffix(path.Base(entry.Path), path.Ext(entry.Path))
			newStem := strings.TrimSuffix(newBase, path.Ext(newBase))
			for _, sidecar := range entries {
				if sidecar.Path == entry.Path || path.Dir(sidecar.Path) != sourceDir {
					continue
				}
				sidecarBase := path.Base(sidecar.Path)
				if strings.HasPrefix(sidecarBase, oldStem+".") {
					suffix := strings.TrimPrefix(sidecarBase, oldStem)
					newSidecar := newStem + suffix
					if path.Base(sidecar.Path) != newSidecar {
						plan.Files = append(plan.Files, FilePlan{Action: "move_file", Source: sidecar.Path, Destination: path.Join(destination, newSidecar), Role: sidecar.Role, ExpectedBytes: sidecar.Size, Reason: "keep sidecar associated with renamed video"})
					}
				}
			}
		}
		usesFileMove, usesDirectoryMove := false, false
		for _, file := range plan.Files {
			if file.Action == "move_file" {
				usesFileMove = true
			} else if file.Action == "move_directory" || strings.HasPrefix(file.Action, "move_directory:") {
				usesDirectoryMove = true
			}
		}
		if usesDirectoryMove && !capabilities.DirMove {
			plan.State = "needs_review"
			plan.Operation = "unsupported"
			plan.Reason = "backend does not advertise DirMove; directory organization will not fall back to copy/delete"
			report.Items = append(report.Items, plan)
			continue
		}
		if usesFileMove && !capabilities.Move {
			plan.State = "needs_review"
			plan.Operation = "unsupported"
			plan.Reason = "backend does not advertise Move; file organization will not fall back to copy/delete"
			report.Items = append(report.Items, plan)
			continue
		}
		if usesDirectoryMove {
			plan.Operation = "backend_dir_move"
		} else if usesFileMove {
			plan.Operation = "backend_move"
		} else {
			plan.Operation = "none"
		}
		if usesDirectoryMove || usesFileMove {
			if capabilities.Local {
				plan.MoveSemantics = "local_rename"
				plan.PossibleCopyDelete = boolPointer(false)
			} else {
				// Rclone exposes no generic field describing whether a backend's
				// Move/DirMove uses server-side rename or copy/delete.
				plan.MoveSemantics = "backend_defined"
			}
		}
		collision := false
		for _, file := range plan.Files {
			target := strings.ToLower(file.Destination)
			if occupiedPaths[target] {
				collision = true
				plan.Reason = "a destination file already exists"
				break
			}
			if other, ok := plannedFiles[target]; ok && other != sourceDir {
				collision = true
				plan.Reason = "a destination file collides with another planned item"
				break
			}
		}
		if collision {
			plan.State = "conflict"
		} else {
			if len(plan.Files) == 0 {
				plan.State = "unchanged"
				plan.Reason = "movie path already matches the normalized layout"
			} else {
				plan.State = "planned"
				plan.Reason = "confident movie match; dry-run plan only"
			}
		}
		report.Items = append(report.Items, plan)
		if plan.State == "planned" {
			for _, file := range plan.Files {
				if file.Action != "create_directory" {
					plannedFiles[strings.ToLower(file.Destination)] = sourceDir
				}
			}
		}
	}
	return report, nil
}

func boolPointer(value bool) *bool { return &value }

func stableItemIDAtPath(coll *collection.Collection, itemPath string) string {
	if coll == nil {
		return ""
	}
	for _, item := range coll.Items {
		if item != nil && item.Path() == itemPath {
			return item.ID()
		}
	}
	return ""
}

func occupiedDestination(occupied map[string]bool, destination string) bool {
	destination = strings.ToLower(strings.Trim(destination, "/"))
	for existing := range occupied {
		if existing == destination || strings.HasPrefix(existing, destination+"/") {
			return true
		}
	}
	return false
}

var folderYear = regexp.MustCompile(`\s*\((19\d{2}|20\d{2})\)\s*$`)
var providerTag = regexp.MustCompile(`\s*\[(?:tmdb|tvdb|imdb)id-[^]]+\]\s*$`)

func movieFolderIdentity(folder string) (string, int) {
	folder = providerTag.ReplaceAllString(folder, "")
	year := 0
	if match := folderYear.FindStringSubmatch(folder); len(match) > 0 {
		for _, r := range match[1] {
			year = year*10 + int(r-'0')
		}
		folder = folderYear.ReplaceAllString(folder, "")
	}
	folder = strings.NewReplacer(".", " ", "_", " ").Replace(folder)
	return strings.Join(strings.Fields(folder), " "), year
}

func planVideoNames(videos []collection.InventoryEntry, folder, fileFormat string, allowedExtensions []string) map[string]string {
	if len(videos) == 0 {
		return nil
	}
	result := map[string]string{}
	outputs := map[string]bool{}
	for _, video := range videos {
		base := path.Base(video.Path)
		ext := path.Ext(base)
		if len(allowedExtensions) > 0 && !containsExtension(allowedExtensions, strings.TrimPrefix(strings.ToLower(ext), ".")) {
			return nil
		}
		release := collection.ReleaseInfo{Tags: video.ReleaseTags, Quality: video.Quality, Resolution: video.Resolution, Version: video.Version, Edition: video.Edition, ReleaseGroup: video.ReleaseGroup}
		if len(release.Tags) == 0 && release.Quality == "" && release.Resolution == "" && release.Version == "" && release.Edition == "" && release.ReleaseGroup == "" {
			release = collection.ParseReleaseInfo(base)
		}
		labels := release.Tags
		if len(videos) > 1 && len(labels) == 0 && release.ReleaseGroup == "" {
			return nil
		}
		name := folder
		if fileFormat != "" {
			formatted, err := renderTemplate(fileFormat, map[string]string{"folder": folder, "quality": release.Quality, "resolution": release.Resolution, "version": release.Version, "edition": release.Edition, "release_group": release.ReleaseGroup, "ext": strings.TrimPrefix(strings.ToLower(ext), ".")})
			if err != nil {
				return nil
			}
			formatted = strings.ReplaceAll(formatted, "-]", "]")
			formatted = strings.ReplaceAll(formatted, "[-", "[")
			formatted = strings.ReplaceAll(formatted, "[]", "")
			name = safeSegment(formatted)
		} else {
			if len(labels) > 0 {
				name += " [" + strings.Join(labels, "-") + "]"
			}
			if release.ReleaseGroup != "" {
				name += " [" + safeSegment(release.ReleaseGroup) + "]"
			}
			name += strings.ToLower(ext)
		}
		if name == "" {
			return nil
		}
		if outputs[strings.ToLower(name)] {
			return nil
		}
		outputs[strings.ToLower(name)] = true
		result[base] = name
	}
	return result
}

func renderFolderName(title string, year int, providerID string, opts Options) (string, error) {
	if opts.FolderFormat == "" {
		folder := safeSegment(title)
		if year > 0 {
			folder += fmt.Sprintf(" (%d)", year)
		}
		if opts.IncludeProviderID && providerID != "" {
			folder += " [tmdbid-" + safeSegment(providerID) + "]"
		}
		return folder, nil
	}
	values := map[string]string{"title": title, "year": "", "tmdb_id": ""}
	if year > 0 {
		values["year"] = fmt.Sprint(year)
	}
	if providerID != "" {
		values["tmdb_id"] = providerID
	}
	folder, err := renderTemplate(opts.FolderFormat, values)
	if err != nil {
		return "", err
	}
	folder = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(folder, "()", ""), "[]", ""))
	folder = safeSegment(folder)
	if folder == "" {
		return "", fmt.Errorf("folder template produced an empty name")
	}
	return folder, nil
}

var templateToken = regexp.MustCompile(`\{([a-z_]+)\}`)
var segmentPunctuationSpace = regexp.MustCompile(`\s+([.,])`)

func renderTemplate(format string, values map[string]string) (string, error) {
	var invalid string
	result := templateToken.ReplaceAllStringFunc(format, func(token string) string {
		key := strings.Trim(token, "{}")
		value, ok := values[key]
		if !ok {
			invalid = key
			return ""
		}
		return value
	})
	if invalid != "" {
		return "", fmt.Errorf("unsupported naming template token {%s}", invalid)
	}
	if strings.ContainsAny(result, "{}") {
		return "", fmt.Errorf("malformed naming template")
	}
	return result, nil
}

func containsExtension(configured []string, ext string) bool {
	for _, v := range configured {
		if strings.EqualFold(strings.TrimPrefix(v, "."), ext) {
			return true
		}
	}
	return false
}

func safeSegment(value string) string {
	value = strings.TrimSpace(value)
	value = strings.ReplaceAll(value, "..", "")
	// Optional naming-template fields can disappear while leaving a separator
	// before punctuation (for example, "Episode Title .mkv"). Normalize that
	// whitespace before collapsing the remaining spaces.
	value = segmentPunctuationSpace.ReplaceAllString(value, "$1")
	var out strings.Builder
	for _, r := range value {
		if r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' || r == '"' || r == '<' || r == '>' || r == '|' || r < 32 {
			out.WriteByte(' ')
		} else {
			out.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(out.String()), " ")
}

// Capabilities is exported through Library and remains the sole basis for
// reporting which operation a backend can perform.
type CapabilityReport = rclonestore.Capabilities
