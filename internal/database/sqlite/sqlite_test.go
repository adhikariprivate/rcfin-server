package sqlite

import (
	"path/filepath"
	"testing"
	"time"
)

func TestNewUsesPureGoSQLiteDriver(t *testing.T) {
	repo, err := New(&ConfigFile{Filename: filepath.Join(t.TempDir(), "rcfin.db")})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() {
		_ = repo.dbReadHandle.Close()
		_ = repo.dbWriteHandle.Close()
	})

	var version string
	if err := repo.dbReadHandle.Get(&version, "SELECT sqlite_version()"); err != nil {
		t.Fatalf("query SQLite version: %v", err)
	}
	if version == "" {
		t.Fatal("SQLite returned an empty version")
	}
}

func TestAccessMigrationRevokesLegacyRegularUserOnly(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "rcfin.db")
	repo, err := New(&ConfigFile{Filename: filename})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	for _, user := range []struct {
		id    string
		admin bool
	}{{"regular", false}, {"admin", true}} {
		if _, err := repo.dbWriteHandle.Exec(`INSERT INTO users(id, username, password, created) VALUES (?, ?, '', ?)`, user.id, user.id, time.Now()); err != nil {
			t.Fatalf("insert user %q: %v", user.id, err)
		}
		if _, err := repo.dbWriteHandle.Exec(`INSERT INTO user_properties(userid, key, value) VALUES (?, 'enableallfolders', '1')`, user.id); err != nil {
			t.Fatalf("insert all-library property for %q: %v", user.id, err)
		}
		if user.admin {
			if _, err := repo.dbWriteHandle.Exec(`INSERT INTO user_properties(userid, key, value) VALUES (?, 'admin', '1')`, user.id); err != nil {
				t.Fatalf("insert admin property: %v", err)
			}
		}
		if _, err := repo.dbWriteHandle.Exec(`INSERT INTO user_properties(userid, key, value) VALUES (?, 'enabledfolders', 'movies,shows')`, user.id); err != nil {
			t.Fatalf("insert folder grants for %q: %v", user.id, err)
		}
	}
	if _, err := repo.dbWriteHandle.Exec(`DELETE FROM schema_migrations WHERE version = 1`); err != nil {
		t.Fatalf("reset migration marker: %v", err)
	}
	if err := repo.Close(); err != nil {
		t.Fatalf("close repo: %v", err)
	}

	repo, err = New(&ConfigFile{Filename: filename})
	if err != nil {
		t.Fatalf("reopen and migrate: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	var enabled string
	if err := repo.dbReadHandle.Get(&enabled, `SELECT value FROM user_properties WHERE userid = 'regular' AND key = 'enableallfolders'`); err != nil {
		t.Fatalf("read regular default: %v", err)
	}
	if enabled != "0" {
		t.Errorf("regular enableallfolders = %q, want 0", enabled)
	}
	var grants int
	if err := repo.dbReadHandle.Get(&grants, `SELECT count(*) FROM user_properties WHERE userid = 'regular' AND key = 'enabledfolders'`); err != nil {
		t.Fatalf("count regular grants: %v", err)
	}
	if grants != 0 {
		t.Errorf("regular retained %d legacy grant rows, want none", grants)
	}
	if err := repo.dbReadHandle.Get(&enabled, `SELECT value FROM user_properties WHERE userid = 'admin' AND key = 'enableallfolders'`); err != nil {
		t.Fatalf("read admin access: %v", err)
	}
	if enabled != "1" {
		t.Errorf("admin enableallfolders = %q, want preserved 1", enabled)
	}
	if err := repo.dbReadHandle.Get(&grants, `SELECT count(*) FROM user_properties WHERE userid = 'admin' AND key = 'enabledfolders'`); err != nil {
		t.Fatalf("count admin grants: %v", err)
	}
	if grants != 1 {
		t.Errorf("admin grant rows = %d, want preserved 1", grants)
	}
}
