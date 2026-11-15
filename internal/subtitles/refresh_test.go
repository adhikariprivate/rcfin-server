package subtitles

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/adhikariprivate/rcfin-server/internal/collection"
	"github.com/adhikariprivate/rcfin-server/internal/rclonestore"
)

type rewriteTransport struct{ target string }

func (t rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base, err := url.Parse(t.target)
	if err != nil {
		return nil, err
	}
	target := *req.URL
	target.Scheme, target.Host = base.Scheme, base.Host
	cloned := req.Clone(req.Context())
	cloned.URL, cloned.Host, cloned.RequestURI = &target, base.Host, ""
	return http.DefaultTransport.RoundTrip(cloned)
}

func TestRefreshConfirmsUnknownSearchFormatFromDownloadedFilename(t *testing.T) {
	root := t.TempDir()
	movieDir := filepath.Join(root, "The Matrix (1999)")
	if err := os.MkdirAll(movieDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(movieDir, "The Matrix (1999).mkv"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	searchCalls, downloadCalls := 0, 0
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/subtitles", func(w http.ResponseWriter, _ *http.Request) {
		searchCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"candidate-1","attributes":{"language":"en","hearing_impaired":false,"foreign_parts_only":false,"files":[{"file_id":7,"file_name":"Candidate.en"}],"feature_details":{"title":"The Matrix","year":1999,"tmdb_id":603,"imdb_id":133093}}}]}`)
	})
	mux.HandleFunc("/api/v1/download", func(w http.ResponseWriter, _ *http.Request) {
		downloadCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"link":"https://download.local/subtitle.srt","file_name":"The.Matrix.1999.en.srt","remaining":99}`)
	})
	mux.HandleFunc("/subtitle.srt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "1\n00:00:01,000 --> 00:00:02,000\nverified cue\n")
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	storage, err := rclonestore.New(context.Background(), root, map[string]any{"read_only": false})
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	client, err := NewOpenSubtitles("test-api-key", "", "", &http.Client{Transport: rewriteTransport{target: server.URL}})
	if err != nil {
		t.Fatal(err)
	}
	coll := &collection.Collection{ID: "movies", Type: collection.CollectionTypeMovies, Storage: storage}
	report, err := Refresh(context.Background(), coll, client, RefreshOptions{
		Languages: []string{"en"}, Formats: []string{"srt"}, MinimumConfidence: 0.5, Write: true,
	})
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if len(report.Actions) != 1 {
		t.Fatalf("actions = %d, want one: %#v", len(report.Actions), report.Actions)
	}
	action := report.Actions[0]
	if action.State != "written" {
		t.Fatalf("action state = %q, reason = %q", action.State, action.Reason)
	}
	if filepath.Ext(action.Destination) != ".srt" {
		t.Fatalf("destination = %q, want downloaded SRT extension", action.Destination)
	}
	data, err := storage.ReadFile(action.Destination)
	if err != nil {
		t.Fatalf("read written subtitle: %v", err)
	}
	if string(data) != "1\n00:00:01,000 --> 00:00:02,000\nverified cue\n" {
		t.Fatalf("written subtitle contents = %q", data)
	}
	if searchCalls != 1 || downloadCalls != 1 {
		t.Fatalf("provider calls: search=%d download=%d, want one each", searchCalls, downloadCalls)
	}
}

func TestOpenSubtitlesSearchAcceptsNumericProviderIDs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data":[{"id":"candidate-2","attributes":{"language":"en","format":"srt","files":[{"file_id":8,"file_name":"sample.srt"}],"feature_details":{"title":"Sample","year":2020,"tmdb_id":123,"imdb_id":456}}}]}`)
	}))
	defer server.Close()
	client, err := NewOpenSubtitles("test-api-key", "", "", &http.Client{Transport: rewriteTransport{target: server.URL}})
	if err != nil {
		t.Fatal(err)
	}
	client.retryLimit = 1
	rows, err := client.Search(context.Background(), SearchQuery{Title: "Sample", Year: 2020, Language: "en", MediaType: "movie"})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(rows) != 1 || rows[0].TMDbID != "123" || rows[0].IMDbID != "456" {
		t.Fatalf("parsed candidates = %#v", rows)
	}
}

func TestSubtitleJobSignatureChangesWhenProviderIdentityChanges(t *testing.T) {
	video := collection.InventoryEntry{
		Path: "Musk (2026)/Musk (2026).mp4", Size: 1234, ModTime: 5678,
		Title: "Musk", Year: 2026,
	}
	opts := RefreshOptions{Languages: []string{"en"}, Formats: []string{"srt"}, MinimumConfidence: 0.84}
	withoutID := subtitleJobSignature(video, "en", opts)
	video.ProviderIDs = map[string]string{"tmdb": "1096228"}
	withID := subtitleJobSignature(video, "en", opts)
	if withoutID == withID {
		t.Fatal("subtitle job signature did not change when movie.nfo supplied a TMDb ID")
	}
}

func TestRankSubtitleScoresMatchingProviderIDWithoutHash(t *testing.T) {
	q := queryContext{
		SearchQuery: SearchQuery{Title: "Musk", Year: 2026, Language: "en"},
		Title:       "Musk", Year: 2026, TMDbID: "1096228",
	}
	candidate := Candidate{
		ID: "candidate", Language: "en", Title: "Musk", Year: 2026,
		TMDbID: "1096228", Format: "srt", MovieHashMatch: false,
	}
	selected, confidence, reason, ok := rankSubtitle(q, []Candidate{candidate}, RefreshOptions{
		Formats: []string{"srt"}, MinimumConfidence: 0.84,
	})
	if !ok {
		t.Fatalf("rankSubtitle() rejected provider-ID match: confidence=%.2f reason=%q", confidence, reason)
	}
	if selected.ID != candidate.ID || confidence < 0.95 {
		t.Fatalf("selected=%q confidence=%.2f, want candidate with >=0.95 confidence", selected.ID, confidence)
	}
}
