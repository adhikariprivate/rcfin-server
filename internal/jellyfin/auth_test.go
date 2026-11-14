package jellyfin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/adhikariprivate/rcfin-server/internal/database/model"
	"github.com/adhikariprivate/rcfin-server/internal/database/sqlite"
)

func TestCanAccessCollectionRequiresExplicitGrant(t *testing.T) {
	user := &model.User{ID: "u", Properties: model.UserProperties{EnableAllFolders: true}}
	if canAccessCollection(user, "movies") {
		t.Fatal("non-admin with legacy EnableAllFolders must not bypass explicit grants")
	}
	user.Properties.EnabledFolders = []string{"movies"}
	if !canAccessCollection(user, "movies") {
		t.Fatal("explicitly granted collection should be accessible")
	}
	user.Properties.Disabled = true
	if canAccessCollection(user, "movies") {
		t.Fatal("disabled account must not access a granted collection")
	}
	user.Properties.Disabled = false
	user.Properties.Admin = true
	user.Properties.EnabledFolders = nil
	if !canAccessCollection(user, "shows") {
		t.Fatal("administrators should bypass library grants")
	}
}

func TestAuthenticateUnknownUserReturnsUnauthorized(t *testing.T) {
	repo, err := sqlite.New(&sqlite.ConfigFile{Filename: t.TempDir() + "/users.db"})
	if err != nil {
		t.Fatalf("create test database: %v", err)
	}
	j := New(&Options{Repo: repo})
	request := httptest.NewRequest(http.MethodPost, "/Users/AuthenticateByName", strings.NewReader(`{"Username":"new-user","Pw":"secret"}`))
	response := httptest.NewRecorder()

	j.usersAuthenticateByNameHandler(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d; body: %s", response.Code, http.StatusUnauthorized, response.Body.String())
	}
}
