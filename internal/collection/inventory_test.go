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
