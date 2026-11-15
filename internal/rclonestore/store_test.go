package rclonestore

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/object"
)

type recursiveFsWrapper struct {
	fs.Fs
	features *fs.Features
}

func (f recursiveFsWrapper) Features() *fs.Features { return f.features }

func TestListRecursiveFallsBackWhenBackendDoesNotSupportIt(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".Inbox", "Nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".Inbox", "Nested", "Movie.mkv"), []byte("media"), 0o600); err != nil {
		t.Fatal(err)
	}
	library, err := New(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer library.Close()
	if entries, supported, err := library.ListRecursive(context.Background()); err != nil {
		t.Fatal(err)
	} else if supported || entries != nil {
		t.Fatalf("local backend listing = (%#v, %v), want unsupported fallback", entries, supported)
	}

	// Simulate a backend that advertises ListR, since the local backend used
	// above intentionally exercises the caller's recursive directory-walk path.
	features := *library.backend.Features()
	features.ListR = func(ctx context.Context, _ string, callback fs.ListRCallback) error {
		return callback(fs.DirEntries{
			fs.NewDir(".Inbox", time.Unix(1, 0)),
			fs.NewDir(".Inbox/Nested", time.Unix(1, 0)),
			object.NewStaticObjectInfo(".Inbox/Nested/Movie.mkv", time.Unix(1, 0), 5, true, nil, library.backend),
		})
	}
	library.backend = recursiveFsWrapper{Fs: library.backend, features: &features}
	entries, supported, err := library.ListRecursive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !supported {
		t.Fatal("backend advertised ListR but it was not used")
	}
	found := false
	for _, entry := range entries {
		if entry.Path == ".Inbox/Nested/Movie.mkv" && !entry.IsDir && entry.Size == 5 {
			found = true
		}
	}
	if !found {
		t.Fatalf("recursive list did not return nested media: %#v", entries)
	}
}

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

func TestWriteSidecarCreatesAndReplacesLocalFile(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "Film"), 0o755); err != nil {
		t.Fatal(err)
	}
	library, err := New(context.Background(), root, map[string]any{"read_only": false})
	if err != nil {
		t.Fatal(err)
	}
	defer library.Close()

	if err := library.WriteSidecar("Film/movie.nfo", []byte("first"), false); err != nil {
		t.Fatalf("WriteSidecar() create error = %v", err)
	}
	if err := library.WriteSidecar("Film/movie.nfo", []byte("replacement"), false); err == nil {
		t.Fatal("WriteSidecar() replaced an existing file without overwrite permission")
	}
	if err := library.WriteSidecar("Film/movie.nfo", []byte("replacement"), true); err != nil {
		t.Fatalf("WriteSidecar() replacement error = %v", err)
	}
	got, err := library.ReadFile("Film/movie.nfo")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(got) != "replacement" {
		t.Fatalf("sidecar contents = %q, want replacement", got)
	}
}

func TestIsNotFoundRecognizesOSNotExistFromVFS(t *testing.T) {
	_, err := os.Stat(filepath.Join(t.TempDir(), "missing.nfo"))
	if !IsNotFound(err) {
		t.Fatalf("IsNotFound(%v) = false, want true", err)
	}
}
