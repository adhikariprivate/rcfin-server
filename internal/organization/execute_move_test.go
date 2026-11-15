package organization

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/adhikariprivate/rcfin-server/internal/collection"
	"github.com/adhikariprivate/rcfin-server/internal/database"
	"github.com/adhikariprivate/rcfin-server/internal/database/model"
	"github.com/adhikariprivate/rcfin-server/internal/database/sqlite"
	"github.com/adhikariprivate/rcfin-server/internal/rclonestore"
)

func TestDirectoryMoveJournalCompletesAfterVerifiedBackendMove(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	source := filepath.Join(root, "source", "Movie.mkv")
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("media"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "organized"), 0o755); err != nil {
		t.Fatal(err)
	}
	storage, err := rclonestore.New(ctx, root, map[string]any{"read_only": false})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(storage.Close)
	coll := &collection.Collection{ID: "movies", Type: collection.CollectionTypeMovies, Storage: storage}
	repo, err := sqlite.New(&sqlite.ConfigFile{Filename: filepath.Join(t.TempDir(), "operations.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	now := time.Now().UTC()
	op := &model.MediaOperation{
		ID: "direct-dir-move", CollectionID: coll.ID, ItemKey: "source",
		SourcePath: "source", DestinationPath: "organized/Movie",
		Action: "move_directory", Status: "planned", NextAttempt: now,
		LeaseUntil: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := repo.UpsertMediaOperation(ctx, op); err != nil {
		t.Fatal(err)
	}
	worker := database.NewMediaOperationWorkerID("move-test")
	ctx, keeper, stop := database.StartMediaOperationLeaseKeeper(ctx, repo, worker, time.Minute)
	defer stop()
	if err := runOperation(ctx, coll, repo, worker, op, keeper); err != nil {
		t.Fatalf("runOperation() error = %v", err)
	}
	finished, err := repo.GetMediaOperation(ctx, coll.ID, "source", "move_directory")
	if err != nil {
		t.Fatal(err)
	}
	if finished == nil || finished.Status != "complete" {
		t.Fatalf("journal operation = %#v, want complete", finished)
	}
	if _, err := storage.Stat("source"); !rclonestore.IsNotFound(err) {
		t.Fatalf("source stat error = %v, want source absent", err)
	}
	if info, err := storage.Stat("organized/Movie"); err != nil || !info.IsDir() {
		t.Fatalf("destination = (%v, %v), want existing directory", info, err)
	}
}

func TestFileMoveJournalRecoversDestinationAfterAmbiguousCompletion(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "renamed.mkv"), []byte("media"), 0o600); err != nil {
		t.Fatal(err)
	}
	storage, err := rclonestore.New(ctx, root, map[string]any{"read_only": false})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(storage.Close)
	coll := &collection.Collection{ID: "movies", Type: collection.CollectionTypeMovies, Storage: storage}
	repo, err := sqlite.New(&sqlite.ConfigFile{Filename: filepath.Join(t.TempDir(), "operations.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	checksum, err := storage.ContentChecksum(ctx, "renamed.mkv")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	op := &model.MediaOperation{
		ID: "direct-file-move-recovery", CollectionID: coll.ID, ItemKey: "original.mkv",
		SourcePath: "original.mkv", DestinationPath: "renamed.mkv",
		Action: "move_file:original.mkv", Status: "retryable", Bytes: int64(len("media")),
		Checksum: checksum, Attempts: 1, NextAttempt: now, LeaseUntil: now,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := repo.UpsertMediaOperation(ctx, op); err != nil {
		t.Fatal(err)
	}
	worker := database.NewMediaOperationWorkerID("move-test")
	ctx, keeper, stop := database.StartMediaOperationLeaseKeeper(ctx, repo, worker, time.Minute)
	defer stop()
	if err := runOperation(ctx, coll, repo, worker, op, keeper); err != nil {
		t.Fatalf("runOperation() error = %v", err)
	}
	finished, err := repo.GetMediaOperation(ctx, coll.ID, "original.mkv", "move_file:original.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if finished == nil || finished.Status != "complete" {
		t.Fatalf("journal operation = %#v, want complete", finished)
	}
	if _, err := storage.Stat("original.mkv"); !rclonestore.IsNotFound(err) {
		t.Fatalf("source stat error = %v, want source absent", err)
	}
	if _, err := storage.Stat("renamed.mkv"); err != nil {
		t.Fatalf("destination missing after recovery: %v", err)
	}
}

func TestPartialFileMoveJournalKeepsBothFilesForReview(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "original.mkv"), []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "renamed.mkv"), []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	storage, err := rclonestore.New(ctx, root, map[string]any{"read_only": false})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(storage.Close)
	coll := &collection.Collection{ID: "movies", Type: collection.CollectionTypeMovies, Storage: storage}
	repo, err := sqlite.New(&sqlite.ConfigFile{Filename: filepath.Join(t.TempDir(), "operations.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	checksum, err := storage.ContentChecksum(ctx, "original.mkv")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	op := &model.MediaOperation{
		ID: "partial-file-move-recovery", CollectionID: coll.ID, ItemKey: "original.mkv",
		SourcePath: "original.mkv", DestinationPath: "renamed.mkv",
		Action: "move_file:original.mkv", Status: "retryable", Bytes: int64(len("source")),
		Checksum: checksum, Attempts: 1, NextAttempt: now, LeaseUntil: now,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := repo.UpsertMediaOperation(ctx, op); err != nil {
		t.Fatal(err)
	}
	worker := database.NewMediaOperationWorkerID("move-test")
	ctx, keeper, stop := database.StartMediaOperationLeaseKeeper(ctx, repo, worker, time.Minute)
	defer stop()
	if err := runOperation(ctx, coll, repo, worker, op, keeper); err != nil {
		t.Fatalf("runOperation() error = %v", err)
	}
	finished, err := repo.GetMediaOperation(ctx, coll.ID, "original.mkv", "move_file:original.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if finished == nil || finished.Status != "needs_review" {
		t.Fatalf("journal operation = %#v, want needs_review", finished)
	}
	for _, file := range []string{"original.mkv", "renamed.mkv"} {
		if _, err := storage.Stat(file); err != nil {
			t.Fatalf("%q was changed during ambiguous recovery: %v", file, err)
		}
	}
}

func TestExplicitExistingItemStagingUsesDirectoryMoveJournal(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".Inbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "Existing Movie"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Existing Movie", "Movie.mkv"), []byte("media"), 0o600); err != nil {
		t.Fatal(err)
	}
	storage, err := rclonestore.New(ctx, root, map[string]any{"read_only": false})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(storage.Close)
	coll := &collection.Collection{ID: "movies", Type: collection.CollectionTypeMovies, Storage: storage, InboxPath: ".Inbox"}
	repo, err := sqlite.New(&sqlite.ConfigFile{Filename: filepath.Join(t.TempDir(), "operations.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	op, err := QueueExistingItemForInbox(ctx, coll, repo, "Existing Movie")
	if err != nil {
		t.Fatalf("QueueExistingItemForInbox() error = %v", err)
	}
	if op.DestinationPath != filepath.ToSlash(filepath.Join(".Inbox", "Existing Movie")) {
		t.Fatalf("staging destination = %q", op.DestinationPath)
	}
	if _, err := ProcessPendingInboxStaging(ctx, coll, repo, database.NewMediaOperationWorkerID("move-test")); err != nil {
		t.Fatalf("ProcessPendingInboxStaging() error = %v", err)
	}
	finished, err := repo.GetMediaOperation(ctx, coll.ID, "Existing Movie", "stage_existing")
	if err != nil {
		t.Fatal(err)
	}
	if finished == nil || finished.Status != "complete" {
		t.Fatalf("staging journal operation = %#v, want complete", finished)
	}
	if _, err := storage.Stat("Existing Movie"); !rclonestore.IsNotFound(err) {
		t.Fatalf("existing source stat error = %v, want source absent", err)
	}
	if info, err := storage.Stat(".Inbox/Existing Movie"); err != nil || !info.IsDir() {
		t.Fatalf("staged destination = (%v, %v), want existing directory", info, err)
	}
}
