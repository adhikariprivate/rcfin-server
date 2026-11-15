package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path"
	"strings"
	"time"

	"github.com/adhikariprivate/rcfin-server/internal/collection"
	"github.com/adhikariprivate/rcfin-server/internal/database"
	"github.com/adhikariprivate/rcfin-server/internal/metadata/provider"
	metadatarefresh "github.com/adhikariprivate/rcfin-server/internal/metadata/refresh"
	"github.com/adhikariprivate/rcfin-server/internal/organization"
	"github.com/adhikariprivate/rcfin-server/internal/subtitles"
)

func libraryScanInterval(config configFile, library collectionConfig) (time.Duration, error) {
	value := strings.TrimSpace(library.Media.ScanInterval)
	if value == "" {
		value = strings.TrimSpace(config.ScanInterval)
	}
	if value == "" {
		value = strings.TrimSpace(config.RescanInterval)
	}
	if value == "" {
		return 30 * time.Minute, nil
	}
	interval, err := time.ParseDuration(value)
	if err != nil || interval <= 0 {
		return 0, fmt.Errorf("invalid scan interval %q", value)
	}
	return interval, nil
}

func currentInventoryProcessingVersions() collection.InventoryProcessingVersions {
	build := version
	if commit != "" {
		build += "+" + commit
	}
	return collection.InventoryProcessingVersions{
		ServerVersion: build,
		AlgorithmVersions: map[string]int{
			"metadata":  metadatarefresh.AlgorithmVersion,
			"subtitles": subtitles.AlgorithmVersion,
		},
	}
}

// libraryAutomationLoop owns the recurring inventory cadence. Each cycle
// lists a library and Inbox once, then runs Inbox provisioning, metadata, and
// subtitles in that order from the same snapshot.
func libraryAutomationLoop(ctx context.Context, collections *collection.CollectionRepo, db database.Repository, config configFile) {
	next := make(map[string]time.Time, len(config.Collections))
	intervals := make(map[string]time.Duration, len(config.Collections))
	for _, library := range config.Collections {
		interval, err := libraryScanInterval(config, library)
		if err != nil {
			log.Printf("library automation for %q disabled: %v", library.ID, err)
			continue
		}
		intervals[library.ID] = interval
	}
	for {
		now := time.Now()
		var nextRun time.Time
		for _, library := range config.Collections {
			interval, enabled := intervals[library.ID]
			if !enabled || now.Before(next[library.ID]) {
				if enabled && (nextRun.IsZero() || next[library.ID].Before(nextRun)) {
					nextRun = next[library.ID]
				}
				continue
			}
			if ctx.Err() != nil {
				return
			}
			if err := runLibraryAutomationCycle(ctx, collections, db, library); err != nil && ctx.Err() == nil {
				log.Printf("library automation for %q failed: %v", library.ID, err)
			}
			next[library.ID] = time.Now().Add(interval)
			candidate := next[library.ID]
			if nextRun.IsZero() || candidate.Before(nextRun) {
				nextRun = candidate
			}
		}
		if ctx.Err() != nil {
			return
		}
		if nextRun.IsZero() {
			<-ctx.Done()
			return
		}
		delay := time.Until(nextRun)
		if delay < 0 {
			delay = 0
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func runLibraryAutomationCycle(ctx context.Context, collections *collection.CollectionRepo, db database.Repository, library collectionConfig) error {
	coll := collections.GetCollection(library.ID)
	if coll == nil {
		return fmt.Errorf("collection is not loaded")
	}
	if err := organization.RecoverPendingOperations(ctx, coll, db, ""); err != nil {
		return fmt.Errorf("recover pending organization operations: %w", err)
	}
	if stagingChanged, err := organization.ProcessPendingInboxStaging(ctx, coll, db, ""); err != nil {
		log.Printf("administrator Inbox staging for collection %q failed: %v", library.ID, err)
	} else if stagingChanged {
		log.Printf("applied queued Inbox staging for collection %q", library.ID)
	}
	coll.Storage.FlushDirCache()
	inventory, err := collection.InventoryLibrary(ctx, coll, coll.InboxPath)
	if err != nil {
		return fmt.Errorf("inventory: %w", err)
	}
	reconciliation, err := collection.ReconcileLibraryInventory(ctx, db, library.ID, inventory, time.Now().UTC(), currentInventoryProcessingVersions())
	if err != nil {
		return fmt.Errorf("reconcile inventory: %w", err)
	}
	log.Printf("library scan %q generation=%d entries=%d changes=%d", library.ID, reconciliation.Generation, len(inventory), len(reconciliation.Changes))
	if err := collections.RefreshCollectionWithInventory(ctx, library.ID, inventory); err != nil {
		return fmt.Errorf("refresh Jellyfin library model: %w", err)
	}

	metadataPolicy := library.Media.Metadata
	subtitlePolicy := library.Media.Subtitles
	organizationPolicy := library.Media.Organize
	metadataEnabled := metadataPolicy.Enabled && (metadataPolicy.FetchNFO || metadataPolicy.FetchArtwork)
	subtitlesEnabled := subtitlePolicy.Enabled && len(subtitlePolicy.Languages) > 0
	if !metadataEnabled && !subtitlesEnabled && !organizationPolicy.Enabled {
		return nil
	}

	changedLibraryPaths := eligibleChangedVideoPaths(reconciliation.Changes, collection.InventoryScopeLibrary)
	if containsString(reconciliation.AlgorithmChanges, "metadata") || containsString(reconciliation.AlgorithmChanges, "subtitles") {
		changedLibraryPaths = appendUniquePaths(changedLibraryPaths, allVideoPaths(inventory, collection.InventoryScopeLibrary)...)
	}
	readyInboxPaths, err := readyInboxVideoPaths(ctx, coll, db, library, inventory)
	if err != nil {
		log.Printf("Inbox readiness for %q deferred: %v", library.ID, err)
	}
	pendingLibraryPaths, err := pendingMediaPaths(ctx, db, library.ID, inventory, collection.InventoryScopeLibrary)
	if err != nil {
		return fmt.Errorf("inspect pending media work: %w", err)
	}
	changedLibraryPaths = appendUniquePaths(changedLibraryPaths, pendingLibraryPaths...)

	var tmdb *provider.TMDb
	needsTMDb := organizationPolicy.Enabled || metadataEnabled && hasEligiblePaths(changedLibraryPaths, readyInboxPaths)
	if needsTMDb {
		keyEnv := strings.TrimSpace(metadataPolicy.APIKeyEnv)
		if keyEnv == "" {
			keyEnv = "RCFIN_TMDB_API_KEY"
		}
		var clientErr error
		tmdb, clientErr = provider.NewTMDb(os.Getenv(keyEnv), mediaHTTPClient(metadataPolicy.RequestTimeout))
		if clientErr != nil {
			log.Printf("metadata/organization for collection %q skipped: credential environment variable %s is empty", library.ID, keyEnv)
			tmdb = nil
		} else if metadataPolicy.RetryLimit > 0 {
			tmdb.SetRetryLimit(metadataPolicy.RetryLimit)
		}
	}

	stableFor := 10 * time.Minute
	stableConfig := library.Media.Inbox.StableFor
	if strings.TrimSpace(stableConfig) == "" {
		stableConfig = organizationPolicy.StableFor
	}
	if strings.TrimSpace(stableConfig) != "" {
		stableFor, err = time.ParseDuration(stableConfig)
		if err != nil || stableFor <= 0 {
			return fmt.Errorf("invalid Inbox stable_for %q", stableConfig)
		}
	}
	completionMarker := library.Media.Inbox.CompletionMarker
	if strings.TrimSpace(completionMarker) == "" {
		completionMarker = organizationPolicy.CompletionMarker
	}

	// Provision stable Inbox arrivals before any regular enrichment.
	readyRoots := map[string]bool{}
	for _, mediaPath := range readyInboxPaths {
		readyRoots[organization.InboxItemRoot(mediaPath, coll.Type)] = true
	}
	folderFormat := organizationPolicy.FolderFormat
	if library.Type == "shows" && organizationPolicy.SeriesFormat != "" {
		folderFormat = organizationPolicy.SeriesFormat
	}
	orgOptions := organization.Options{
		Inventory: inventory, IncludeProviderID: organizationPolicy.IncludeProviderID,
		EnableDiscFolders:          organizationPolicy.DiscFolders,
		FlattenGroupingDirectories: !organizationPolicy.PreserveGroupingDirectories,
		MinimumConfidence:          organizationPolicy.MinimumConfidence,
		FolderFormat:               folderFormat, FileFormat: organizationPolicy.FileFormat,
		SeasonFormat: organizationPolicy.SeasonFormat, EpisodeFormat: organizationPolicy.EpisodeFormat,
		Language: metadataPolicy.Language, FallbackLanguages: organizationPolicy.EpisodeFallbackLanguages,
		AllowedExtensions: organizationPolicy.AllowedExtensions, StableFor: stableFor,
		CompletionMarker: completionMarker,
	}
	orgRoots := pendingInboxOrganizationRoots(ctx, db, coll.ID, readyInboxPaths, coll.Type)
	orgOptions.ItemRoots = orgRoots
	var orgReport organization.Report
	if organizationPolicy.Enabled && !organizationPolicy.DryRun && tmdb != nil && len(orgRoots) > 0 {
		orgErr := perLibraryMediaJobs.run(ctx, library.ID, library.Media.MaxParallelJobs, func() error {
			var runErr error
			if coll.Type == collection.CollectionTypeShows {
				orgReport, runErr = organization.OrganizeInboxShows(ctx, coll, tmdb, db, orgOptions, organizationPolicy.DryRun, "")
			} else {
				orgReport, runErr = organization.OrganizeInboxMovies(ctx, coll, tmdb, db, orgOptions, organizationPolicy.DryRun, "")
			}
			return runErr
		})
		if orgErr != nil {
			log.Printf("Inbox organization for collection %q failed: %v", library.ID, orgErr)
		} else {
			encoded, _ := json.Marshal(orgReport)
			log.Printf("Inbox organization report: %s", encoded)
		}
		if organizationReportSubmitted(orgReport) {
			inventory, reconciliation, err = rescanAfterMediaMutation(ctx, collections, db, library.ID, coll)
			if err != nil {
				return fmt.Errorf("refresh inventory after Inbox organization: %w", err)
			}
			changedLibraryPaths = appendUniquePaths(changedLibraryPaths, eligibleChangedVideoPaths(reconciliation.Changes, collection.InventoryScopeLibrary)...)
			if containsString(reconciliation.AlgorithmChanges, "metadata") || containsString(reconciliation.AlgorithmChanges, "subtitles") {
				changedLibraryPaths = appendUniquePaths(changedLibraryPaths, allVideoPaths(inventory, collection.InventoryScopeLibrary)...)
			}
			readyInboxPaths = relocateReadyPaths(readyInboxPaths, orgReport)
		}
	}

	libraryPaths := append([]string(nil), changedLibraryPaths...)
	// A confirmed Inbox promotion is a path move, not new media. Carry the
	// original eligible work over to its final path without repeating detection.
	for _, pathBefore := range readyInboxPaths {
		if entryIsVideoAtPath(inventory, pathBefore) && entryScopeAtPath(inventory, pathBefore) == collection.InventoryScopeLibrary {
			libraryPaths = appendUnique(libraryPaths, pathBefore)
		}
	}
	inboxPaths := make([]string, 0)
	for _, mediaPath := range readyInboxPaths {
		if entryIsVideoAtPath(inventory, mediaPath) && entryScopeAtPath(inventory, mediaPath) == collection.InventoryScopeInbox {
			inboxPaths = appendUnique(inboxPaths, mediaPath)
		}
	}

	var metadataReport metadatarefresh.Report
	metadataReport.CollectionID = library.ID
	if metadataEnabled && tmdb != nil {
		if len(libraryPaths) > 0 {
			var stageReport metadatarefresh.Report
			stageReport, err = runMetadataForPaths(ctx, collections, db, coll, library, inventory, libraryPaths, false, tmdb)
			metadataReport.Items = append(metadataReport.Items, stageReport.Items...)
			if err != nil {
				log.Printf("metadata refresh for collection %q failed: %v", library.ID, err)
			}
		}
		if len(inboxPaths) > 0 {
			var stageReport metadatarefresh.Report
			stageReport, err = runMetadataForPaths(ctx, collections, db, coll, library, inventory, inboxPaths, true, tmdb)
			metadataReport.Items = append(metadataReport.Items, stageReport.Items...)
			if err != nil {
				log.Printf("Inbox metadata refresh for collection %q failed: %v", library.ID, err)
			}
		}
		if len(metadataReport.Items) > 0 {
			mergeMetadataIDs(inventory, metadataReport, coll.Type)
			encoded, _ := json.Marshal(metadataReport)
			log.Printf("metadata refresh report: %s", encoded)
			if metadataReportMayHaveChangedStorage(metadataReport) {
				if err := collections.RefreshCollectionWithInventory(ctx, library.ID, inventory); err != nil {
					log.Printf("collection refresh after metadata for %q failed: %v", library.ID, err)
				}
			}
		}
	}

	if subtitlesEnabled && hasEligiblePaths(libraryPaths, inboxPaths) {
		keyEnv := strings.TrimSpace(subtitlePolicy.APIKeyEnv)
		if keyEnv == "" {
			keyEnv = "RCFIN_OPENSUBTITLES_API_KEY"
		}
		usernameEnv := strings.TrimSpace(subtitlePolicy.UsernameEnv)
		if usernameEnv == "" {
			usernameEnv = "RCFIN_OPENSUBTITLES_USERNAME"
		}
		passwordEnv := strings.TrimSpace(subtitlePolicy.PasswordEnv)
		if passwordEnv == "" {
			passwordEnv = "RCFIN_OPENSUBTITLES_PASSWORD"
		}
		client, clientErr := subtitles.NewOpenSubtitles(os.Getenv(keyEnv), os.Getenv(usernameEnv), os.Getenv(passwordEnv), mediaHTTPClient(subtitlePolicy.RequestTimeout))
		if clientErr != nil {
			log.Printf("subtitle refresh for collection %q skipped: credential environment variable %s is empty", library.ID, keyEnv)
		} else {
			if subtitlePolicy.RetryLimit > 0 {
				client.SetRetryLimit(subtitlePolicy.RetryLimit)
			}
			if subtitlePolicy.MaxDownloadBytes > 0 {
				client.SetMaxDownloadBytes(subtitlePolicy.MaxDownloadBytes)
			}
			if len(libraryPaths) > 0 {
				if report, runErr := runSubtitlesForPaths(ctx, collections, db, coll, library, inventory, libraryPaths, false, client); runErr != nil {
					log.Printf("subtitle refresh for collection %q failed: %v", library.ID, runErr)
				} else {
					encoded, _ := json.Marshal(report)
					log.Printf("subtitle refresh report: %s", encoded)
				}
			}
			if len(inboxPaths) > 0 {
				if report, runErr := runSubtitlesForPaths(ctx, collections, db, coll, library, inventory, inboxPaths, true, client); runErr != nil {
					log.Printf("Inbox subtitle refresh for collection %q failed: %v", library.ID, runErr)
				} else {
					encoded, _ := json.Marshal(report)
					log.Printf("Inbox subtitle report: %s", encoded)
				}
			}
		}
	}
	return nil
}

func runMetadataForPaths(ctx context.Context, collections *collection.CollectionRepo, db database.Repository, coll *collection.Collection, library collectionConfig, inventory []collection.InventoryEntry, paths []string, inboxOnly bool, client *provider.TMDb) (metadatarefresh.Report, error) {
	policy := library.Media.Metadata
	var report metadatarefresh.Report
	err := perLibraryMediaJobs.run(ctx, library.ID, library.Media.MaxParallelJobs, func() error {
		var runErr error
		report, runErr = metadatarefresh.Run(ctx, coll, client, metadatarefresh.Options{
			Write: true, InboxOnly: inboxOnly, Inventory: inventory, ItemPaths: paths,
			Journal: db, FetchNFO: policy.FetchNFO, FetchArtwork: policy.FetchArtwork,
			OverwriteExisting: policy.OverwriteExisting, OverwriteArtwork: policy.OverwriteArtwork,
			Language: policy.Language, FallbackLanguages: policy.FallbackLanguages,
			ArtworkMaxBytes: policy.ArtworkMaxBytes, ArtworkMaxWidth: policy.ArtworkMaxWidth,
			ArtworkMaxHeight: policy.ArtworkMaxHeight, ArtworkFormats: policy.ArtworkFormats,
			ArtworkPosterSize: policy.ArtworkPosterSize, ArtworkBackdropSize: policy.ArtworkBackdropSize,
			MinimumConfidence: policy.MinimumConfidence,
			AlgorithmVersion:  metadatarefresh.AlgorithmVersion,
		})
		return runErr
	})
	if err == nil && metadataReportMayHaveChangedStorage(report) {
		if refreshErr := collections.RefreshCollectionWithInventory(ctx, library.ID, inventory); refreshErr != nil {
			log.Printf("collection refresh after metadata for %q failed: %v", library.ID, refreshErr)
		}
	}
	return report, err
}

func runSubtitlesForPaths(ctx context.Context, collections *collection.CollectionRepo, db database.Repository, coll *collection.Collection, library collectionConfig, inventory []collection.InventoryEntry, paths []string, inboxOnly bool, client *subtitles.OpenSubtitles) (subtitles.SubtitleReport, error) {
	policy := library.Media.Subtitles
	var report subtitles.SubtitleReport
	err := perLibraryMediaJobs.run(ctx, library.ID, library.Media.MaxParallelJobs, func() error {
		var runErr error
		report, runErr = subtitles.Refresh(ctx, coll, client, subtitles.RefreshOptions{
			InboxOnly: inboxOnly, Inventory: inventory, ItemPaths: paths, Journal: db,
			Languages: policy.Languages, Formats: policy.Formats,
			HearingImpaired: policy.HearingImpaired, Forced: policy.Forced,
			OverwriteExisting: policy.OverwriteExisting, MinimumConfidence: policy.MinimumConfidence, Write: true,
			AlgorithmVersion: subtitles.AlgorithmVersion,
		})
		return runErr
	})
	if err == nil && subtitleReportMayHaveChangedStorage(report) {
		if refreshErr := collections.RefreshCollectionWithInventory(ctx, library.ID, inventory); refreshErr != nil {
			log.Printf("collection refresh after subtitles for %q failed: %v", library.ID, refreshErr)
		}
	}
	return report, err
}

func eligibleChangedVideoPaths(changes []collection.InventoryChange, scope string) []string {
	var paths []string
	for _, change := range changes {
		if change.Kind != collection.InventoryNew && change.Kind != collection.InventoryChanged {
			continue
		}
		if change.Entry.Role == collection.InventoryVideo && !change.Entry.IsDir && change.Entry.Scope == scope {
			paths = appendUnique(paths, change.Path)
		}
	}
	return paths
}

func allVideoPaths(inventory []collection.InventoryEntry, scope string) []string {
	var paths []string
	for _, entry := range inventory {
		if entry.Scope == scope && entry.Role == collection.InventoryVideo && !entry.IsDir {
			paths = appendUnique(paths, entry.Path)
		}
	}
	return paths
}

func pendingMediaPaths(ctx context.Context, db database.Repository, libraryID string, inventory []collection.InventoryEntry, scope string) ([]string, error) {
	operations, err := database.ListAllMediaOperations(ctx, db, libraryID)
	if err != nil {
		return nil, err
	}
	byPath := map[string]collection.InventoryEntry{}
	for _, entry := range inventory {
		if entry.Role == collection.InventoryVideo && !entry.IsDir && entry.Scope == scope {
			byPath[entry.Path] = entry
		}
	}
	now := time.Now().UTC()
	var paths []string
	for _, operation := range operations {
		if operation.Action != "metadata_refresh" && !strings.HasPrefix(operation.Action, "subtitle_refresh:") {
			continue
		}
		due := operation.Status == "planned" || (operation.Status == "failed_retryable" || operation.Status == "retryable") && !operation.NextAttempt.After(now) || operation.Status == "running" && !operation.LeaseUntil.After(now)
		if !due {
			continue
		}
		if _, exists := byPath[operation.ItemKey]; exists {
			paths = appendUnique(paths, operation.ItemKey)
		}
	}
	return paths, nil
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func appendUniquePaths(paths []string, candidates ...string) []string {
	for _, candidate := range candidates {
		paths = appendUnique(paths, candidate)
	}
	return paths
}

func readyInboxVideoPaths(ctx context.Context, coll *collection.Collection, db database.Repository, library collectionConfig, inventory []collection.InventoryEntry) ([]string, error) {
	policy := library.Media.Organize
	stableFor := 10 * time.Minute
	stableConfig := library.Media.Inbox.StableFor
	if strings.TrimSpace(stableConfig) == "" {
		stableConfig = policy.StableFor
	}
	if strings.TrimSpace(stableConfig) != "" {
		parsed, err := time.ParseDuration(stableConfig)
		if err != nil || parsed <= 0 {
			return nil, fmt.Errorf("invalid Inbox stable_for %q", stableConfig)
		}
		stableFor = parsed
	}
	marker := library.Media.Inbox.CompletionMarker
	if strings.TrimSpace(marker) == "" {
		marker = policy.CompletionMarker
	}
	readiness := organization.Options{Inventory: inventory, EnableDiscFolders: true, StableFor: stableFor, CompletionMarker: marker}
	rootReady := map[string]bool{}
	rootChecked := map[string]bool{}
	var videos []string
	for _, entry := range inventory {
		discRoot := coll.Type == collection.CollectionTypeMovies && entry.Role == collection.InventoryDisc && entry.IsDir
		if entry.Scope != collection.InventoryScopeInbox || entry.Role != collection.InventoryVideo && !discRoot {
			continue
		}
		root := organization.InboxItemRoot(entry.Path, coll.Type)
		if !rootChecked[root] {
			ready, err := organization.InboxItemReady(ctx, coll, db, root, readiness)
			if err != nil {
				return nil, err
			}
			rootReady[root], rootChecked[root] = ready, true
		}
		if entry.Role == collection.InventoryVideo && !entry.IsDir && rootReady[root] {
			videos = appendUnique(videos, entry.Path)
		}
	}
	return videos, nil
}

func pendingInboxOrganizationRoots(ctx context.Context, db database.Repository, libraryID string, readyPaths []string, kind collection.CollectionType) []string {
	var roots []string
	seen := map[string]bool{}
	now := time.Now().UTC()
	for _, mediaPath := range readyPaths {
		root := organization.InboxItemRoot(mediaPath, kind)
		if seen[root] {
			continue
		}
		seen[root] = true
		review, err := db.GetMediaOperation(ctx, libraryID, root, "organization_review")
		if err != nil {
			log.Printf("read Inbox organization status for %q/%q: %v", libraryID, root, err)
			continue
		}
		if review == nil || review.Status == "planned" || (review.Status == "failed_retryable" || review.Status == "retryable") && !review.NextAttempt.After(now) {
			roots = append(roots, root)
		}
	}
	return roots
}

func hasEligiblePaths(groups ...[]string) bool {
	for _, paths := range groups {
		if len(paths) > 0 {
			return true
		}
	}
	return false
}

func entryIsVideoAtPath(inventory []collection.InventoryEntry, target string) bool {
	for _, entry := range inventory {
		if entry.Path == target && entry.Role == collection.InventoryVideo && !entry.IsDir {
			return true
		}
	}
	return false
}

func entryScopeAtPath(inventory []collection.InventoryEntry, target string) string {
	for _, entry := range inventory {
		if entry.Path == target {
			return entry.Scope
		}
	}
	return ""
}

func appendUnique(paths []string, candidate string) []string {
	for _, existing := range paths {
		if existing == candidate {
			return paths
		}
	}
	return append(paths, candidate)
}

func relocateReadyPaths(paths []string, report organization.Report) []string {
	result := make([]string, 0, len(paths))
	for _, mediaPath := range paths {
		mapped := mediaPath
		for _, item := range report.Items {
			if item.Destination == "" || item.State != "submitted" && item.State != "unchanged" {
				continue
			}
			if mediaPath == item.Source || strings.HasPrefix(mediaPath, item.Source+"/") {
				mapped = path.Join(item.Destination, strings.TrimPrefix(mediaPath, item.Source))
				break
			}
		}
		result = appendUnique(result, mapped)
	}
	return result
}

func mergeMetadataIDs(inventory []collection.InventoryEntry, report metadatarefresh.Report, kind collection.CollectionType) {
	for _, item := range report.Items {
		if item.ProviderID == "" || item.Source == "" {
			continue
		}
		for i := range inventory {
			if inventory[i].Role != collection.InventoryVideo {
				continue
			}
			belongs := path.Dir(inventory[i].Path) == path.Dir(item.Source)
			if kind == collection.CollectionTypeShows {
				belongs = collection.InventoryShowRoot(inventory[i].Path) == collection.InventoryShowRoot(item.Source)
			}
			if belongs {
				if inventory[i].ProviderIDs == nil {
					inventory[i].ProviderIDs = map[string]string{}
				}
				inventory[i].ProviderIDs["tmdb"] = item.ProviderID
			}
		}
	}
}

func rescanAfterMediaMutation(ctx context.Context, collections *collection.CollectionRepo, db database.Repository, libraryID string, coll *collection.Collection) ([]collection.InventoryEntry, collection.InventoryReconciliation, error) {
	coll.Storage.FlushDirCache()
	inventory, err := collection.InventoryLibrary(ctx, coll, coll.InboxPath)
	if err != nil {
		return nil, collection.InventoryReconciliation{}, err
	}
	reconciliation, err := collection.ReconcileLibraryInventory(ctx, db, libraryID, inventory, time.Now().UTC(), currentInventoryProcessingVersions())
	if err != nil {
		return nil, collection.InventoryReconciliation{}, err
	}
	if err := collections.RefreshCollectionWithInventory(ctx, libraryID, inventory); err != nil {
		return nil, collection.InventoryReconciliation{}, err
	}
	return inventory, reconciliation, nil
}
