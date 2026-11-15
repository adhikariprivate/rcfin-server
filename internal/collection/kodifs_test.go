package collection

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/adhikariprivate/rcfin-server/internal/database/sqlite"
	"github.com/adhikariprivate/rcfin-server/internal/rclonestore"
)

func TestYearFromNameSupportsProviderIDSuffix(t *testing.T) {
	for _, test := range []struct {
		name string
		want int
	}{
		{name: "The Mitchells vs. the Machines (2021)", want: 2021},
		{name: "The Mitchells vs. the Machines (2021) [tmdbid-501929]", want: 2021},
		{name: "Show (2024) [tmdbid-123] [edition-cut]", want: 2024},
		{name: "Unknown Title", want: 0},
	} {
		if got := yearFromName(test.name); got != test.want {
			t.Errorf("yearFromName(%q) = %d, want %d", test.name, got, test.want)
		}
	}
}

func TestMovieWithProviderSuffixUsesFolderYearInsteadOfCurrentYear(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(&sqlite.ConfigFile{Filename: filepath.Join(t.TempDir(), "catalog.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	root := t.TempDir()
	movieDir := filepath.Join(root, "The Mitchells vs. the Machines (2021) [tmdbid-501929]")
	if err := os.MkdirAll(movieDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(movieDir, "The Mitchells vs. the Machines [1080p].mp4"), []byte("video"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(movieDir, "movie.nfo"), []byte(`<movie><title>The Mitchells vs. the Machines</title><year>2021</year><uniqueid type="tmdb">501929</uniqueid></movie>`), 0644); err != nil {
		t.Fatal(err)
	}
	storage, err := rclonestore.New(ctx, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	collections := New(&Options{Repo: db})
	collections.AddCollection("Movies", "movies", string(CollectionTypeMovies), root, storage)
	if err := collections.InitContext(ctx); err != nil {
		t.Fatal(err)
	}
	defer collections.Close()
	items := collections.GetCollection("movies").Items
	if len(items) != 1 {
		t.Fatalf("movie count = %d, want 1", len(items))
	}
	if got := items[0].Year(); got != 2021 {
		t.Fatalf("movie year = %d, want 2021", got)
	}
}
