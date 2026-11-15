package collection

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/adhikariprivate/rcfin-server/internal/database/model"
	"github.com/adhikariprivate/rcfin-server/internal/database/sqlite"
	"github.com/adhikariprivate/rcfin-server/internal/rclonestore"
)

func TestInventoryMovieReadsFolderMovieNFOProviderIDs(t *testing.T) {
	root := t.TempDir()
	movieDir := filepath.Join(root, "Musk (2026)")
	if err := os.MkdirAll(movieDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		"Musk (2026).mp4": "video fixture",
		"movie.nfo":       `<movie><uniqueid type="tmdb" default="true">1096228</uniqueid></movie>`,
	} {
		if err := os.WriteFile(filepath.Join(movieDir, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	storage, err := rclonestore.New(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()

	entries, err := InventoryCollection(context.Background(), &Collection{
		ID: "movies", Type: CollectionTypeMovies, Storage: storage,
	}, "", false)
	if err != nil {
		t.Fatalf("InventoryCollection() error = %v", err)
	}
	for _, entry := range entries {
		if entry.Role != InventoryVideo {
			continue
		}
		if got := entry.ProviderIDs["tmdb"]; got != "1096228" {
			t.Fatalf("movie inventory tmdb ID = %q, want 1096228; entry=%#v", got, entry)
		}
		return
	}
	t.Fatal("movie video was absent from inventory")
}

func TestOpenInventoryDirBuildsSortedSnapshotWithoutStorage(t *testing.T) {
	modTime := time.Date(2025, 4, 3, 2, 1, 0, 0, time.UTC)
	inventory := []InventoryEntry{
		{Path: "Show/Season 01/Episode 01.mkv", Size: 123, ModTime: modTime.UnixNano(), Role: InventoryVideo},
		{Path: "Show/tvshow.nfo", Size: 42, ModTime: modTime.UnixNano(), Role: InventoryNFO},
	}

	dir := OpenInventoryDir("Show", inventory)
	entries, err := dir.Readdir(0)
	if err != nil {
		t.Fatalf("Readdir() error = %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("snapshot children = %d, want 2", len(entries))
	}
	if entries[0].Name() != "Season 01" || !entries[0].IsDir() {
		t.Fatalf("first child = %q (directory %v), want Season 01 directory", entries[0].Name(), entries[0].IsDir())
	}
	if entries[1].Name() != "tvshow.nfo" || entries[1].IsDir() || entries[1].Size() != 42 {
		t.Fatalf("second child = %q (directory %v, size %d), want tvshow.nfo file size 42", entries[1].Name(), entries[1].IsDir(), entries[1].Size())
	}

	season, err := OpenInventoryDir("Show/Season 01", inventory).Readdir(0)
	if err != nil {
		t.Fatalf("season Readdir() error = %v", err)
	}
	if len(season) != 1 || season[0].Name() != "Episode 01.mkv" || season[0].Size() != 123 || !season[0].Modtime().Equal(modTime) {
		t.Fatalf("season snapshot entry = %#v, want episode metadata from inventory", season)
	}
}

func TestInventoryLibraryCoversLibraryAndInboxWithScope(t *testing.T) {
	root := t.TempDir()
	for _, relative := range []string{"Film/movie.mkv", ".Inbox/New/movie.mkv"} {
		filename := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte("video"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	storage, err := rclonestore.New(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	inventory, err := InventoryLibrary(context.Background(), &Collection{ID: "movies", Type: CollectionTypeMovies, Storage: storage, InboxPath: ".Inbox"}, ".Inbox")
	if err != nil {
		t.Fatalf("InventoryLibrary() error = %v", err)
	}
	seen := map[string]string{}
	for _, entry := range inventory {
		if entry.Role == InventoryVideo {
			seen[entry.Path] = entry.Scope
		}
	}
	if seen["Film/movie.mkv"] != InventoryScopeLibrary || seen[".Inbox/New/movie.mkv"] != InventoryScopeInbox {
		t.Fatalf("video scopes = %#v", seen)
	}
	if len(inventory) < 4 {
		t.Fatalf("unified inventory omitted directory records: %#v", inventory)
	}
}

func TestCollectionItemIDSurvivesConfirmedFolderRename(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	movieDir := filepath.Join(root, "Film (2025)")
	if err := os.MkdirAll(movieDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(movieDir, "Film (2025).mkv"), []byte("video fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo, err := sqlite.New(&sqlite.ConfigFile{Filename: filepath.Join(t.TempDir(), "inventory.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	storage, err := rclonestore.New(ctx, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	collections := New(&Options{Repo: repo})
	collections.AddCollectionWithInbox("Movies", "movies", "movies", root, storage, ".Inbox")
	coll := collections.GetCollection("movies")
	firstInventory, err := InventoryLibrary(ctx, coll, coll.InboxPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReconcileLibraryInventory(ctx, repo, "movies", firstInventory, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := collections.RefreshCollectionWithInventory(ctx, "movies", firstInventory); err != nil {
		t.Fatal(err)
	}
	if len(coll.Items) != 1 {
		t.Fatalf("initial movie model count = %d, want 1", len(coll.Items))
	}
	firstID := coll.Items[0].ID()
	snapshot, err := repo.GetLibraryInventory(ctx, "movies")
	if err != nil {
		t.Fatal(err)
	}
	if snapshotEntryByPath(snapshot, "Film (2025)").JellyfinID != firstID {
		t.Fatal("initial Jellyfin ID was not saved in the inventory catalog")
	}

	if err := os.Rename(movieDir, filepath.Join(root, "Renamed Film (2025)")); err != nil {
		t.Fatal(err)
	}
	storage.FlushDirCache()
	secondInventory, err := InventoryLibrary(ctx, coll, coll.InboxPath)
	if err != nil {
		t.Fatal(err)
	}
	reconciliation, err := ReconcileLibraryInventory(ctx, repo, "movies", secondInventory, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	moved := false
	for _, change := range reconciliation.Changes {
		if change.Kind == InventoryMoved && change.OldPath == "Film (2025)" && change.Path == "Renamed Film (2025)" {
			moved = true
		}
	}
	if !moved {
		t.Fatalf("folder rename was not recognized as a move: %#v", reconciliation.Changes)
	}
	if err := collections.RefreshCollectionWithInventory(ctx, "movies", secondInventory); err != nil {
		t.Fatal(err)
	}
	if len(coll.Items) != 1 || coll.Items[0].ID() != firstID {
		t.Fatalf("Jellyfin item ID after folder rename = %v, want %q", coll.Items, firstID)
	}
	snapshot, err = repo.GetLibraryInventory(ctx, "movies")
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshotEntryByPath(snapshot, "Renamed Film (2025)").JellyfinID; got != firstID {
		t.Fatalf("catalog Jellyfin ID after move = %q, want %q", got, firstID)
	}
}

func TestRefreshCollectionWithInventoryPersistsNewSidecarsWithoutRelisting(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	movieDir := filepath.Join(root, "Film")
	if err := os.MkdirAll(movieDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(movieDir, "Film.mkv"), []byte("video fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo, err := sqlite.New(&sqlite.ConfigFile{Filename: filepath.Join(t.TempDir(), "inventory.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	storage, err := rclonestore.New(ctx, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	collections := New(&Options{Repo: repo})
	collections.AddCollectionWithInbox("Movies", "movies", "movies", root, storage, ".Inbox")
	coll := collections.GetCollection("movies")
	inventory, err := InventoryLibrary(ctx, coll, coll.InboxPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReconcileLibraryInventory(ctx, repo, "movies", inventory, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := collections.RefreshCollectionWithInventory(ctx, "movies", inventory); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(movieDir, "movie.nfo"), []byte(`<movie><uniqueid type="tmdb" default="true">12345</uniqueid></movie>`), 0o644); err != nil {
		t.Fatal(err)
	}
	storage.FlushDirCache()
	info, err := storage.Stat("Film/movie.nfo")
	if err != nil {
		t.Fatal(err)
	}
	inventory = append(inventory, InventoryEntry{Path: "Film/movie.nfo", Scope: InventoryScopeLibrary, Role: InventoryNFO, Size: info.Size(), ModTime: info.ModTime().UnixNano()})
	if err := collections.RefreshCollectionWithInventory(ctx, "movies", inventory); err != nil {
		t.Fatal(err)
	}
	if len(coll.Items) != 1 || coll.Items[0].(*Movie).Metadata.ProviderIDs()["tmdb"] != "12345" {
		t.Fatalf("new NFO was not loaded into the current model: %#v", coll.Items)
	}
	snapshot, err := repo.GetLibraryInventory(ctx, "movies")
	if err != nil {
		t.Fatal(err)
	}
	if entry := snapshotEntryByPath(snapshot, "Film/movie.nfo"); entry.Path == "" || entry.Role != string(InventoryNFO) {
		t.Fatalf("new sidecar was not added to the durable snapshot: %#v", snapshot.Entries)
	}
	if snapshotEntryByPath(snapshot, "Film").TreeFingerprint == "" {
		t.Fatal("parent subtree fingerprint was not updated after sidecar write")
	}
}

func snapshotEntryByPath(snapshot *model.LibraryInventorySnapshot, itemPath string) model.LibraryInventoryEntry {
	if snapshot != nil {
		for _, entry := range snapshot.Entries {
			if entry.Path == itemPath {
				return entry
			}
		}
	}
	return model.LibraryInventoryEntry{}
}
