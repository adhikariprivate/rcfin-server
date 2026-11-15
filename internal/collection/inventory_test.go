package collection

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

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
