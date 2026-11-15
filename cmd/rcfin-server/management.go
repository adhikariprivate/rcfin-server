package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"github.com/spf13/pflag"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"

	"github.com/adhikariprivate/rcfin-server/internal/collection"
	"github.com/adhikariprivate/rcfin-server/internal/database"
	"github.com/adhikariprivate/rcfin-server/internal/database/model"
	"github.com/adhikariprivate/rcfin-server/internal/idhash"
	"github.com/adhikariprivate/rcfin-server/internal/metadata/provider"
	metadatarefresh "github.com/adhikariprivate/rcfin-server/internal/metadata/refresh"
	"github.com/adhikariprivate/rcfin-server/internal/organization"
	"github.com/adhikariprivate/rcfin-server/internal/rclonestore"
	"github.com/adhikariprivate/rcfin-server/internal/subtitles"
)

func managementCommand(args []string) (commandArgs []string, configPath string, ok bool) {
	configPath = "rcfin.yaml"
	remaining := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "--config" {
			if i+1 >= len(args) {
				return nil, configPath, false
			}
			configPath = args[i+1]
			i++
			continue
		}
		if strings.HasPrefix(args[i], "--config=") {
			configPath = strings.TrimPrefix(args[i], "--config=")
			continue
		}
		remaining = append(remaining, args[i])
	}
	if len(remaining) == 0 || (remaining[0] != "user" && remaining[0] != "library" && remaining[0] != "media") {
		return nil, configPath, false
	}
	return remaining, configPath, true
}

func runManagementCommand(configPath string, args []string) error {
	config, err := readConfig(configPath)
	if err != nil {
		return fmt.Errorf("read config %q: %w", configPath, err)
	}
	if err := validateCollections(config.Collections); err != nil {
		return fmt.Errorf("invalid collections: %w", err)
	}
	if args[0] == "media" && len(args) > 1 && args[1] == "inventory" {
		return runMediaInventoryCommand(context.Background(), config, args[2:])
	}
	if args[0] == "media" && len(args) > 1 && args[1] == "plan" {
		return runMediaPlanCommand(context.Background(), config, args[2:])
	}
	repo, err := openConfiguredDatabase(config)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer repo.Close()

	ctx := context.Background()
	switch args[0] {
	case "library":
		if len(args) == 2 && args[1] == "list" {
			for _, library := range config.Collections {
				fmt.Printf("%-20s %-8s %s\n", library.ID, library.Type, library.Name)
			}
			return nil
		}
		if len(args) >= 2 && args[1] == "scan" {
			return runLibraryScanCommand(ctx, repo, config, args[2:])
		}
		if len(args) >= 2 && args[1] == "status" {
			return runLibraryStatusCommand(ctx, repo, config.Collections, args[2:])
		}
		if len(args) >= 2 && args[1] == "refresh" {
			return runLibraryRefreshCommand(ctx, repo, config, args[2:])
		}
		if len(args) >= 2 && args[1] == "inbox" {
			return runLibraryInboxCommand(ctx, repo, config, args[2:])
		}
		if len(args) >= 2 && args[1] == "organize" {
			return runLibraryOrganizeCommand(ctx, repo, config, args[2:])
		}
		if len(args) >= 2 && args[1] == "retry" {
			return runMediaCommand(ctx, repo, config.Collections, append([]string{"retry"}, args[2:]...))
		}
		return errors.New("usage: rcfin library <list|scan LIBRARY_ID [--full]|status LIBRARY_ID|refresh metadata|subtitles LIBRARY_ID|retry LIBRARY_ID ITEM_KEY ACTION|inbox stage LIBRARY_ID ITEM_PATH|organize plan|apply LIBRARY_ID>")
	case "user":
		return runUserCommand(ctx, repo, config.Collections, args[1:])
	case "media":
		return runMediaCommand(ctx, repo, config.Collections, args[1:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runLibraryOrganizeCommand(ctx context.Context, repo database.Repository, config configFile, args []string) error {
	if len(args) < 2 || (args[0] != "plan" && args[0] != "apply") {
		return errors.New("usage: rcfin library organize <plan|apply> LIBRARY_ID [--item PATH] [--existing] [--yes]")
	}
	action, libraryID := args[0], args[1]
	flags := pflag.NewFlagSet("library organize", pflag.ContinueOnError)
	flags.SetOutput(io.Discard)
	itemPath := flags.String("item", "", "Scope organization to one relative item path")
	existing := flags.Bool("existing", false, "Normalize existing library paths instead of Inbox items")
	yes := flags.Bool("yes", false, "Confirm the configured organization operation")
	if err := flags.Parse(args[2:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected arguments after library organize flags")
	}
	if action == "apply" && !*yes {
		return errors.New("library organize apply requires --yes after reviewing the plan")
	}
	var selected *collectionConfig
	for i := range config.Collections {
		if config.Collections[i].ID == libraryID {
			selected = &config.Collections[i]
			break
		}
	}
	if selected == nil {
		return fmt.Errorf("unknown library ID %q", libraryID)
	}
	policy := selected.Media.Organize
	if action == "apply" {
		if *existing {
			if !selected.Media.NormalizeExisting.Enabled {
				return errors.New("enable media.normalize_existing.enabled before applying existing-path normalization")
			}
			if selected.Media.NormalizeExisting.DryRun {
				return errors.New("set media.normalize_existing.dry_run: false before applying existing-path normalization")
			}
		} else {
			if !policy.Enabled {
				return errors.New("enable media.organize.enabled before applying Inbox organization")
			}
			if policy.DryRun {
				return errors.New("set media.organize.dry_run: false before applying Inbox organization")
			}
		}
	}
	if err := rclonestore.Configure(config.Rclone.Config, config.Rclone.CacheDir); err != nil {
		return fmt.Errorf("configure Rclone: %w", err)
	}
	storage, err := rclonestore.New(ctx, selected.Remote, selected.VFS)
	if err != nil {
		return fmt.Errorf("open library %q: %w", libraryID, err)
	}
	defer storage.Close()
	inbox := selected.Media.InboxPath
	if inbox == "" {
		inbox = ".Inbox"
	}
	coll := &collection.Collection{ID: selected.ID, Name: selected.Name, Type: collection.CollectionType(selected.Type), Remote: selected.Remote, Storage: storage, InboxPath: inbox}
	storage.FlushDirCache()
	inventory, err := collection.InventoryLibrary(ctx, coll, inbox)
	if err != nil {
		return fmt.Errorf("inventory library: %w", err)
	}
	if *itemPath != "" {
		clean := path.Clean(*itemPath)
		if path.IsAbs(*itemPath) || clean != *itemPath || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(*itemPath, "\\") {
			return fmt.Errorf("item path must be a clean relative path within the library")
		}
	}
	metadataPolicy := selected.Media.Metadata
	keyEnv := strings.TrimSpace(metadataPolicy.APIKeyEnv)
	if keyEnv == "" {
		keyEnv = "RCFIN_TMDB_API_KEY"
	}
	client, err := provider.NewTMDb(os.Getenv(keyEnv), mediaHTTPClient(metadataPolicy.RequestTimeout))
	if err != nil {
		return fmt.Errorf("organization provider credentials are unavailable in %s", keyEnv)
	}
	if metadataPolicy.RetryLimit > 0 {
		client.SetRetryLimit(metadataPolicy.RetryLimit)
	}
	folderFormat := policy.FolderFormat
	if selected.Type == "shows" && policy.SeriesFormat != "" {
		folderFormat = policy.SeriesFormat
	}
	stableFor := 10 * time.Minute
	stableValue := selected.Media.Inbox.StableFor
	if strings.TrimSpace(stableValue) == "" {
		stableValue = policy.StableFor
	}
	if strings.TrimSpace(stableValue) != "" {
		stableFor, err = time.ParseDuration(stableValue)
		if err != nil || stableFor <= 0 {
			return fmt.Errorf("invalid Inbox stable_for %q", stableValue)
		}
	}
	marker := selected.Media.Inbox.CompletionMarker
	if strings.TrimSpace(marker) == "" {
		marker = policy.CompletionMarker
	}
	opts := organization.Options{
		Inventory: inventory, IncludeProviderID: policy.IncludeProviderID, EnableDiscFolders: policy.DiscFolders,
		FlattenGroupingDirectories: !policy.PreserveGroupingDirectories, MinimumConfidence: policy.MinimumConfidence,
		FolderFormat: folderFormat, FileFormat: policy.FileFormat, SeasonFormat: policy.SeasonFormat, EpisodeFormat: policy.EpisodeFormat,
		Language: metadataPolicy.Language, FallbackLanguages: policy.EpisodeFallbackLanguages, AllowedExtensions: policy.AllowedExtensions,
		StableFor: stableFor, CompletionMarker: marker,
	}
	if *itemPath != "" {
		if *existing {
			filtered := make([]collection.InventoryEntry, 0)
			for _, entry := range inventory {
				if entry.Scope == collection.InventoryScopeLibrary && (entry.Path == *itemPath || strings.HasPrefix(entry.Path, *itemPath+"/")) {
					filtered = append(filtered, entry)
				}
			}
			opts.Inventory = filtered
		} else {
			root := organization.InboxItemRoot(*itemPath, coll.Type)
			opts.ItemRoots = []string{root}
		}
	}
	dryRun := action == "plan"
	var report organization.Report
	if coll.Type == collection.CollectionTypeShows {
		if *existing {
			report, err = organization.OrganizeExistingShows(ctx, coll, client, repo, opts, dryRun, "")
		} else {
			report, err = organization.OrganizeInboxShows(ctx, coll, client, repo, opts, dryRun, "")
		}
	} else if *existing {
		report, err = organization.OrganizeExistingMovies(ctx, coll, client, repo, opts, dryRun, "")
	} else {
		report, err = organization.OrganizeInboxMovies(ctx, coll, client, repo, opts, dryRun, "")
	}
	if err != nil {
		return fmt.Errorf("%s organization for %q: %w", action, libraryID, err)
	}
	return json.NewEncoder(os.Stdout).Encode(report)
}

func runLibraryInboxCommand(ctx context.Context, repo database.Repository, config configFile, args []string) error {
	if len(args) > 0 && args[0] == "status" {
		return runLibraryInboxStatusCommand(ctx, repo, config, args[1:])
	}
	if len(args) != 3 || args[0] != "stage" {
		return errors.New("usage: rcfin library inbox <status LIBRARY_ID [--item PATH]|stage LIBRARY_ID ITEM_PATH>")
	}
	libraryID, source := args[1], args[2]
	var selected *collectionConfig
	for i := range config.Collections {
		if config.Collections[i].ID == libraryID {
			selected = &config.Collections[i]
			break
		}
	}
	if selected == nil {
		return fmt.Errorf("unknown library ID %q", libraryID)
	}
	if err := rclonestore.Configure(config.Rclone.Config, config.Rclone.CacheDir); err != nil {
		return fmt.Errorf("configure Rclone: %w", err)
	}
	storage, err := rclonestore.New(ctx, selected.Remote, selected.VFS)
	if err != nil {
		return fmt.Errorf("open library remote: %w", err)
	}
	defer storage.Close()
	inbox := selected.Media.InboxPath
	if inbox == "" {
		inbox = ".Inbox"
	}
	coll := &collection.Collection{ID: selected.ID, Name: selected.Name, Type: collection.CollectionType(selected.Type), Remote: selected.Remote, Storage: storage, InboxPath: inbox}
	operation, err := organization.QueueExistingItemForInbox(ctx, coll, repo, source)
	if err != nil {
		return fmt.Errorf("stage existing item: %w", err)
	}
	return json.NewEncoder(os.Stdout).Encode(operation)
}

func runLibraryInboxStatusCommand(ctx context.Context, repo database.Repository, config configFile, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: rcfin library inbox status LIBRARY_ID [--item PATH]")
	}
	libraryID := args[0]
	flags := pflag.NewFlagSet("library inbox status", pflag.ContinueOnError)
	flags.SetOutput(io.Discard)
	itemPath := flags.String("item", "", "Filter to one relative Inbox item path")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected arguments after Inbox status flags")
	}
	var selected *collectionConfig
	for i := range config.Collections {
		if config.Collections[i].ID == libraryID {
			selected = &config.Collections[i]
			break
		}
	}
	if selected == nil {
		return fmt.Errorf("unknown library ID %q", libraryID)
	}
	inboxPath := strings.TrimSpace(selected.Media.InboxPath)
	if inboxPath == "" {
		inboxPath = ".Inbox"
	}
	snapshot, err := repo.GetLibraryInventory(ctx, libraryID)
	if err != nil {
		return fmt.Errorf("read library inventory: %w", err)
	}
	if snapshot == nil {
		return fmt.Errorf("library %q has no completed inventory; run `rcfin library scan %s`", libraryID, libraryID)
	}
	clean := strings.TrimSpace(*itemPath)
	if clean != "" && (path.IsAbs(clean) || path.Clean(clean) != clean || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "\\")) {
		return errors.New("item path must be a clean relative path within the library")
	}
	entries := make([]model.LibraryInventoryEntry, 0)
	for _, entry := range snapshot.Entries {
		if entry.Scope != collection.InventoryScopeInbox {
			continue
		}
		if clean != "" && entry.Path != clean && !strings.HasPrefix(entry.Path, clean+"/") && !strings.HasPrefix(clean, entry.Path+"/") {
			continue
		}
		entries = append(entries, entry)
	}
	operations, err := database.ListAllMediaOperations(ctx, repo, libraryID)
	if err != nil {
		return fmt.Errorf("read Inbox operation status: %w", err)
	}
	relevant := make([]model.MediaOperation, 0)
	for _, operation := range operations {
		isInboxWork := operation.Action == "inbox_stability" || operation.Action == "organization_review" || strings.HasPrefix(operation.ItemKey, inboxPath+"/") || operation.ItemKey == inboxPath
		if !isInboxWork {
			continue
		}
		if clean != "" && operation.ItemKey != clean && !strings.HasPrefix(operation.ItemKey, clean+"/") && !strings.HasPrefix(clean, operation.ItemKey+"/") {
			continue
		}
		relevant = append(relevant, operation)
	}
	return json.NewEncoder(os.Stdout).Encode(struct {
		CollectionID string                        `json:"collection_id"`
		Generation   int64                         `json:"generation"`
		CompletedAt  time.Time                     `json:"completed_at"`
		Entries      []model.LibraryInventoryEntry `json:"inbox_entries"`
		Operations   []model.MediaOperation        `json:"operations"`
	}{libraryID, snapshot.Generation, snapshot.CompletedAt, entries, relevant})
}

func runLibraryRefreshCommand(ctx context.Context, repo database.Repository, config configFile, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: rcfin library refresh <metadata|subtitles> LIBRARY_ID [--item PATH] [--language LANG] [--replace]")
	}
	stage, libraryID := args[0], args[1]
	if stage != "metadata" && stage != "subtitles" {
		return fmt.Errorf("unsupported refresh stage %q; expected metadata or subtitles", stage)
	}
	var selected *collectionConfig
	for i := range config.Collections {
		if config.Collections[i].ID == libraryID {
			selected = &config.Collections[i]
			break
		}
	}
	if selected == nil {
		return fmt.Errorf("unknown library ID %q", libraryID)
	}
	flags := pflag.NewFlagSet("library refresh", pflag.ContinueOnError)
	itemPath := flags.String("item", "", "Item path, file or containing folder")
	replace := flags.Bool("replace", false, "Allow replacing existing metadata NFOs or subtitles for this selected stage")
	replaceArtwork := flags.Bool("replace-artwork", false, "Allow replacing existing posters and backdrops during metadata refresh")
	language := flags.StringArray("language", nil, "Subtitle language (repeatable; subtitles stage only)")
	if err := flags.Parse(args[2:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected arguments after library refresh flags")
	}
	if stage == "metadata" && len(*language) > 0 {
		return errors.New("--language is only valid for subtitle refresh")
	}
	if stage == "subtitles" && *replaceArtwork {
		return errors.New("--replace-artwork is only valid for metadata refresh")
	}
	if err := rclonestore.Configure(config.Rclone.Config, config.Rclone.CacheDir); err != nil {
		return fmt.Errorf("configure Rclone: %w", err)
	}
	storage, err := rclonestore.New(ctx, selected.Remote, selected.VFS)
	if err != nil {
		return fmt.Errorf("open library remote: %w", err)
	}
	defer storage.Close()
	inbox := selected.Media.InboxPath
	if inbox == "" {
		inbox = ".Inbox"
	}
	coll := &collection.Collection{ID: selected.ID, Name: selected.Name, Type: collection.CollectionType(selected.Type), Remote: selected.Remote, Storage: storage, InboxPath: inbox}
	storage.FlushDirCache()
	inventory, err := collection.InventoryLibrary(ctx, coll, inbox)
	if err != nil {
		return fmt.Errorf("inventory library: %w", err)
	}
	reconciliation, err := collection.ReconcileLibraryInventory(ctx, repo, libraryID, inventory, time.Now().UTC(), inventoryVersionsForLibrary(*selected))
	if err != nil {
		return fmt.Errorf("save inventory snapshot: %w", err)
	}
	if err := relinkMovedMediaOperations(ctx, repo, libraryID, coll.Type, reconciliation.Changes, inventory); err != nil {
		return fmt.Errorf("relink moved media operations: %w", err)
	}
	pathsByScope, err := selectRefreshPaths(inventory, *itemPath)
	if err != nil {
		return err
	}
	readyInbox, err := readyInboxVideoPaths(ctx, coll, repo, *selected, inventory)
	if err != nil {
		return fmt.Errorf("check Inbox readiness: %w", err)
	}
	readySet := make(map[string]bool, len(readyInbox))
	for _, path := range readyInbox {
		readySet[path] = true
	}
	filteredInbox := make([]string, 0, len(pathsByScope[collection.InventoryScopeInbox]))
	for _, path := range pathsByScope[collection.InventoryScopeInbox] {
		if readySet[path] {
			filteredInbox = append(filteredInbox, path)
		}
	}
	if *itemPath != "" && len(pathsByScope[collection.InventoryScopeInbox]) > 0 && len(filteredInbox) == 0 && len(pathsByScope[collection.InventoryScopeLibrary]) == 0 {
		return fmt.Errorf("Inbox item %q is still pending transfer; wait for its completion marker or stable_for interval", *itemPath)
	}
	pathsByScope[collection.InventoryScopeInbox] = filteredInbox
	if len(pathsByScope[collection.InventoryScopeInbox])+len(pathsByScope[collection.InventoryScopeLibrary]) == 0 {
		return errors.New("no matching video files found for refresh")
	}

	switch stage {
	case "metadata":
		policy := selected.Media.Metadata
		if !policy.FetchNFO && !policy.FetchArtwork {
			return errors.New("metadata refresh has no configured output; enable fetch_nfo or fetch_artwork")
		}
		keyEnv := strings.TrimSpace(policy.APIKeyEnv)
		if keyEnv == "" {
			keyEnv = "RCFIN_TMDB_API_KEY"
		}
		client, err := provider.NewTMDb(os.Getenv(keyEnv), mediaHTTPClient(policy.RequestTimeout))
		if err != nil {
			return fmt.Errorf("metadata provider credential environment variable %s is empty", keyEnv)
		}
		if policy.RetryLimit > 0 {
			client.SetRetryLimit(policy.RetryLimit)
		}
		if *replace {
			policy.OverwriteExisting = true
		}
		if *replaceArtwork {
			policy.OverwriteArtwork = true
		}
		var reports []metadatarefresh.Report
		for _, scope := range []string{collection.InventoryScopeLibrary, collection.InventoryScopeInbox} {
			paths := pathsByScope[scope]
			if len(paths) == 0 {
				continue
			}
			report, err := metadatarefresh.Run(ctx, coll, client, metadatarefresh.Options{
				Write: true, Force: true, AlgorithmVersion: metadatarefresh.AlgorithmVersion,
				InboxOnly: scope == collection.InventoryScopeInbox, Inventory: inventory, ItemPaths: paths,
				Journal: repo, FetchNFO: policy.FetchNFO, FetchArtwork: policy.FetchArtwork,
				OverwriteExisting: policy.OverwriteExisting, OverwriteArtwork: policy.OverwriteArtwork,
				Language: policy.Language, FallbackLanguages: policy.FallbackLanguages,
				ArtworkMaxBytes: policy.ArtworkMaxBytes, ArtworkMaxWidth: policy.ArtworkMaxWidth,
				ArtworkMaxHeight: policy.ArtworkMaxHeight, ArtworkFormats: policy.ArtworkFormats,
				ArtworkPosterSize: policy.ArtworkPosterSize, ArtworkBackdropSize: policy.ArtworkBackdropSize,
				MinimumConfidence: policy.MinimumConfidence,
			})
			if err != nil {
				return fmt.Errorf("metadata refresh: %w", err)
			}
			reports = append(reports, report)
		}
		return json.NewEncoder(os.Stdout).Encode(reports)
	case "subtitles":
		policy := selected.Media.Subtitles
		keyEnv, usernameEnv, passwordEnv := strings.TrimSpace(policy.APIKeyEnv), strings.TrimSpace(policy.UsernameEnv), strings.TrimSpace(policy.PasswordEnv)
		if keyEnv == "" {
			keyEnv = "RCFIN_OPENSUBTITLES_API_KEY"
		}
		if usernameEnv == "" {
			usernameEnv = "RCFIN_OPENSUBTITLES_USERNAME"
		}
		if passwordEnv == "" {
			passwordEnv = "RCFIN_OPENSUBTITLES_PASSWORD"
		}
		client, err := subtitles.NewOpenSubtitles(os.Getenv(keyEnv), os.Getenv(usernameEnv), os.Getenv(passwordEnv), mediaHTTPClient(policy.RequestTimeout))
		if err != nil {
			return fmt.Errorf("subtitle provider credential environment variable %s is empty", keyEnv)
		}
		if policy.RetryLimit > 0 {
			client.SetRetryLimit(policy.RetryLimit)
		}
		if policy.MaxDownloadBytes > 0 {
			client.SetMaxDownloadBytes(policy.MaxDownloadBytes)
		}
		if *replace {
			policy.OverwriteExisting = true
		}
		languages := policy.Languages
		if len(*language) > 0 {
			languages = *language
		}
		if len(languages) == 0 {
			return errors.New("subtitle refresh requires at least one configured language or --language")
		}
		var reports []subtitles.SubtitleReport
		for _, scope := range []string{collection.InventoryScopeLibrary, collection.InventoryScopeInbox} {
			paths := pathsByScope[scope]
			if len(paths) == 0 {
				continue
			}
			report, err := subtitles.Refresh(ctx, coll, client, subtitles.RefreshOptions{
				Force: true, AlgorithmVersion: subtitles.AlgorithmVersion,
				InboxOnly: scope == collection.InventoryScopeInbox, Inventory: inventory, ItemPaths: paths,
				Journal: repo, Languages: languages, Formats: policy.Formats,
				HearingImpaired: policy.HearingImpaired, Forced: policy.Forced,
				OverwriteExisting: policy.OverwriteExisting, MinimumConfidence: policy.MinimumConfidence, Write: true,
			})
			if err != nil {
				return fmt.Errorf("subtitle refresh: %w", err)
			}
			reports = append(reports, report)
		}
		return json.NewEncoder(os.Stdout).Encode(reports)
	}
	return nil
}

func selectRefreshPaths(inventory []collection.InventoryEntry, requested string) (map[string][]string, error) {
	paths := map[string][]string{collection.InventoryScopeLibrary: {}, collection.InventoryScopeInbox: {}}
	requested = strings.TrimSpace(requested)
	if requested != "" && (path.IsAbs(requested) || path.Clean(requested) != requested || strings.HasPrefix(requested, "../")) {
		return nil, fmt.Errorf("item path must be a safe library-relative path")
	}
	matched := false
	for _, entry := range inventory {
		if entry.Role != collection.InventoryVideo || entry.IsDir {
			continue
		}
		if requested != "" && entry.Path != requested && !strings.HasPrefix(entry.Path, requested+"/") {
			continue
		}
		paths[entry.Scope] = appendUnique(paths[entry.Scope], entry.Path)
		matched = true
	}
	if requested != "" && !matched {
		return nil, fmt.Errorf("no video files matched item path %q", requested)
	}
	return paths, nil
}

func runLibraryScanCommand(ctx context.Context, repo database.Repository, config configFile, args []string) error {
	if len(args) < 1 || len(args) > 2 || (len(args) == 2 && args[1] != "--full") {
		return errors.New("usage: rcfin library scan LIBRARY_ID [--full]")
	}
	libraryID := args[0]
	var selected *collectionConfig
	for i := range config.Collections {
		if config.Collections[i].ID == libraryID {
			selected = &config.Collections[i]
			break
		}
	}
	if selected == nil {
		return fmt.Errorf("unknown library ID %q", libraryID)
	}
	if err := rclonestore.Configure(config.Rclone.Config, config.Rclone.CacheDir); err != nil {
		return fmt.Errorf("configure Rclone: %w", err)
	}
	storage, err := rclonestore.New(ctx, selected.Remote, selected.VFS)
	if err != nil {
		return fmt.Errorf("open library remote: %w", err)
	}
	defer storage.Close()
	inbox := selected.Media.InboxPath
	if inbox == "" {
		inbox = ".Inbox"
	}
	coll := &collection.Collection{ID: selected.ID, Name: selected.Name, Type: collection.CollectionType(selected.Type), Remote: selected.Remote, Storage: storage, InboxPath: inbox}
	entries, err := collection.InventoryLibrary(ctx, coll, inbox)
	if err != nil {
		return fmt.Errorf("inventory library: %w", err)
	}
	reconciliation, err := collection.ReconcileLibraryInventory(ctx, repo, selected.ID, entries, time.Now().UTC(), inventoryVersionsForLibrary(*selected))
	if err != nil {
		return fmt.Errorf("reconcile library inventory: %w", err)
	}
	if err := relinkMovedMediaOperations(ctx, repo, selected.ID, collection.CollectionType(selected.Type), reconciliation.Changes, entries); err != nil {
		return fmt.Errorf("relink moved media operations: %w", err)
	}
	return json.NewEncoder(os.Stdout).Encode(reconciliation)
}

func runLibraryStatusCommand(ctx context.Context, repo database.Repository, libraries []collectionConfig, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: rcfin library status LIBRARY_ID")
	}
	libraryID := args[0]
	if !configuredLibrary(libraries, libraryID) {
		return fmt.Errorf("unknown library ID %q", libraryID)
	}
	snapshot, err := repo.GetLibraryInventory(ctx, libraryID)
	if err != nil {
		return fmt.Errorf("read library inventory status: %w", err)
	}
	if snapshot == nil {
		return fmt.Errorf("library %q has no completed inventory; run `rcfin library scan %s`", libraryID, libraryID)
	}
	return json.NewEncoder(os.Stdout).Encode(snapshot)
}

func runMediaCommand(ctx context.Context, repo database.Repository, libraries []collectionConfig, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: rcfin media <inventory|plan|status|refresh|retry> [arguments]")
	}
	switch args[0] {
	case "status":
		if len(args) > 2 {
			return errors.New("usage: rcfin media status [LIBRARY_ID]")
		}
		selected := libraries
		if len(args) == 2 {
			selected = nil
			for _, library := range libraries {
				if library.ID == args[1] {
					selected = append(selected, library)
				}
			}
			if len(selected) == 0 {
				return fmt.Errorf("unknown library ID %q", args[1])
			}
		}
		for _, library := range selected {
			operations, err := database.ListAllMediaOperations(ctx, repo, library.ID)
			if err != nil {
				return fmt.Errorf("list media operations for %q: %w", library.ID, err)
			}
			for _, op := range operations {
				fmt.Printf("%-16s %-24s %-24s %-20s attempts=%d next=%s source=%s destination=%s error=%s\n", library.ID, op.ItemKey, op.Action, op.Status, op.Attempts, op.NextAttempt.UTC().Format(time.RFC3339), op.SourcePath, op.DestinationPath, op.Error)
			}
		}
		return nil
	case "retry":
		if len(args) != 4 {
			return errors.New("usage: rcfin media retry LIBRARY_ID ITEM_KEY ACTION")
		}
		libraryID, itemKey, action := args[1], args[2], args[3]
		if !configuredLibrary(libraries, libraryID) {
			return fmt.Errorf("unknown library ID %q", libraryID)
		}
		op, err := repo.GetMediaOperation(ctx, libraryID, itemKey, action)
		if err != nil {
			return fmt.Errorf("look up media operation: %w", err)
		}
		if op == nil {
			return fmt.Errorf("media operation %q for %q not found", action, itemKey)
		}
		if op.Status == "needs_review" {
			return errors.New("operation needs review; resolve its identity or conflict before retrying")
		}
		if op.Status != "failed_retryable" && op.Status != "failed_terminal" && op.Status != "failed" && op.Status != "retryable" {
			return fmt.Errorf("operation status %q is not retryable", op.Status)
		}
		op.Status = "planned"
		op.NextAttempt = time.Now().UTC()
		op.LeaseOwner = ""
		op.LeaseUntil = time.Now().UTC()
		op.Error = ""
		op.UpdatedAt = time.Now().UTC()
		if err := repo.UpsertMediaOperation(ctx, op); err != nil {
			return fmt.Errorf("queue media operation retry: %w", err)
		}
		fmt.Printf("Queued retry for %s %s %s; the running RCFin server will pick it up on its next media cycle.\n", libraryID, itemKey, action)
		return nil
	case "refresh":
		if len(args) < 2 || len(args) > 3 {
			return errors.New("usage: rcfin media refresh LIBRARY_ID [ITEM_KEY]")
		}
		libraryID := args[1]
		if !configuredLibrary(libraries, libraryID) {
			return fmt.Errorf("unknown library ID %q", libraryID)
		}
		operations, err := database.ListAllMediaOperations(ctx, repo, libraryID)
		if err != nil {
			return fmt.Errorf("list media operations for %q: %w", libraryID, err)
		}
		queued := 0
		for _, op := range operations {
			if len(args) == 3 && op.ItemKey != args[2] {
				continue
			}
			refreshable := op.Action == "metadata_refresh" || strings.HasPrefix(op.Action, "subtitle_refresh:")
			if !refreshable || op.Status != "complete" {
				continue
			}
			op.Status = "planned"
			op.NextAttempt = time.Now().UTC()
			op.LeaseOwner = ""
			op.LeaseUntil = time.Now().UTC()
			op.Error = ""
			op.UpdatedAt = time.Now().UTC()
			if err := repo.UpsertMediaOperation(ctx, &op); err != nil {
				return fmt.Errorf("queue refresh for %q %s: %w", op.ItemKey, op.Action, err)
			}
			queued++
		}
		if queued == 0 {
			return errors.New("no completed metadata or subtitle refresh operations matched; use the server's next scan to discover new items")
		}
		fmt.Printf("Queued %d metadata/subtitle refresh operation(s) for %s; enabled workers will process them on their next media cycle.\n", queued, libraryID)
		return nil
	default:
		return fmt.Errorf("unknown media command %q (expected status, refresh, or retry)", args[0])
	}
}

func runMediaPlanCommand(ctx context.Context, config configFile, args []string) error {
	if len(args) < 2 || args[0] != "metadata" && args[0] != "subtitles" && args[0] != "organization" {
		return errors.New("usage: rcfin media plan <metadata|subtitles> LIBRARY_ID | rcfin media plan organization LIBRARY_ID [--existing]")
	}
	existingOnly := false
	if args[0] == "organization" {
		if len(args) > 3 || len(args) == 3 && args[2] != "--existing" {
			return errors.New("usage: rcfin media plan organization LIBRARY_ID [--existing]")
		}
		existingOnly = len(args) == 3
	} else if len(args) != 2 {
		return errors.New("usage: rcfin media plan <metadata|subtitles> LIBRARY_ID")
	}
	var selected *collectionConfig
	for i := range config.Collections {
		if config.Collections[i].ID == args[1] {
			selected = &config.Collections[i]
			break
		}
	}
	if selected == nil {
		return fmt.Errorf("unknown library ID %q", args[1])
	}
	if err := rclonestore.Configure(config.Rclone.Config, config.Rclone.CacheDir); err != nil {
		return fmt.Errorf("configure Rclone: %w", err)
	}
	storage, err := rclonestore.New(ctx, selected.Remote, selected.VFS)
	if err != nil {
		return fmt.Errorf("open library %q: %w", selected.ID, err)
	}
	defer storage.Close()
	inbox := selected.Media.InboxPath
	if inbox == "" {
		inbox = ".Inbox"
	}
	coll := &collection.Collection{ID: selected.ID, Type: collection.CollectionType(selected.Type), Storage: storage, InboxPath: inbox}
	var report any
	if args[0] == "organization" {
		repo, dbErr := openConfiguredDatabase(config)
		if dbErr != nil {
			return fmt.Errorf("open database: %w", dbErr)
		}
		defer repo.Close()
		metadataPolicy := selected.Media.Metadata
		providerName := strings.ToLower(strings.TrimSpace(metadataPolicy.Provider))
		if providerName == "" {
			providerName = "tmdb"
		}
		if providerName != "tmdb" {
			return fmt.Errorf("unsupported organization metadata provider %q", providerName)
		}
		keyEnv := strings.TrimSpace(metadataPolicy.APIKeyEnv)
		if keyEnv == "" {
			keyEnv = "RCFIN_TMDB_API_KEY"
		}
		client, clientErr := provider.NewTMDb(os.Getenv(keyEnv), mediaHTTPClient(metadataPolicy.RequestTimeout))
		if clientErr != nil {
			return fmt.Errorf("organization provider credentials are unavailable in %s", keyEnv)
		}
		if metadataPolicy.RetryLimit > 0 {
			client.SetRetryLimit(metadataPolicy.RetryLimit)
		}
		policy := selected.Media.Organize
		folderFormat := policy.FolderFormat
		if selected.Type == "shows" && policy.SeriesFormat != "" {
			folderFormat = policy.SeriesFormat
		}
		stableFor := 10 * time.Minute
		stableConfig := selected.Media.Inbox.StableFor
		if strings.TrimSpace(stableConfig) == "" {
			stableConfig = policy.StableFor
		}
		if configured := strings.TrimSpace(stableConfig); configured != "" {
			if parsed, parseErr := time.ParseDuration(configured); parseErr == nil {
				stableFor = parsed
			}
		}
		completionMarker := selected.Media.Inbox.CompletionMarker
		if strings.TrimSpace(completionMarker) == "" {
			completionMarker = policy.CompletionMarker
		}
		opts := organization.Options{IncludeProviderID: policy.IncludeProviderID, EnableDiscFolders: policy.DiscFolders, FlattenGroupingDirectories: !policy.PreserveGroupingDirectories, MinimumConfidence: policy.MinimumConfidence, FolderFormat: folderFormat, FileFormat: policy.FileFormat, SeasonFormat: policy.SeasonFormat, EpisodeFormat: policy.EpisodeFormat, Language: metadataPolicy.Language, FallbackLanguages: policy.EpisodeFallbackLanguages, AllowedExtensions: policy.AllowedExtensions, StableFor: stableFor, CompletionMarker: completionMarker}
		var plan organization.Report
		var planErr error
		if selected.Type == "shows" {
			if existingOnly {
				plan, planErr = organization.OrganizeExistingShows(ctx, coll, client, repo, opts, true, "")
			} else {
				plan, planErr = organization.OrganizeInboxShows(ctx, coll, client, repo, opts, true, "")
			}
		} else if existingOnly {
			plan, planErr = organization.OrganizeExistingMovies(ctx, coll, client, repo, opts, true, "")
		} else {
			plan, planErr = organization.OrganizeInboxMovies(ctx, coll, client, repo, opts, true, "")
		}
		if planErr != nil {
			return fmt.Errorf("plan organization for %q: %w", selected.ID, planErr)
		}
		report = plan
	} else if args[0] == "metadata" {
		policy := selected.Media.Metadata
		if !policy.FetchNFO && !policy.FetchArtwork {
			return errors.New("enable at least one metadata output to plan a metadata refresh")
		}
		providerName := strings.ToLower(strings.TrimSpace(policy.Provider))
		if providerName == "" {
			providerName = "tmdb"
		}
		if providerName != "tmdb" {
			return fmt.Errorf("unsupported metadata provider %q", providerName)
		}
		keyEnv := strings.TrimSpace(policy.APIKeyEnv)
		if keyEnv == "" {
			keyEnv = "RCFIN_TMDB_API_KEY"
		}
		client, clientErr := provider.NewTMDb(os.Getenv(keyEnv), mediaHTTPClient(policy.RequestTimeout))
		if clientErr != nil {
			return fmt.Errorf("metadata provider credentials are unavailable in %s", keyEnv)
		}
		if policy.RetryLimit > 0 {
			client.SetRetryLimit(policy.RetryLimit)
		}
		refreshAfter := 24 * time.Hour
		if configured := strings.TrimSpace(policy.RefreshExistingAfter); configured != "" {
			if parsed, parseErr := time.ParseDuration(configured); parseErr == nil {
				refreshAfter = parsed
			}
		}
		metadataReport, runErr := metadatarefresh.Run(ctx, coll, client, metadatarefresh.Options{Write: false, FetchNFO: policy.FetchNFO, FetchArtwork: policy.FetchArtwork, OverwriteExisting: policy.OverwriteExisting, OverwriteArtwork: policy.OverwriteArtwork, Language: policy.Language, FallbackLanguages: policy.FallbackLanguages, ArtworkMaxBytes: policy.ArtworkMaxBytes, ArtworkMaxWidth: policy.ArtworkMaxWidth, ArtworkMaxHeight: policy.ArtworkMaxHeight, ArtworkFormats: policy.ArtworkFormats, ArtworkPosterSize: policy.ArtworkPosterSize, ArtworkBackdropSize: policy.ArtworkBackdropSize, MinimumConfidence: policy.MinimumConfidence, RefreshExistingAfter: refreshAfter})
		if runErr != nil {
			return fmt.Errorf("plan metadata refresh for %q: %w", selected.ID, runErr)
		}
		report = metadataReport
	} else {
		policy := selected.Media.Subtitles
		if len(policy.Languages) == 0 {
			return errors.New("configure at least one subtitle language to plan subtitle refresh")
		}
		providerName := strings.ToLower(strings.TrimSpace(policy.Provider))
		if providerName == "" {
			providerName = "opensubtitles"
		}
		if providerName != "opensubtitles" {
			return fmt.Errorf("unsupported subtitle provider %q", providerName)
		}
		apiKeyEnv := strings.TrimSpace(policy.APIKeyEnv)
		if apiKeyEnv == "" {
			apiKeyEnv = "RCFIN_OPENSUBTITLES_API_KEY"
		}
		usernameEnv := strings.TrimSpace(policy.UsernameEnv)
		if usernameEnv == "" {
			usernameEnv = "RCFIN_OPENSUBTITLES_USERNAME"
		}
		passwordEnv := strings.TrimSpace(policy.PasswordEnv)
		if passwordEnv == "" {
			passwordEnv = "RCFIN_OPENSUBTITLES_PASSWORD"
		}
		client, clientErr := subtitles.NewOpenSubtitles(os.Getenv(apiKeyEnv), os.Getenv(usernameEnv), os.Getenv(passwordEnv), mediaHTTPClient(policy.RequestTimeout))
		if clientErr != nil {
			return fmt.Errorf("subtitle provider credentials are unavailable in %s, %s, or %s", apiKeyEnv, usernameEnv, passwordEnv)
		}
		if policy.RetryLimit > 0 {
			client.SetRetryLimit(policy.RetryLimit)
		}
		if policy.MaxDownloadBytes > 0 {
			client.SetMaxDownloadBytes(policy.MaxDownloadBytes)
		}
		subtitleReport, runErr := subtitles.Refresh(ctx, coll, client, subtitles.RefreshOptions{Languages: policy.Languages, Formats: policy.Formats, HearingImpaired: policy.HearingImpaired, Forced: policy.Forced, OverwriteExisting: policy.OverwriteExisting, MinimumConfidence: policy.MinimumConfidence, Write: false})
		if runErr != nil {
			return fmt.Errorf("plan subtitle refresh for %q: %w", selected.ID, runErr)
		}
		report = subtitleReport
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func runMediaInventoryCommand(ctx context.Context, config configFile, args []string) error {
	if len(args) < 1 || len(args) > 2 || (len(args) == 2 && args[1] != "--inbox") {
		return errors.New("usage: rcfin media inventory LIBRARY_ID [--inbox]")
	}
	var selected *collectionConfig
	for i := range config.Collections {
		if config.Collections[i].ID == args[0] {
			selected = &config.Collections[i]
			break
		}
	}
	if selected == nil {
		return fmt.Errorf("unknown library ID %q", args[0])
	}
	if err := rclonestore.Configure(config.Rclone.Config, config.Rclone.CacheDir); err != nil {
		return fmt.Errorf("configure Rclone: %w", err)
	}
	storage, err := rclonestore.New(ctx, selected.Remote, selected.VFS)
	if err != nil {
		return fmt.Errorf("open library %q: %w", selected.ID, err)
	}
	defer storage.Close()
	inbox := selected.Media.InboxPath
	if inbox == "" {
		inbox = ".Inbox"
	}
	inboxOnly := len(args) == 2
	entries, err := collection.InventoryCollection(ctx, &collection.Collection{ID: selected.ID, Type: collection.CollectionType(selected.Type), Storage: storage, InboxPath: inbox}, inbox, inboxOnly)
	if err != nil {
		return fmt.Errorf("inventory library %q: %w", selected.ID, err)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(struct {
		CollectionID string                      `json:"collection_id"`
		Scope        string                      `json:"scope"`
		Entries      []collection.InventoryEntry `json:"entries"`
	}{CollectionID: selected.ID, Scope: map[bool]string{true: "inbox", false: "library"}[inboxOnly], Entries: entries})
}

func runUserCommand(ctx context.Context, repo database.Repository, libraries []collectionConfig, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: rcfin user <add|list|delete|disable|enable|grant-admin|revoke-admin|password|libraries>")
	}
	switch args[0] {
	case "add":
		flags := pflag.NewFlagSet("user add", pflag.ContinueOnError)
		flags.SetOutput(io.Discard)
		usernameFlag := flags.String("username", "", "Account username")
		admin := flags.Bool("admin", false, "Create as administrator")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		username := strings.ToLower(strings.TrimSpace(*usernameFlag))
		if flags.NArg() != 0 || username == "" {
			return errors.New("usage: rcfin user add --username NAME [--admin]")
		}
		if _, err := repo.GetUser(ctx, username); err == nil {
			return fmt.Errorf("user %q already exists", username)
		} else if !errors.Is(err, model.ErrNotFound) {
			return fmt.Errorf("look up user: %w", err)
		}
		password, err := promptPassword("New password: ")
		if err != nil {
			return err
		}
		confirmation, err := promptPassword("Confirm password: ")
		if err != nil {
			return err
		}
		if password == "" || password != confirmation {
			return errors.New("passwords must be non-empty and match")
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			return fmt.Errorf("hash password: %w", err)
		}
		props := model.UserPropertiesDefault()
		props.Admin = *admin
		props.EnableAllFolders = *admin
		props.EnabledFolders = []string{}
		user := &model.User{ID: idhash.NewRandomID(), Username: username, Password: string(hash), CreatedAt: time.Now().UTC(), Properties: props}
		if err := repo.UpsertUser(ctx, user); err != nil {
			return fmt.Errorf("create user: %w", err)
		}
		fmt.Printf("Created user %q (admin=%t)\n", user.Username, props.Admin)
		return nil
	case "list":
		if len(args) != 1 {
			return errors.New("usage: rcfin user list")
		}
		users, err := repo.GetAllUsers(ctx)
		if err != nil {
			return fmt.Errorf("list users: %w", err)
		}
		for _, user := range users {
			state, role := "enabled", "user"
			if user.Properties.Disabled {
				state = "disabled"
			}
			if user.Properties.Admin {
				role = "admin"
			}
			fmt.Printf("%-24s %-8s %s\n", user.Username, role, state)
		}
		return nil
	case "password":
		if len(args) != 3 || args[1] != "reset" {
			return errors.New("usage: rcfin user password reset USERNAME")
		}
		user, err := getUserByName(ctx, repo, args[2])
		if err != nil {
			return err
		}
		if err := resetUserPassword(ctx, repo, user); err != nil {
			return err
		}
		fmt.Printf("Reset password for %q\n", user.Username)
		return nil
	case "disable", "enable", "delete", "grant-admin", "revoke-admin":
		if len(args) != 2 {
			return fmt.Errorf("usage: rcfin user %s USERNAME", args[0])
		}
		user, err := getUserByName(ctx, repo, args[1])
		if err != nil {
			return err
		}
		switch args[0] {
		case "disable":
			if user.Properties.Admin && !user.Properties.Disabled && enabledAdminCount(ctx, repo, user.ID) == 0 {
				return errors.New("cannot disable the last enabled administrator")
			}
			user.Properties.Disabled = true
			if err := repo.UpsertUser(ctx, user); err != nil {
				return fmt.Errorf("disable user: %w", err)
			}
			if err := revokeUserTokens(ctx, repo, user.ID); err != nil {
				return err
			}
		case "enable":
			user.Properties.Disabled = false
			if err := repo.UpsertUser(ctx, user); err != nil {
				return fmt.Errorf("enable user: %w", err)
			}
		case "delete":
			if user.Properties.Admin && enabledAdminCount(ctx, repo, user.ID) == 0 {
				return errors.New("cannot delete the last enabled administrator")
			}
			if err := repo.DeleteUser(ctx, user.ID); err != nil {
				return fmt.Errorf("delete user: %w", err)
			}
		case "grant-admin":
			user.Properties.Admin = true
			user.Properties.EnableAllFolders = true
			user.Properties.EnableDownloads = true
			if err := repo.UpsertUser(ctx, user); err != nil {
				return fmt.Errorf("promote user: %w", err)
			}
		case "revoke-admin":
			if !user.Properties.Admin {
				return fmt.Errorf("user %q is not an administrator", user.Username)
			}
			if user.Properties.Admin && enabledAdminCount(ctx, repo, user.ID) == 0 {
				return errors.New("cannot revoke admin from the last enabled administrator")
			}
			user.Properties.Admin = false
			user.Properties.EnableAllFolders = false
			user.Properties.EnabledFolders = []string{}
			if err := repo.UpsertUser(ctx, user); err != nil {
				return fmt.Errorf("revoke admin: %w", err)
			}
		}
		fmt.Printf("Updated user %q\n", user.Username)
		return nil
	case "libraries":
		if len(args) < 2 {
			return errors.New("usage: rcfin user libraries USERNAME [grant|revoke LIBRARY_ID]")
		}
		user, err := getUserByName(ctx, repo, args[1])
		if err != nil {
			return err
		}
		if len(args) == 2 {
			if user.Properties.Admin {
				fmt.Printf("%s: all libraries (admin)\n", user.Username)
				return nil
			}
			for _, library := range libraries {
				access := "denied"
				if user.Properties.EnableAllFolders || contains(user.Properties.EnabledFolders, library.ID) {
					access = "allowed"
				}
				fmt.Printf("%-20s %s\n", library.ID, access)
			}
			return nil
		}
		if len(args) != 4 || (args[2] != "grant" && args[2] != "revoke") {
			return errors.New("usage: rcfin user libraries USERNAME [grant|revoke LIBRARY_ID]")
		}
		if user.Properties.Admin {
			return errors.New("admins already have access to all libraries; change role before editing grants")
		}
		libraryID := args[3]
		if !configuredLibrary(libraries, libraryID) {
			return fmt.Errorf("unknown library ID %q; run 'rcfin library list'", libraryID)
		}
		user.Properties.EnableAllFolders = false
		user.Properties.EnabledFolders = removeString(user.Properties.EnabledFolders, libraryID)
		if args[2] == "grant" {
			user.Properties.EnabledFolders = append(user.Properties.EnabledFolders, libraryID)
		}
		if err := repo.UpsertUser(ctx, user); err != nil {
			return fmt.Errorf("update library grants: %w", err)
		}
		verb := "revoked"
		if args[2] == "grant" {
			verb = "granted"
		}
		fmt.Printf("Library %q %s for %q\n", libraryID, verb, user.Username)
		return nil
	default:
		return fmt.Errorf("unknown user command %q", args[0])
	}
}

func promptPassword(label string) (string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", errors.New("password entry requires an interactive terminal")
	}
	fmt.Fprint(os.Stderr, label)
	value, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	return string(value), err
}

func getUserByName(ctx context.Context, repo database.Repository, username string) (*model.User, error) {
	user, err := repo.GetUser(ctx, strings.ToLower(strings.TrimSpace(username)))
	if errors.Is(err, model.ErrNotFound) {
		return nil, fmt.Errorf("user %q not found", username)
	}
	if err != nil {
		return nil, fmt.Errorf("look up user %q: %w", username, err)
	}
	return user, nil
}

func resetUserPassword(ctx context.Context, repo database.Repository, user *model.User) error {
	password, err := promptPassword("New password: ")
	if err != nil {
		return err
	}
	confirmation, err := promptPassword("Confirm password: ")
	if err != nil {
		return err
	}
	if password == "" || password != confirmation {
		return errors.New("passwords must be non-empty and match")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	user.Password = string(hash)
	if err := repo.UpsertUser(ctx, user); err != nil {
		return fmt.Errorf("save password: %w", err)
	}
	return revokeUserTokens(ctx, repo, user.ID)
}

func revokeUserTokens(ctx context.Context, repo database.Repository, userID string) error {
	tokens, err := repo.GetAccessTokens(ctx, userID)
	if err != nil {
		return fmt.Errorf("list user sessions: %w", err)
	}
	for _, token := range tokens {
		if err := repo.DeleteAccessToken(ctx, token.Token); err != nil {
			return fmt.Errorf("revoke user session: %w", err)
		}
	}
	return nil
}

func enabledAdminCount(ctx context.Context, repo database.Repository, excluding string) int {
	users, err := repo.GetAllUsers(ctx)
	if err != nil {
		return 0
	}
	count := 0
	for _, user := range users {
		if user.ID != excluding && user.Properties.Admin && !user.Properties.Disabled {
			count++
		}
	}
	return count
}

func configuredLibrary(libraries []collectionConfig, id string) bool {
	for _, library := range libraries {
		if library.ID == id {
			return true
		}
	}
	return false
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func removeString(values []string, value string) []string {
	result := make([]string, 0, len(values))
	for _, candidate := range values {
		if candidate != value {
			result = append(result, candidate)
		}
	}
	return result
}
