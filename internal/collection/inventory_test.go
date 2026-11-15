package collection

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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
