package organization

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/adhikariprivate/rcfin-server/internal/collection"
	"github.com/adhikariprivate/rcfin-server/internal/database/sqlite"
	"github.com/adhikariprivate/rcfin-server/internal/rclonestore"
)

func TestInboxReadinessWaitsForQuietTreeAndResetsOnChange(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	itemRoot := filepath.Join(root, ".Inbox", "Movie (2025)")
	if err := os.MkdirAll(itemRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	mediaPath := filepath.Join(itemRoot, "Movie (2025).mkv")
	if err := os.WriteFile(mediaPath, []byte("uploading"), 0o600); err != nil {
		t.Fatal(err)
	}
	storage, err := rclonestore.New(ctx, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	repo, err := sqlite.New(&sqlite.ConfigFile{Filename: filepath.Join(t.TempDir(), "rcfin.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	coll := &collection.Collection{ID: "movies", Type: collection.CollectionTypeMovies, Storage: storage, InboxPath: ".Inbox"}
	options := Options{StableFor: 50 * time.Millisecond}
	options.Inventory, err = collection.InventoryLibrary(ctx, coll, coll.InboxPath)
	if err != nil {
		t.Fatal(err)
	}
	ready, err := InboxItemReady(ctx, coll, repo, ".Inbox/Movie (2025)", options)
	if err != nil {
		t.Fatal(err)
	}
	if ready {
		t.Fatal("newly observed Inbox tree became ready immediately")
	}
	time.Sleep(60 * time.Millisecond)
	ready, err = InboxItemReady(ctx, coll, repo, ".Inbox/Movie (2025)", options)
	if err != nil || !ready {
		t.Fatalf("unchanged tree did not become ready after stable_for: ready=%v err=%v", ready, err)
	}
	options.StableFor = 100 * time.Millisecond
	ready, err = InboxItemReady(ctx, coll, repo, ".Inbox/Movie (2025)", options)
	if err != nil {
		t.Fatal(err)
	}
	if ready {
		t.Fatal("changing stable_for reused elapsed time from the previous readiness policy")
	}
	if err := os.WriteFile(mediaPath, []byte("upload finished with more bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	storage.FlushDirCache()
	options.Inventory, err = collection.InventoryLibrary(ctx, coll, coll.InboxPath)
	if err != nil {
		t.Fatal(err)
	}
	ready, err = InboxItemReady(ctx, coll, repo, ".Inbox/Movie (2025)", options)
	if err != nil {
		t.Fatal(err)
	}
	if ready {
		t.Fatal("changed Inbox tree did not reset the quiet period")
	}
	time.Sleep(110 * time.Millisecond)
	ready, err = InboxItemReady(ctx, coll, repo, ".Inbox/Movie (2025)", options)
	if err != nil || !ready {
		t.Fatalf("unchanged updated tree did not become ready: ready=%v err=%v", ready, err)
	}
	if err := os.WriteFile(filepath.Join(itemRoot, "Movie.en.srt"), []byte("subtitle companion"), 0o600); err != nil {
		t.Fatal(err)
	}
	storage.FlushDirCache()
	options.Inventory, err = collection.InventoryLibrary(ctx, coll, coll.InboxPath)
	if err != nil {
		t.Fatal(err)
	}
	ready, err = InboxItemReady(ctx, coll, repo, ".Inbox/Movie (2025)", options)
	if err != nil || ready {
		t.Fatalf("late companion file did not reset the full-tree quiet period: ready=%v err=%v", ready, err)
	}
}

func TestInboxCompletionMarkerAndReadinessSurviveRepositoryRestart(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	itemRoot := filepath.Join(root, ".Inbox", "Film (2025)")
	if err := os.MkdirAll(itemRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(itemRoot, "Film.mkv"), []byte("video"), 0o600); err != nil {
		t.Fatal(err)
	}
	storage, err := rclonestore.New(ctx, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	coll := &collection.Collection{ID: "movies", Type: collection.CollectionTypeMovies, Storage: storage, InboxPath: ".Inbox"}
	databasePath := filepath.Join(t.TempDir(), "rcfin.db")
	repo, err := sqlite.New(&sqlite.ConfigFile{Filename: databasePath})
	if err != nil {
		t.Fatal(err)
	}
	options := Options{StableFor: 50 * time.Millisecond}
	options.Inventory, err = collection.InventoryLibrary(ctx, coll, coll.InboxPath)
	if err != nil {
		t.Fatal(err)
	}
	ready, err := InboxItemReady(ctx, coll, repo, ".Inbox/Film (2025)", options)
	if err != nil || ready {
		t.Fatalf("first stability observation = ready %v, err %v; want pending", ready, err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	repo, err = sqlite.New(&sqlite.ConfigFile{Filename: databasePath})
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	storage.FlushDirCache()
	options.Inventory, err = collection.InventoryLibrary(ctx, coll, coll.InboxPath)
	if err != nil {
		t.Fatal(err)
	}
	ready, err = InboxItemReady(ctx, coll, repo, ".Inbox/Film (2025)", options)
	if err != nil || !ready {
		t.Fatalf("persisted quiet period was lost across SQLite restart: ready=%v err=%v", ready, err)
	}
	options.CompletionMarker = ".rcfin-complete"
	ready, err = InboxItemReady(ctx, coll, repo, ".Inbox/Film (2025)", options)
	if err != nil || ready {
		t.Fatalf("missing completion marker = ready %v, err %v; want pending", ready, err)
	}
	if err := os.WriteFile(filepath.Join(itemRoot, ".rcfin-complete"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	storage.FlushDirCache()
	options.Inventory, err = collection.InventoryLibrary(ctx, coll, coll.InboxPath)
	if err != nil {
		t.Fatal(err)
	}
	ready, err = InboxItemReady(ctx, coll, repo, ".Inbox/Film (2025)", options)
	if err != nil || !ready {
		t.Fatalf("completion marker did not release Inbox item: ready=%v err=%v", ready, err)
	}
	options.CompletionMarker = "../unsafe"
	if _, err := InboxItemReady(ctx, coll, repo, ".Inbox/Film (2025)", options); err == nil {
		t.Fatal("unsafe completion marker path was accepted")
	}
}
