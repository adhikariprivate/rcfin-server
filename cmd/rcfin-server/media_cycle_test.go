package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adhikariprivate/rcfin-server/internal/collection"
	"github.com/adhikariprivate/rcfin-server/internal/database/model"
	"github.com/adhikariprivate/rcfin-server/internal/database/sqlite"
)

func TestConfirmedFolderMoveRelinksCompletedMetadataOperation(t *testing.T) {
	repo, err := sqlite.New(&sqlite.ConfigFile{Filename: filepath.Join(t.TempDir(), "media.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	ctx := context.Background()
	now := time.Now().UTC()
	oldPath, newPath := "Old/Movie.mkv", "Renamed/Movie.mkv"
	operation := &model.MediaOperation{ID: mediaOperationID("movies", oldPath, "metadata_refresh"), CollectionID: "movies", ItemKey: oldPath, SourcePath: oldPath, DestinationPath: "Old/movie.nfo", Action: "metadata_refresh", Status: "complete", Evidence: `{"signature":"old","config_version":1,"configuration":{}}`, CreatedAt: now, UpdatedAt: now, NextAttempt: now, LeaseUntil: now}
	if err := repo.UpsertMediaOperation(ctx, operation); err != nil {
		t.Fatal(err)
	}
	stableIDOperation := &model.MediaOperation{ID: mediaOperationID("movies", "Inbox/Movie", "move_directory"), CollectionID: "movies", ItemKey: "Inbox/Movie", SourcePath: "Inbox/Movie", DestinationPath: "Old", Action: "move_directory", Status: "complete", Evidence: `{"stable_item_id":"jellyfin-stable-id"}`, CreatedAt: now, UpdatedAt: now, NextAttempt: now, LeaseUntil: now}
	if err := repo.UpsertMediaOperation(ctx, stableIDOperation); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveLibraryInventory(ctx, &model.LibraryInventorySnapshot{CollectionID: "movies", CompletedAt: now, Entries: []model.LibraryInventoryEntry{{Path: "Renamed", MoveRelinkPending: true}, {Path: newPath, MoveRelinkPending: true}}}); err != nil {
		t.Fatal(err)
	}
	changes := []collection.InventoryChange{{Kind: collection.InventoryMoved, OldPath: "Old", Path: "Renamed", Entry: collection.InventoryEntry{Path: "Renamed", IsDir: true}}, {Kind: collection.InventoryMoved, OldPath: oldPath, Path: newPath, Entry: collection.InventoryEntry{Path: newPath, Role: collection.InventoryVideo, Size: 10, ModTime: 20}}}
	inventory := []collection.InventoryEntry{{Path: "Renamed", Scope: collection.InventoryScopeLibrary, IsDir: true}, {Path: newPath, Scope: collection.InventoryScopeLibrary, Role: collection.InventoryVideo, Size: 10, ModTime: 20}}
	if err := relinkMovedMediaOperations(ctx, repo, "movies", collection.CollectionTypeMovies, changes, inventory); err != nil {
		t.Fatal(err)
	}
	relinked, err := repo.GetMediaOperation(ctx, "movies", newPath, "metadata_refresh")
	if err != nil {
		t.Fatal(err)
	}
	if relinked == nil || relinked.Status != "complete" || relinked.SourcePath != newPath || relinked.DestinationPath != "Renamed/movie.nfo" {
		t.Fatalf("move did not preserve and rebase metadata decision: %#v", relinked)
	}
	if relinked.ID != mediaOperationID("movies", newPath, "metadata_refresh") {
		t.Fatalf("operation id did not follow the new idempotency key: %q", relinked.ID)
	}
	stableRelinked, err := repo.GetMediaOperation(ctx, "movies", "Inbox/Movie", "move_directory")
	if err != nil {
		t.Fatal(err)
	}
	if stableRelinked == nil || stableRelinked.DestinationPath != "Renamed" || !strings.Contains(stableRelinked.Evidence, "jellyfin-stable-id") {
		t.Fatalf("stable Jellyfin item ID journal did not follow its confirmed folder rename: %#v", stableRelinked)
	}
	snapshot, err := repo.GetLibraryInventory(ctx, "movies")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range snapshot.Entries {
		if entry.MoveRelinkPending {
			t.Fatalf("move relinking was not durably acknowledged for %q", entry.Path)
		}
	}
}

func TestDeferredProviderCredentialsKeepNewMediaQueued(t *testing.T) {
	repo, err := sqlite.New(&sqlite.ConfigFile{Filename: filepath.Join(t.TempDir(), "media.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	ctx := context.Background()
	paths := []string{"Show/S01/Show S01E01.mkv", "Show/S01/Show S01E02.mkv"}
	inventory := []collection.InventoryEntry{
		{Path: paths[0], Scope: collection.InventoryScopeLibrary, Role: collection.InventoryVideo},
		{Path: paths[1], Scope: collection.InventoryScopeLibrary, Role: collection.InventoryVideo},
	}
	retryAt := time.Now().Add(10 * time.Minute)
	if err := queueDeferredMetadataOperations(ctx, repo, "shows", inventory, collection.CollectionTypeShows, paths, retryAt, "TMDb credentials are unavailable"); err != nil {
		t.Fatal(err)
	}
	metadataFirst, err := repo.GetMediaOperation(ctx, "shows", paths[0], "metadata_refresh")
	if err != nil {
		t.Fatal(err)
	}
	metadataSecond, err := repo.GetMediaOperation(ctx, "shows", paths[1], "metadata_refresh")
	if err != nil {
		t.Fatal(err)
	}
	if metadataFirst == nil || metadataFirst.Status != "failed_retryable" || metadataSecond != nil {
		t.Fatalf("show metadata work should have one durable retry row: first=%#v second=%#v", metadataFirst, metadataSecond)
	}
	if err := queueDeferredSubtitleOperations(ctx, repo, "shows", paths, []string{"en"}, retryAt, "OpenSubtitles credentials are unavailable"); err != nil {
		t.Fatal(err)
	}
	for _, itemPath := range paths {
		operation, err := repo.GetMediaOperation(ctx, "shows", itemPath, "subtitle_refresh:en")
		if err != nil {
			t.Fatal(err)
		}
		if operation == nil || operation.Status != "failed_retryable" || operation.Error == "" {
			t.Fatalf("subtitle retry missing for %q: %#v", itemPath, operation)
		}
	}
}

func TestStageWorkPathsOnlyExpandsForChangedStageAlgorithm(t *testing.T) {
	inventory := []collection.InventoryEntry{
		{Path: "Film/one.mkv", Scope: collection.InventoryScopeLibrary, Role: collection.InventoryVideo},
		{Path: "Film/two.mkv", Scope: collection.InventoryScopeLibrary, Role: collection.InventoryVideo},
	}
	metadataPaths := stageWorkPaths(nil, nil, inventory, collection.InventoryScopeLibrary, true)
	subtitlePaths := stageWorkPaths(nil, nil, inventory, collection.InventoryScopeLibrary, false)
	if len(metadataPaths) != 2 {
		t.Fatalf("metadata algorithm change selected %v, want both library videos", metadataPaths)
	}
	if len(subtitlePaths) != 0 {
		t.Fatalf("unchanged subtitle algorithm selected %v, want no work", subtitlePaths)
	}
}

func TestPendingMediaPathsAreStageScoped(t *testing.T) {
	repo, err := sqlite.New(&sqlite.ConfigFile{Filename: filepath.Join(t.TempDir(), "media.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	now := time.Now().UTC()
	for _, operation := range []*model.MediaOperation{
		{ID: mediaOperationID("movies", "Film/one.mkv", "metadata_refresh"), CollectionID: "movies", ItemKey: "Film/one.mkv", Action: "metadata_refresh", Status: "planned", CreatedAt: now, UpdatedAt: now},
		{ID: mediaOperationID("movies", "Film/two.mkv", "subtitle_refresh:en"), CollectionID: "movies", ItemKey: "Film/two.mkv", Action: "subtitle_refresh:en", Status: "planned", CreatedAt: now, UpdatedAt: now},
	} {
		if err := repo.UpsertMediaOperation(context.Background(), operation); err != nil {
			t.Fatal(err)
		}
	}
	inventory := []collection.InventoryEntry{
		{Path: "Film/one.mkv", Scope: collection.InventoryScopeLibrary, Role: collection.InventoryVideo},
		{Path: "Film/two.mkv", Scope: collection.InventoryScopeLibrary, Role: collection.InventoryVideo},
	}
	metadataPaths, err := pendingMediaPaths(context.Background(), repo, "movies", inventory, collection.InventoryScopeLibrary, "metadata")
	if err != nil {
		t.Fatal(err)
	}
	subtitlePaths, err := pendingMediaPaths(context.Background(), repo, "movies", inventory, collection.InventoryScopeLibrary, "subtitles")
	if err != nil {
		t.Fatal(err)
	}
	if len(metadataPaths) != 1 || metadataPaths[0] != "Film/one.mkv" {
		t.Fatalf("pending metadata paths = %v", metadataPaths)
	}
	if len(subtitlePaths) != 1 || subtitlePaths[0] != "Film/two.mkv" {
		t.Fatalf("pending subtitle paths = %v", subtitlePaths)
	}
}

func TestLibraryRetryFindsAndRequeuesOperationByJobID(t *testing.T) {
	repo, err := sqlite.New(&sqlite.ConfigFile{Filename: filepath.Join(t.TempDir(), "media.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	now := time.Now().UTC()
	operation := &model.MediaOperation{ID: "job-123", CollectionID: "movies", ItemKey: "Film/Movie.mkv", SourcePath: "Film/Movie.mkv", Action: "metadata_refresh", Status: "failed_retryable", Error: "temporary provider failure", CreatedAt: now, UpdatedAt: now, NextAttempt: now.Add(time.Hour), LeaseUntil: now}
	if err := repo.UpsertMediaOperation(context.Background(), operation); err != nil {
		t.Fatal(err)
	}
	if err := runLibraryRetryCommand(context.Background(), repo, []collectionConfig{{ID: "movies"}}, []string{"movies", "--job", "job-123"}); err != nil {
		t.Fatal(err)
	}
	retried, err := repo.GetMediaOperation(context.Background(), "movies", "Film/Movie.mkv", "metadata_refresh")
	if err != nil {
		t.Fatal(err)
	}
	if retried == nil || retried.Status != "planned" || retried.Error != "" {
		t.Fatalf("job retry was not queued: %#v", retried)
	}
}
