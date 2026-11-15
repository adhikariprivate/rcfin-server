package organization

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/adhikariprivate/rcfin-server/internal/collection"
	"github.com/adhikariprivate/rcfin-server/internal/metadata/provider"
	"github.com/adhikariprivate/rcfin-server/internal/rclonestore"
)

type testTVProvider struct{}

func (testTVProvider) Search(context.Context, provider.MediaType, string, int) ([]provider.Candidate, error) {
	return []provider.Candidate{{ID: "1396", Title: "Breaking Bad", Year: 2008, Type: provider.Show}}, nil
}

func (testTVProvider) Get(_ context.Context, kind provider.MediaType, id, _ string) (provider.Metadata, error) {
	return provider.Metadata{Provider: "tmdb", Type: kind, ID: id, Title: "Breaking Bad", Year: 2008}, nil
}

func (testTVProvider) FindExternalID(context.Context, string, string, provider.MediaType, string) (provider.Metadata, error) {
	return provider.Metadata{}, nil
}

func (testTVProvider) GetSeasonEpisodes(context.Context, string, int, string) ([]provider.EpisodeMetadata, error) {
	return []provider.EpisodeMetadata{{SeasonNumber: 1, EpisodeNumber: 1, Name: "Pilot"}}, nil
}

func TestPlanInboxShowsDoesNotTreatEpisodeVideoAsSidecar(t *testing.T) {
	root := t.TempDir()
	seasonDir := filepath.Join(root, ".Inbox", "Breaking Bad (2008)", "Season 01")
	if err := os.MkdirAll(seasonDir, 0o755); err != nil {
		t.Fatal(err)
	}
	video := "Breaking Bad S01E01 - Pilot.mkv"
	for _, name := range []string{video, "Breaking Bad S01E01 - Pilot.en.srt"} {
		if err := os.WriteFile(filepath.Join(seasonDir, name), []byte("fixture"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	storage, err := rclonestore.New(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	coll := &collection.Collection{ID: "shows", Type: collection.CollectionTypeShows, Storage: storage, InboxPath: ".Inbox"}
	report, err := PlanInboxShows(context.Background(), coll, testTVProvider{}, Options{
		FolderFormat:      "{title} ({year}) [tmdbid-{tmdb_id}]",
		SeasonFormat:      "Season {season}",
		EpisodeFormat:     "{series} - {episode} - {episode_title}.{ext}",
		MinimumConfidence: 0.5,
	})
	if err != nil {
		t.Fatalf("PlanInboxShows() error = %v", err)
	}
	if len(report.Items) != 1 || report.Items[0].State != "planned" {
		t.Fatalf("plan items = %#v, want one planned show", report.Items)
	}
	var videoMoves, subtitleMoves int
	for _, file := range report.Items[0].Files {
		if file.Action != "move_file" {
			continue
		}
		switch file.Role {
		case collection.InventoryVideo:
			videoMoves++
		case collection.InventorySubtitle:
			subtitleMoves++
		}
	}
	if videoMoves != 1 || subtitleMoves != 1 {
		t.Fatalf("file moves: video=%d subtitles=%d files=%#v, want one of each", videoMoves, subtitleMoves, report.Items[0].Files)
	}
	if _, err := storage.Stat(filepath.ToSlash(filepath.Join(".Inbox", "Breaking Bad (2008)", "Season 01", video))); err != nil {
		t.Fatalf("dry-run changed the source video: %v", err)
	}
}
