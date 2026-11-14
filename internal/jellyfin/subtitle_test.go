package jellyfin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gorilla/mux"

	"github.com/adhikariprivate/rcfin-server/internal/collection"
	"github.com/adhikariprivate/rcfin-server/internal/database/model"
	"github.com/adhikariprivate/rcfin-server/internal/database/sqlite"
	"github.com/adhikariprivate/rcfin-server/internal/rclonestore"
)

func TestSRTToVTT(t *testing.T) {
	got := string(srtToVTT([]byte("\xef\xbb\xbf1\r\n00:00:01,250 --> 00:00:02,500\r\nHello\r\n")))
	if !strings.HasPrefix(got, "WEBVTT\n\n1\r\n00:00:01.250 --> 00:00:02.500") {
		t.Fatalf("unexpected VTT output: %q", got)
	}
}

func TestSubtitleStreamsAndAuthorizedDelivery(t *testing.T) {
	root := t.TempDir()
	movieDir := filepath.Join(root, "Film (2020)")
	seasonDir := filepath.Join(root, "The Fixers", "S01")
	for _, dir := range []string{movieDir, seasonDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		filepath.Join(movieDir, "Film.mp4"):                          "video",
		filepath.Join(movieDir, "Film.en.default.srt"):               "1\n00:00:01,000 --> 00:00:02,000\nMovie cue\n",
		filepath.Join(movieDir, "Film.fr.vtt"):                       "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nFrench cue\n",
		filepath.Join(seasonDir, "The Fixers S01E01 - Pilot.mp4"):    "video",
		filepath.Join(seasonDir, "The Fixers S01E01 - Pilot.en.srt"): "1\n00:00:03,000 --> 00:00:04,000\nEpisode cue\n",
	}
	for name, data := range files {
		if err := os.WriteFile(name, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	store, err := rclonestore.New(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sqlite.New(&sqlite.ConfigFile{Filename: filepath.Join(t.TempDir(), "test.db")})
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	defer db.Close()
	collections := collection.New(&collection.Options{Collections: []collection.Collection{
		{ID: "movies", Name: "Movies", Type: collection.CollectionTypeMovies, Storage: store},
		{ID: "shows", Name: "TV", Type: collection.CollectionTypeShows, Storage: store},
	}, Repo: db})
	defer collections.Close()
	collections.Init()
	j := New(&Options{Collections: collections, Repo: db})
	router := mux.NewRouter()
	j.RegisterHandlers(router)
	for _, route := range []string{
		"/Videos/test/test/Subtitles/2/Stream.vtt",
		"/Videos/test/test/Subtitles/2/0/Stream.vtt",
	} {
		match := &mux.RouteMatch{}
		if !router.Match(httptest.NewRequest(http.MethodGet, route, nil), match) {
			t.Errorf("registered subtitle route did not match %q", route)
		}
	}

	var movie, episode collection.Item
	for _, c := range collections.GetCollections() {
		for _, item := range c.Items {
			if m, ok := item.(*collection.Movie); ok {
				movie = m
			}
			if show, ok := item.(*collection.Show); ok && len(show.Seasons) > 0 && len(show.Seasons[0].Episodes) > 0 {
				episode = &show.Seasons[0].Episodes[0]
			}
		}
	}
	if movie == nil || episode == nil {
		t.Fatalf("scanner did not produce movie and episode: movie=%v episode=%v", movie, episode)
	}
	streams := j.makeJFMediaStreams(movie)
	if len(streams) != 4 || streams[2].Type != "Subtitle" || !streams[2].IsDefault || streams[2].Language != "en" {
		t.Fatalf("unexpected movie streams: %#v", streams)
	}
	user := &model.User{ID: "test-user", Properties: model.UserProperties{EnabledFolders: []string{"movies", "shows"}}}
	if err := db.UpsertUser(context.Background(), user); err != nil {
		t.Fatal(err)
	}
	accessToken := &model.AccessToken{UserID: user.ID, Token: "subtitle-test-token"}
	if err := db.UpsertAccessToken(context.Background(), *accessToken); err != nil {
		t.Fatal(err)
	}
	reqContext := &requestContext{Token: accessToken, User: user}
	ctx := context.WithValue(context.Background(), requestContextKey, reqContext)
	detail, err := j.makeJFItem(ctx, user.ID, movie, "movies", true)
	if err != nil || len(detail.MediaStreams) != 4 || detail.MediaStreams[2].Type != "Subtitle" {
		t.Fatalf("movie item details did not include subtitle tracks: err=%v streams=%#v", err, detail.MediaStreams)
	}
	episodeDetail, err := j.makeJFItem(ctx, user.ID, episode, "shows", true)
	if err != nil || len(episodeDetail.MediaStreams) != 3 || episodeDetail.MediaStreams[2].Type != "Subtitle" {
		t.Fatalf("episode item details did not include subtitle tracks: err=%v streams=%#v", err, episodeDetail.MediaStreams)
	}

	playbackRequest := httptest.NewRequest(http.MethodPost, "/Items/"+movie.ID()+"/PlaybackInfo", nil)
	playbackRequest.Header.Set("X-Emby-Token", accessToken.Token)
	playbackResponse := httptest.NewRecorder()
	router.ServeHTTP(playbackResponse, playbackRequest)
	var playback JFPlaybackInfoResponse
	if err := json.Unmarshal(playbackResponse.Body.Bytes(), &playback); err != nil {
		t.Fatalf("decode playback response: %v; body=%s", err, playbackResponse.Body.String())
	}
	if playbackResponse.Code != http.StatusOK || len(playback.MediaSources) != 1 || playback.MediaSources[0].DefaultSubtitleStreamIndex != 2 || len(playback.MediaSources[0].MediaStreams) != 4 {
		t.Fatalf("unexpected playback info response: status=%d response=%#v", playbackResponse.Code, playback)
	}
	deliveryURL := playback.MediaSources[0].MediaStreams[2].DeliveryUrl
	if got := strings.TrimPrefix(deliveryURL, "/"); !strings.Contains(got, "api_key="+accessToken.Token) {
		t.Fatalf("subtitle delivery URL does not include the playback token: %q", deliveryURL)
	}
	subtitleRequest := httptest.NewRequest(http.MethodGet, deliveryURL, nil)
	subtitleResponse := httptest.NewRecorder()
	router.ServeHTTP(subtitleResponse, subtitleRequest)
	if subtitleResponse.Code != http.StatusOK || !strings.Contains(subtitleResponse.Body.String(), "WEBVTT") {
		t.Fatalf("authenticated subtitle URL fetch: status=%d content=%q", subtitleResponse.Code, subtitleResponse.Body.String())
	}
	if got := j.makeMediaSource(episode)[0].MediaStreams; len(got) != 3 || got[2].Language != "en" {
		t.Fatalf("unexpected episode streams: %#v", got)
	}

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request = request.WithContext(context.WithValue(request.Context(), requestContextKey, reqContext))
	request = mux.SetURLVars(request, map[string]string{
		"itemid": movie.ID(), "mediasourceid": movie.ID(), "index": "2", "format": "vtt",
	})
	response := httptest.NewRecorder()
	j.subtitleStreamHandler(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "WEBVTT") || !strings.Contains(response.Body.String(), "00:00:01.000") {
		t.Fatalf("movie SRT delivery: status=%d content=%q", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/", nil)
	request = request.WithContext(context.WithValue(request.Context(), requestContextKey, reqContext))
	request = mux.SetURLVars(request, map[string]string{
		"itemid": movie.ID(), "mediasourceid": movie.ID(), "index": "2", "format": "srt",
	})
	response = httptest.NewRecorder()
	j.subtitleStreamHandler(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "00:00:01,000") || strings.Contains(response.Body.String(), "WEBVTT") {
		t.Fatalf("raw SRT delivery: status=%d content=%q", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/", nil)
	request = request.WithContext(context.WithValue(request.Context(), requestContextKey, reqContext))
	request = mux.SetURLVars(request, map[string]string{
		"itemid": movie.ID(), "mediasourceid": movie.ID(), "index": "3", "format": "vtt",
	})
	response = httptest.NewRecorder()
	j.subtitleStreamHandler(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "text/vtt; charset=utf-8" || !strings.Contains(response.Body.String(), "French cue") {
		t.Fatalf("native VTT delivery: status=%d content-type=%q content=%q", response.Code, response.Header().Get("Content-Type"), response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/", nil)
	request = request.WithContext(context.WithValue(request.Context(), requestContextKey, reqContext))
	request = mux.SetURLVars(request, map[string]string{
		"itemid": itemprefix_episode + episode.ID(), "mediasourceid": episode.ID(), "index": "2", "format": "vtt",
	})
	response = httptest.NewRecorder()
	j.subtitleStreamHandler(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Episode cue") {
		t.Fatalf("episode subtitle delivery: status=%d content=%q item-path=%q subtitles=%#v id=%q", response.Code, response.Body.String(), episode.Path(), collection.SidecarSubtitles(episode), episode.ID())
	}

	request = httptest.NewRequest(http.MethodGet, "/", nil)
	denied := &model.User{ID: "denied"}
	deniedContext := &requestContext{Token: &model.AccessToken{UserID: denied.ID, Token: "denied"}, User: denied}
	request = request.WithContext(context.WithValue(request.Context(), requestContextKey, deniedContext))
	request = mux.SetURLVars(request, map[string]string{
		"itemid": movie.ID(), "mediasourceid": movie.ID(), "index": "2", "format": "vtt",
	})
	response = httptest.NewRecorder()
	j.subtitleStreamHandler(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("ungranted user subtitle status = %d, want 404", response.Code)
	}
}
