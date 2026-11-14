package rclonestore

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestConfigureLoadsNamedRemoteFromConfigFile(t *testing.T) {
	tempDir := t.TempDir()
	remoteRoot := filepath.Join(tempDir, "media")
	if err := os.MkdirAll(remoteRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(tempDir, "rclone.config")
	if err := os.WriteFile(configPath, []byte("[files]\ntype = local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Configure(configPath, filepath.Join(tempDir, "vfs-cache")); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}

	library, err := New(context.Background(), "files:"+filepath.ToSlash(remoteRoot), nil)
	if err != nil {
		t.Fatalf("New() did not load the named remote from the configured file: %v", err)
	}
	defer library.Close()

	if got := library.Remote(); got != "files:"+filepath.ToSlash(remoteRoot) {
		t.Errorf("Remote() = %q, want configured remote", got)
	}
}
