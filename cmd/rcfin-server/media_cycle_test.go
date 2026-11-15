package main

import (
	"context"
	"path/filepath"
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
	if err := repo.SaveLibraryInventory(ctx, &model.LibraryInventorySnapshot{CollectionID: "movies", CompletedAt: now, Entries: []model.LibraryInventoryEntry{{Path: "Renamed", MoveRelinkPending: true}, {Path: newPath, MoveRelinkPending: true}}}); err != nil {
		t.Fatal(err)
	}
	changes := []collection.InventoryChange{{Kind: collection.InventoryMoved, OldPath: "Old", Path: "Renamed"}, {Kind: collection.InventoryMoved, OldPath: oldPath, Path: newPath}}
	inventory := []collection.InventoryEntry{{Path: newPath, Scope: collection.InventoryScopeLibrary, Role: collection.InventoryVideo, Size: 10, ModTime: 20}}
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
