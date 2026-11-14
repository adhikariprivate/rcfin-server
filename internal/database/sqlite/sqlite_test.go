package sqlite

import (
	"path/filepath"
	"testing"
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
