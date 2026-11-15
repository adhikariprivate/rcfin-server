package webdav

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adhikariprivate/rcfin-server/internal/collection"
	"github.com/adhikariprivate/rcfin-server/internal/database/model"
	"github.com/adhikariprivate/rcfin-server/internal/database/sqlite"
	"github.com/adhikariprivate/rcfin-server/internal/rclonestore"
	"golang.org/x/crypto/bcrypt"
)

func TestServerAuthListsGrantedLibraryAndRejectsWrites(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(&sqlite.ConfigFile{Filename: filepath.Join(t.TempDir(), "dav.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	password, err := bcrypt.GenerateFromPassword([]byte("secret"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertUser(ctx, &model.User{ID: "user-1", Username: "alice", Password: string(password), Properties: model.UserProperties{Admin: true}}); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "Film (2020)"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Film (2020)", "Film.mkv"), []byte("video bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	storage, err := rclonestore.New(ctx, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	collections := collection.New(&collection.Options{Repo: db})
	collections.AddCollection("Movies", "movies", "movies", root, storage)
	if err := collections.InitContext(ctx); err != nil {
		t.Fatal(err)
	}
	defer collections.Close()
	handler := New(Options{Path: "/dav/", Collections: collections, Repo: db, Libraries: map[string]LibraryConfig{"movies": {Enabled: true, Views: []string{"all"}, ShowNFO: true, ShowArtwork: true}}})
	request := httptest.NewRequest("PROPFIND", "/dav/", strings.NewReader(`<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:allprop/></d:propfind>`))
	request.Header.Set("Depth", "1")
	request.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("alice:secret")))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 207 {
		t.Fatalf("PROPFIND status = %d, body %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "Movies") {
		t.Fatalf("PROPFIND omitted granted library: %s", response.Body.String())
	}
	get := httptest.NewRequest(http.MethodGet, "/dav/Movies/All/Film%20(2020)/Film.mkv", nil)
	get.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("alice:secret")))
	getResponse := httptest.NewRecorder()
	handler.ServeHTTP(getResponse, get)
	if getResponse.Code != http.StatusOK || getResponse.Body.String() != "video bytes" {
		t.Fatalf("virtual video GET = %d %q", getResponse.Code, getResponse.Body.String())
	}
	ranged := httptest.NewRequest(http.MethodGet, "/dav/Movies/All/Film%20(2020)/Film.mkv", nil)
	ranged.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("alice:secret")))
	ranged.Header.Set("Range", "bytes=0-4")
	rangeResponse := httptest.NewRecorder()
	handler.ServeHTTP(rangeResponse, ranged)
	if rangeResponse.Code != http.StatusPartialContent || rangeResponse.Body.String() != "video" {
		t.Fatalf("ranged virtual video GET = %d %q", rangeResponse.Code, rangeResponse.Body.String())
	}
	nfo := httptest.NewRequest(http.MethodGet, "/dav/Movies/All/Film%20(2020)/movie.nfo", nil)
	nfo.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("alice:secret")))
	nfoResponse := httptest.NewRecorder()
	handler.ServeHTTP(nfoResponse, nfo)
	if nfoResponse.Code != http.StatusOK || !strings.Contains(nfoResponse.Body.String(), "<movie>") {
		t.Fatalf("generated NFO GET = %d %q", nfoResponse.Code, nfoResponse.Body.String())
	}

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/dav/", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET status = %d", unauthorized.Code)
	}

	put := httptest.NewRequest(http.MethodPut, "/dav/Movies/All/Film%20(2020)/Film.mkv", strings.NewReader("overwritten"))
	put.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("alice:secret")))
	writeResponse := httptest.NewRecorder()
	handler.ServeHTTP(writeResponse, put)
	if writeResponse.Code != http.StatusForbidden {
		t.Fatalf("PUT status = %d, want forbidden", writeResponse.Code)
	}
	options := httptest.NewRequest(http.MethodOptions, "/dav/", nil)
	options.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("alice:secret")))
	optionsResponse := httptest.NewRecorder()
	handler.ServeHTTP(optionsResponse, options)
	if optionsResponse.Code != http.StatusOK || optionsResponse.Header().Get("DAV") != "1" || strings.Contains(optionsResponse.Header().Get("Allow"), "PUT") {
		t.Fatalf("read-only OPTIONS response = %d, DAV %q, Allow %q", optionsResponse.Code, optionsResponse.Header().Get("DAV"), optionsResponse.Header().Get("Allow"))
	}
}
