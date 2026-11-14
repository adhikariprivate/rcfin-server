package jellyfin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/adhikariprivate/rcfin-server/internal/database/sqlite"
)

func TestAuthenticateUnknownUserWithoutAutoRegisterReturnsUnauthorized(t *testing.T) {
	repo, err := sqlite.New(&sqlite.ConfigFile{Filename: t.TempDir() + "/users.db"})
	if err != nil {
		t.Fatalf("create test database: %v", err)
	}
	j := New(&Options{Repo: repo, AutoRegister: false})
	request := httptest.NewRequest(http.MethodPost, "/Users/AuthenticateByName", strings.NewReader(`{"Username":"new-user","Pw":"secret"}`))
	response := httptest.NewRecorder()

	j.usersAuthenticateByNameHandler(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d; body: %s", response.Code, http.StatusUnauthorized, response.Body.String())
	}
}
