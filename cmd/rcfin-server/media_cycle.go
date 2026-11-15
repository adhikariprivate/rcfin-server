package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/adhikariprivate/rcfin-server/internal/collection"
	"github.com/adhikariprivate/rcfin-server/internal/database"
	"github.com/adhikariprivate/rcfin-server/internal/database/model"
	"github.com/adhikariprivate/rcfin-server/internal/metadata/provider"
	metadatarefresh "github.com/adhikariprivate/rcfin-server/internal/metadata/refresh"
	"github.com/adhikariprivate/rcfin-server/internal/organization"
	"github.com/adhikariprivate/rcfin-server/internal/rclonestore"
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

func stageConfigurationSignature(configuration any) string {
	encoded, err := json.Marshal(configuration)
	if err != nil {
		return ""
	}
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:])
}

func inventoryVersionsForLibrary(library collectionConfig, storage ...*rclonestore.Library) collection.InventoryProcessingVersions {
	versions := currentInventoryProcessingVersions()
	versions.ConfigurationSignatures = map[string]string{
		"metadata":  stageConfigurationSignature(library.Media.Metadata),
		"subtitles": stageConfigurationSignature(library.Media.Subtitles),
	}
	if len(storage) > 0 && storage[0] != nil {
		versions.ListingMode = storage[0].InventoryListingMode()
	}
	if value := strings.TrimSpace(library.Media.MissingGrace); value != "" {
		if parsed, err := time.ParseDuration(value); err == nil && parsed > 0 {
			versions.MissingGrace = parsed
		}
	}
	return versions
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
			if err := runLibraryAutomationCycle(ctx, collections, db, library, interval); err != nil && ctx.Err() == nil {
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

func runLibraryAutomationCycle(ctx context.Context, collections *collection.CollectionRepo, db database.Repository, library collectionConfig, scanInterval time.Duration) error {
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
	processingVersions := inventoryVersionsForLibrary(library, coll.Storage)
	reconciliation, err := collection.ReconcileLibraryInventory(ctx, db, library.ID, inventory, time.Now().UTC(), processingVersions)
	if err != nil {
		return fmt.Errorf("reconcile inventory: %w", err)
	}
	if err := relinkMovedMediaOperations(ctx, db, library.ID, coll.Type, reconciliation.Changes, inventory); err != nil {
		return fmt.Errorf("relink moved media operations: %w", err)
	}
	log.Printf("library scan %q generation=%d entries=%d changes=%d listing=%s backend_ids=%d", library.ID, reconciliation.Generation, len(inventory), len(reconciliation.Changes), reconciliation.ListingMode, reconciliation.BackendIDsObserved)
	if err := collections.RefreshCollectionWithInventory(ctx, library.ID, inventory); err != nil {
		return fmt.Errorf("refresh Jellyfin library model: %w", err)
	}

	metadataPolicy := library.Media.Metadata
	subtitlePolicy := library.Media.Subtitles
	organizationPolicy := library.Media.Organize
	metadataEnabled := metadataPolicy.Enabled && (metadataPolicy.FetchNFO || metadataPolicy.FetchArtwork)
	subtitlesEnabled := subtitlePolicy.Enabled && len(subtitlePolicy.Languages) > 0
	if !metadataEnabled && !subtitlesEnabled && !organizationPolicy.Enabled {
		return updateLibraryStageState(ctx, db, library.ID, processingVersions, "metadata", "subtitles")
	}

	changedVideoPaths := eligibleChangedVideoPaths(reconciliation.Changes, collection.InventoryScopeLibrary)
	changedMetadataLibraryPaths := appendUniquePaths(append([]string(nil), changedVideoPaths...), affectedMediaPathsBySidecarChanges(reconciliation.Changes, inventory, collection.InventoryScopeLibrary, "metadata", coll.Type)...)
	changedSubtitleLibraryPaths := appendUniquePaths(append([]string(nil), changedVideoPaths...), affectedMediaPathsBySidecarChanges(reconciliation.Changes, inventory, collection.InventoryScopeLibrary, "subtitles", coll.Type)...)
	metadataAlgorithmChanged := containsString(reconciliation.AlgorithmChanges, "metadata")
	subtitleAlgorithmChanged := containsString(reconciliation.AlgorithmChanges, "subtitles")
	metadataConfigurationChanged := containsString(reconciliation.ConfigurationChanges, "metadata")
	subtitleConfigurationChanged := containsString(reconciliation.ConfigurationChanges, "subtitles")
	readyInboxPaths, err := readyInboxVideoPaths(ctx, coll, db, library, inventory)
	if err != nil {
		log.Printf("Inbox readiness for %q deferred: %v", library.ID, err)
	}
	pendingMetadataPaths, err := pendingMediaPaths(ctx, db, library.ID, inventory, collection.InventoryScopeLibrary, "metadata")
	if err != nil {
		return fmt.Errorf("inspect pending metadata work: %w", err)
	}
	pendingSubtitlePaths, err := pendingMediaPaths(ctx, db, library.ID, inventory, collection.InventoryScopeLibrary, "subtitles")
	if err != nil {
		return fmt.Errorf("inspect pending subtitle work: %w", err)
	}
	metadataCandidates := stageWorkPaths(changedMetadataLibraryPaths, pendingMetadataPaths, inventory, collection.InventoryScopeLibrary, metadataAlgorithmChanged || metadataConfigurationChanged)
	metadataForced := metadataAlgorithmChanged || metadataConfigurationChanged
	var tmdb *provider.TMDb
	needsTMDb := organizationPolicy.Enabled || metadataEnabled && hasEligiblePaths(metadataCandidates, readyInboxPaths)
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
			inventory, reconciliation, err = rescanAfterMediaMutation(ctx, collections, db, library, coll)
			if err != nil {
				return fmt.Errorf("refresh inventory after Inbox organization: %w", err)
			}
			changedMetadataLibraryPaths = appendUniquePaths(changedMetadataLibraryPaths, eligibleChangedVideoPaths(reconciliation.Changes, collection.InventoryScopeLibrary)...)
			changedMetadataLibraryPaths = appendUniquePaths(changedMetadataLibraryPaths, affectedMediaPathsBySidecarChanges(reconciliation.Changes, inventory, collection.InventoryScopeLibrary, "metadata", coll.Type)...)
			changedSubtitleLibraryPaths = appendUniquePaths(changedSubtitleLibraryPaths, eligibleChangedVideoPaths(reconciliation.Changes, collection.InventoryScopeLibrary)...)
			changedSubtitleLibraryPaths = appendUniquePaths(changedSubtitleLibraryPaths, affectedMediaPathsBySidecarChanges(reconciliation.Changes, inventory, collection.InventoryScopeLibrary, "subtitles", coll.Type)...)
			metadataAlgorithmChanged = metadataAlgorithmChanged || containsString(reconciliation.AlgorithmChanges, "metadata")
			subtitleAlgorithmChanged = subtitleAlgorithmChanged || containsString(reconciliation.AlgorithmChanges, "subtitles")
			metadataConfigurationChanged = metadataConfigurationChanged || containsString(reconciliation.ConfigurationChanges, "metadata")
			subtitleConfigurationChanged = subtitleConfigurationChanged || containsString(reconciliation.ConfigurationChanges, "subtitles")
			readyInboxPaths = relocateReadyPaths(readyInboxPaths, orgReport)
		}
	}

	metadataLibraryPaths := stageWorkPaths(changedMetadataLibraryPaths, pendingMetadataPaths, inventory, collection.InventoryScopeLibrary, metadataAlgorithmChanged || metadataConfigurationChanged)
	subtitleLibraryPaths := stageWorkPaths(changedSubtitleLibraryPaths, pendingSubtitlePaths, inventory, collection.InventoryScopeLibrary, subtitleAlgorithmChanged || subtitleConfigurationChanged)
	// A confirmed Inbox promotion is a path move, not new media. Carry the
	// original eligible work over to its final path without repeating detection.
	for _, pathBefore := range readyInboxPaths {
		if entryIsVideoAtPath(inventory, pathBefore) && entryScopeAtPath(inventory, pathBefore) == collection.InventoryScopeLibrary {
			metadataLibraryPaths = appendUnique(metadataLibraryPaths, pathBefore)
			subtitleLibraryPaths = appendUnique(subtitleLibraryPaths, pathBefore)
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
	refreshCollectionAfterSidecars := false
	metadataStageSucceeded := true
	if metadataEnabled && tmdb != nil {
		if len(metadataLibraryPaths) > 0 {
			var stageReport metadatarefresh.Report
			stageReport, err = runMetadataForPaths(ctx, db, coll, library, inventory, metadataLibraryPaths, false, tmdb, metadataForced)
			metadataReport.Items = append(metadataReport.Items, stageReport.Items...)
			if err != nil {
				log.Printf("metadata refresh for collection %q failed: %v", library.ID, err)
				metadataStageSucceeded = false
			} else {
				metadataStageSucceeded = true
			}
		}
		if len(inboxPaths) > 0 {
			var stageReport metadatarefresh.Report
			stageReport, err = runMetadataForPaths(ctx, db, coll, library, inventory, inboxPaths, true, tmdb, metadataForced)
			metadataReport.Items = append(metadataReport.Items, stageReport.Items...)
			if err != nil {
				log.Printf("Inbox metadata refresh for collection %q failed: %v", library.ID, err)
				metadataStageSucceeded = false
			}
		}
		if len(metadataReport.Items) > 0 {
			mergeMetadataIDs(inventory, metadataReport, coll.Type)
			for _, item := range metadataReport.Items {
				for _, file := range item.Files {
					if file.State == "written" {
						inventory = mergeWrittenSidecarInventory(coll, inventory, item.Source, file.Destination)
					}
				}
			}
			refreshCollectionAfterSidecars = refreshCollectionAfterSidecars || metadataReportMayHaveChangedStorage(metadataReport)
			encoded, _ := json.Marshal(metadataReport)
			log.Printf("metadata refresh report: %s", encoded)
		}
	} else if metadataEnabled && hasEligiblePaths(metadataLibraryPaths, inboxPaths) {
		retryAt := time.Now().Add(scanInterval)
		if err := queueDeferredMetadataOperations(ctx, db, library.ID, inventory, coll.Type, appendUniquePaths(append([]string(nil), metadataLibraryPaths...), inboxPaths...), retryAt, "required metadata provider credentials are unavailable"); err != nil {
			return fmt.Errorf("journal deferred metadata work: %w", err)
		}
		metadataStageSucceeded = true
	} else if metadataEnabled {
		metadataStageSucceeded = true
	}

	subtitleStageSucceeded := !subtitlesEnabled
	if subtitlesEnabled && hasEligiblePaths(subtitleLibraryPaths, inboxPaths) {
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
			reason := fmt.Sprintf("required subtitle provider credentials are unavailable (environment variables: %s, %s, %s)", keyEnv, usernameEnv, passwordEnv)
			log.Printf("subtitle refresh for collection %q skipped: %s", library.ID, reason)
			if err := queueDeferredSubtitleOperations(ctx, db, library.ID, appendUniquePaths(append([]string(nil), subtitleLibraryPaths...), inboxPaths...), subtitlePolicy.Languages, time.Now().Add(scanInterval), reason); err != nil {
				return fmt.Errorf("journal deferred subtitle work: %w", err)
			}
			subtitleStageSucceeded = true
		} else {
			if subtitlePolicy.RetryLimit > 0 {
				client.SetRetryLimit(subtitlePolicy.RetryLimit)
			}
			if subtitlePolicy.MaxDownloadBytes > 0 {
				client.SetMaxDownloadBytes(subtitlePolicy.MaxDownloadBytes)
			}
			if len(subtitleLibraryPaths) > 0 {
				report, runErr := runSubtitlesForPaths(ctx, db, coll, library, inventory, subtitleLibraryPaths, false, client)
				inventory = mergeWrittenSubtitleInventory(coll, inventory, report)
				refreshCollectionAfterSidecars = refreshCollectionAfterSidecars || subtitleReportMayHaveChangedStorage(report)
				if runErr != nil {
					log.Printf("subtitle refresh for collection %q failed: %v", library.ID, runErr)
					subtitleStageSucceeded = false
				} else {
					encoded, _ := json.Marshal(report)
					log.Printf("subtitle refresh report: %s", encoded)
				}
			}
			if len(inboxPaths) > 0 {
				report, runErr := runSubtitlesForPaths(ctx, db, coll, library, inventory, inboxPaths, true, client)
				inventory = mergeWrittenSubtitleInventory(coll, inventory, report)
				refreshCollectionAfterSidecars = refreshCollectionAfterSidecars || subtitleReportMayHaveChangedStorage(report)
				if runErr != nil {
					log.Printf("Inbox subtitle refresh for collection %q failed: %v", library.ID, runErr)
					subtitleStageSucceeded = false
				} else {
					encoded, _ := json.Marshal(report)
					log.Printf("Inbox subtitle report: %s", encoded)
				}
			}
		}
	} else if subtitlesEnabled {
		subtitleStageSucceeded = true
	}
	if refreshCollectionAfterSidecars {
		if err := collections.RefreshCollectionWithInventory(ctx, library.ID, inventory); err != nil {
			log.Printf("collection refresh after sidecar writes for %q failed: %v", library.ID, err)
		}
	}
	if metadataStageSucceeded {
		if err := updateLibraryStageState(ctx, db, library.ID, processingVersions, "metadata"); err != nil {
			return fmt.Errorf("acknowledge metadata stage state: %w", err)
		}
	}
	if subtitleStageSucceeded {
		if err := updateLibraryStageState(ctx, db, library.ID, processingVersions, "subtitles"); err != nil {
			return fmt.Errorf("acknowledge subtitle stage state: %w", err)
		}
	}
	return nil
}

func runMetadataForPaths(ctx context.Context, db database.Repository, coll *collection.Collection, library collectionConfig, inventory []collection.InventoryEntry, paths []string, inboxOnly bool, client *provider.TMDb, force bool) (metadatarefresh.Report, error) {
	policy := library.Media.Metadata
	var report metadatarefresh.Report
	err := perLibraryMediaJobs.run(ctx, library.ID, library.Media.MaxParallelJobs, func() error {
		var runErr error
		report, runErr = metadatarefresh.Run(ctx, coll, client, metadatarefresh.Options{
			Write: true, InboxOnly: inboxOnly, Inventory: inventory, ItemPaths: paths,
			Force:   force,
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
	return report, err
}

func runSubtitlesForPaths(ctx context.Context, db database.Repository, coll *collection.Collection, library collectionConfig, inventory []collection.InventoryEntry, paths []string, inboxOnly bool, client *subtitles.OpenSubtitles) (subtitles.SubtitleReport, error) {
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

// affectedMediaPathsBySidecarChanges maps changed or removed sidecars back to
// their media items. Sidecars are separate inventory objects, but metadata and
// subtitle decisions can depend on them (for example, an edited NFO provider
// ID or a deleted local subtitle).
func affectedMediaPathsBySidecarChanges(changes []collection.InventoryChange, inventory []collection.InventoryEntry, scope, stage string, kind collection.CollectionType) []string {
	var paths []string
	for _, change := range changes {
		if change.Kind != collection.InventoryNew && change.Kind != collection.InventoryChanged && change.Kind != collection.InventoryMissing {
			continue
		}
		sidecarPath := change.Path
		role := change.Entry.Role
		sidecarScope := change.Entry.Scope
		if change.PreviousEntry != nil && (change.Kind == collection.InventoryMissing || sidecarPath == "") {
			sidecarPath = change.PreviousEntry.Path
			role = collection.InventoryRole(change.PreviousEntry.Role)
			sidecarScope = change.PreviousEntry.Scope
		}
		if sidecarScope != scope || sidecarPath == "" {
			continue
		}
		if stage == "metadata" && role != collection.InventoryNFO && role != collection.InventoryArtwork || stage == "subtitles" && role != collection.InventorySubtitle {
			continue
		}
		for _, video := range inventory {
			if video.Scope != scope || video.Role != collection.InventoryVideo || video.IsDir {
				continue
			}
			affected := false
			if stage == "metadata" {
				if kind == collection.CollectionTypeShows {
					affected = collection.InventoryShowRoot(video.Path) == collection.InventoryShowRoot(sidecarPath)
				} else {
					affected = path.Dir(video.Path) == path.Dir(sidecarPath)
				}
			} else {
				videoStem := strings.TrimSuffix(path.Base(video.Path), path.Ext(video.Path))
				sidecarStem := strings.TrimSuffix(path.Base(sidecarPath), path.Ext(sidecarPath))
				affected = path.Dir(video.Path) == path.Dir(sidecarPath) && (strings.EqualFold(videoStem, sidecarStem) || strings.HasPrefix(strings.ToLower(sidecarStem), strings.ToLower(videoStem)+"."))
			}
			if affected {
				paths = appendUnique(paths, video.Path)
			}
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

func stageWorkPaths(changed, pending []string, inventory []collection.InventoryEntry, scope string, algorithmChanged bool) []string {
	paths := appendUniquePaths(append([]string(nil), changed...), pending...)
	if algorithmChanged {
		paths = appendUniquePaths(paths, allVideoPaths(inventory, scope)...)
	}
	return paths
}

func mergeWrittenSidecarInventory(coll *collection.Collection, inventory []collection.InventoryEntry, source, destination string) []collection.InventoryEntry {
	if coll == nil || coll.Storage == nil || destination == "" {
		return inventory
	}
	scope := entryScopeAtPath(inventory, source)
	if scope == "" {
		return inventory
	}
	info, err := coll.Storage.Stat(destination)
	if err != nil || info.IsDir() {
		return inventory
	}
	extension := strings.ToLower(path.Ext(destination))
	role := collection.InventoryOther
	switch extension {
	case ".nfo":
		role = collection.InventoryNFO
	case ".jpg", ".jpeg", ".png", ".tbn", ".webp":
		role = collection.InventoryArtwork
	case ".srt", ".vtt", ".ass", ".ssa", ".sub":
		role = collection.InventorySubtitle
	}
	entry := collection.InventoryEntry{
		Path: destination, Scope: scope, Size: info.Size(), ModTime: info.ModTime().UnixNano(),
		Role: role, Extension: strings.TrimPrefix(extension, "."),
	}
	for i := range inventory {
		if inventory[i].Path == destination {
			inventory[i] = entry
			return inventory
		}
	}
	return append(inventory, entry)
}

func mergeWrittenSubtitleInventory(coll *collection.Collection, inventory []collection.InventoryEntry, report subtitles.SubtitleReport) []collection.InventoryEntry {
	for _, action := range report.Actions {
		if action.State == "written" {
			inventory = mergeWrittenSidecarInventory(coll, inventory, action.Source, action.Destination)
		}
	}
	return inventory
}

func pendingMediaPaths(ctx context.Context, db database.Repository, libraryID string, inventory []collection.InventoryEntry, scope, stage string) ([]string, error) {
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
		if stage == "metadata" && operation.Action != "metadata_refresh" || stage == "subtitles" && !strings.HasPrefix(operation.Action, "subtitle_refresh:") {
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

func queueDeferredMetadataOperations(ctx context.Context, db database.Repository, libraryID string, inventory []collection.InventoryEntry, kind collection.CollectionType, paths []string, retryAt time.Time, reason string) error {
	entries := make(map[string]collection.InventoryEntry, len(inventory))
	for _, entry := range inventory {
		if entry.Role == collection.InventoryVideo && !entry.IsDir {
			entries[entry.Path] = entry
		}
	}
	selected := append([]string(nil), paths...)
	sort.Strings(selected)
	seenGroups := map[string]bool{}
	for _, itemPath := range selected {
		_, exists := entries[itemPath]
		if !exists {
			continue
		}
		group := path.Dir(itemPath)
		if kind == collection.CollectionTypeShows {
			group = collection.InventoryShowRoot(itemPath)
		}
		if seenGroups[group] {
			continue
		}
		seenGroups[group] = true
		if err := queueDeferredMediaOperation(ctx, db, libraryID, itemPath, "metadata_refresh", retryAt, reason); err != nil {
			return err
		}
	}
	return nil
}

func queueDeferredSubtitleOperations(ctx context.Context, db database.Repository, libraryID string, paths, languages []string, retryAt time.Time, reason string) error {
	selectedLanguages := make(map[string]bool, len(languages))
	for _, language := range languages {
		language = strings.ToLower(strings.TrimSpace(language))
		if language != "" {
			selectedLanguages[language] = true
		}
	}
	for _, itemPath := range paths {
		for language := range selectedLanguages {
			if err := queueDeferredMediaOperation(ctx, db, libraryID, itemPath, "subtitle_refresh:"+language, retryAt, reason); err != nil {
				return err
			}
		}
	}
	return nil
}

func queueDeferredMediaOperation(ctx context.Context, db database.Repository, libraryID, itemPath, action string, retryAt time.Time, reason string) error {
	existing, err := db.GetMediaOperation(ctx, libraryID, itemPath, action)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if retryAt.IsZero() || retryAt.Before(now) {
		retryAt = now.Add(30 * time.Minute)
	}
	if existing != nil && (existing.Status == "needs_review" || existing.Status == "conflict" || existing.Status == "running" && existing.LeaseUntil.After(now)) {
		return nil
	}
	op := model.MediaOperation{
		ID: mediaOperationID(libraryID, itemPath, action), CollectionID: libraryID,
		ItemKey: itemPath, SourcePath: itemPath, Action: action,
		Status: "failed_retryable", Evidence: `{"deferred":"provider_credentials_unavailable"}`,
		Error: reason, NextAttempt: retryAt, LeaseUntil: now, CreatedAt: now, UpdatedAt: now,
	}
	if existing != nil {
		op.ID = existing.ID
		op.CreatedAt = existing.CreatedAt
		op.Attempts = existing.Attempts
	}
	return db.UpsertMediaOperation(ctx, &op)
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

func rescanAfterMediaMutation(ctx context.Context, collections *collection.CollectionRepo, db database.Repository, library collectionConfig, coll *collection.Collection) ([]collection.InventoryEntry, collection.InventoryReconciliation, error) {
	coll.Storage.FlushDirCache()
	inventory, err := collection.InventoryLibrary(ctx, coll, coll.InboxPath)
	if err != nil {
		return nil, collection.InventoryReconciliation{}, err
	}
	reconciliation, err := collection.ReconcileLibraryInventory(ctx, db, library.ID, inventory, time.Now().UTC(), inventoryVersionsForLibrary(library, coll.Storage))
	if err != nil {
		return nil, collection.InventoryReconciliation{}, err
	}
	if err := relinkMovedMediaOperations(ctx, db, library.ID, coll.Type, reconciliation.Changes, inventory); err != nil {
		return nil, collection.InventoryReconciliation{}, err
	}
	if err := collections.RefreshCollectionWithInventory(ctx, library.ID, inventory); err != nil {
		return nil, collection.InventoryReconciliation{}, err
	}
	return inventory, reconciliation, nil
}

func relinkMovedMediaOperations(ctx context.Context, db database.Repository, libraryID string, kind collection.CollectionType, changes []collection.InventoryChange, inventory []collection.InventoryEntry) error {
	var moves []collection.InventoryChange
	for _, change := range changes {
		if change.Kind == collection.InventoryMoved {
			moves = append(moves, change)
		}
	}
	if len(moves) == 0 {
		return nil
	}
	movedPaths := make([]string, 0, len(moves))
	for _, move := range moves {
		movedPaths = append(movedPaths, move.Path)
	}
	operations, err := database.ListAllMediaOperations(ctx, db, libraryID)
	if err != nil {
		return err
	}
	// Organization root operations carry the stable Jellyfin item ID in their
	// evidence. Rebase only their completed destination path when a confirmed
	// directory move is observed, so the stable ID remains attached to the item.
	for _, operation := range operations {
		if !isStableItemIDOperation(operation.Action) || operation.Status != "complete" && operation.Status != "completed" || operation.DestinationPath == "" {
			continue
		}
		var selected *collection.InventoryChange
		for i := range moves {
			move := &moves[i]
			if !move.Entry.IsDir || operation.DestinationPath != move.OldPath && !strings.HasPrefix(operation.DestinationPath, move.OldPath+"/") {
				continue
			}
			if selected == nil || len(move.OldPath) > len(selected.OldPath) {
				selected = move
			}
		}
		if selected == nil {
			continue
		}
		newDestination := rebaseOperationPath(operation.DestinationPath, selected.OldPath, selected.Path)
		isDirectory := false
		for _, entry := range inventory {
			if entry.Path == newDestination && entry.IsDir {
				isDirectory = true
				break
			}
		}
		if !isDirectory {
			continue
		}
		operation.DestinationPath = newDestination
		operation.UpdatedAt = time.Now().UTC()
		if err := db.UpsertMediaOperation(ctx, &operation); err != nil {
			return fmt.Errorf("relink stable item ID for %q: %w", operation.ItemKey, err)
		}
	}
	for _, operation := range operations {
		if operation.Action != "metadata_refresh" && !strings.HasPrefix(operation.Action, "subtitle_refresh:") {
			continue
		}
		var selected *collection.InventoryChange
		for i := range moves {
			move := &moves[i]
			if operation.ItemKey != move.OldPath && !strings.HasPrefix(operation.ItemKey, move.OldPath+"/") {
				continue
			}
			if selected == nil || len(move.OldPath) > len(selected.OldPath) {
				selected = move
			}
		}
		if selected == nil {
			continue
		}
		newKey := selected.Path + strings.TrimPrefix(operation.ItemKey, selected.OldPath)
		entry, found := inventoryVideoAtPath(inventory, newKey)
		if !found {
			continue
		}
		if existing, lookupErr := db.GetMediaOperation(ctx, libraryID, newKey, operation.Action); lookupErr != nil {
			return lookupErr
		} else if existing != nil {
			// A destination journal row is already authoritative. Keep the old
			// record for audit rather than colliding or overwriting its decision.
			continue
		}
		oldRoot, newRoot := path.Dir(selected.OldPath), path.Dir(selected.Path)
		if kind == collection.CollectionTypeShows {
			oldRoot, newRoot = collection.InventoryShowRoot(selected.OldPath), collection.InventoryShowRoot(selected.Path)
		}
		updated := operation
		oldItemKey := operation.ItemKey
		oldID := operation.ID
		updated.ItemKey = newKey
		updated.SourcePath = selected.Path + strings.TrimPrefix(operation.SourcePath, selected.OldPath)
		updated.DestinationPath = rebaseOperationPath(operation.DestinationPath, oldRoot, newRoot)
		updated.ID = mediaOperationID(libraryID, newKey, operation.Action)
		updated.UpdatedAt = time.Now().UTC()
		if operation.Action == "metadata_refresh" {
			if err := metadatarefresh.RelinkOperationEvidence(&updated, entry, oldRoot, newRoot); err != nil {
				return fmt.Errorf("relink metadata evidence for %q: %w", oldItemKey, err)
			}
		} else if err := subtitles.RelinkOperationEvidence(&updated, entry, oldRoot, newRoot); err != nil {
			return fmt.Errorf("relink subtitle evidence for %q: %w", oldItemKey, err)
		}
		relinked, err := db.RelinkMediaOperation(ctx, libraryID, oldID, oldItemKey, &updated)
		if err != nil {
			return fmt.Errorf("relink journal operation %q: %w", oldItemKey, err)
		}
		if !relinked {
			// A live worker or concurrent scanner still owns this path. Leave the
			// durable move pending so a later cycle can retry after its lease ends.
			return nil
		}
	}
	return db.MarkLibraryInventoryMovesApplied(ctx, libraryID, movedPaths)
}

func isStableItemIDOperation(action string) bool {
	return action == "stage_existing" || action == "move_directory" || action == "move_directory:root"
}

func inventoryVideoAtPath(inventory []collection.InventoryEntry, target string) (collection.InventoryEntry, bool) {
	for _, entry := range inventory {
		if entry.Path == target && entry.Role == collection.InventoryVideo && !entry.IsDir {
			return entry, true
		}
	}
	return collection.InventoryEntry{}, false
}

func mediaOperationID(collectionID, itemKey, action string) string {
	hash := sha256.Sum256([]byte(collectionID + "\x00" + itemKey + "\x00" + action))
	return hex.EncodeToString(hash[:])
}

func rebaseOperationPath(value, oldRoot, newRoot string) string {
	if value == oldRoot {
		return newRoot
	}
	if strings.HasPrefix(value, oldRoot+"/") {
		return newRoot + strings.TrimPrefix(value, oldRoot)
	}
	return value
}
