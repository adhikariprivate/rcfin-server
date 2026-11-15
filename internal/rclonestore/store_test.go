package rclonestore

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/object"
)

type moveTestFs struct {
	fs.Fs
	features *fs.Features
}

func (f moveTestFs) Features() *fs.Features { return f.features }

type moveTestCalls struct {
	dirMove int
	move    int
	copy    int
	source  string
	dest    string
}

func installRemoteMoveFeatures(t *testing.T, library *Library, calls *moveTestCalls, dirMove func(context.Context, fs.Fs, string, string) error, move func(context.Context, fs.Object, string) (fs.Object, error)) {
	t.Helper()
	base := library.backend
	baseFeatures := *base.Features()
	features := baseFeatures
	features.IsLocal = false
	// A backend can advertise Move/DirMove without Copy. Direct moves must not
	// require or invoke Copy.
	features.Copy = func(context.Context, fs.Object, string) (fs.Object, error) {
		calls.copy++
		return nil, fs.ErrorCantCopy
	}
	features.Move = func(ctx context.Context, source fs.Object, destination string) (fs.Object, error) {
		calls.move++
		calls.source, calls.dest = source.Remote(), destination
		if move != nil {
			return move(ctx, source, destination)
		}
		return baseFeatures.Move(ctx, source, destination)
	}
	features.DirMove = func(ctx context.Context, _ fs.Fs, source, destination string) error {
		calls.dirMove++
		calls.source, calls.dest = source, destination
		if dirMove != nil {
			return dirMove(ctx, base, source, destination)
		}
		return baseFeatures.DirMove(ctx, base, source, destination)
	}
	library.backend = moveTestFs{Fs: base, features: &features}
}

func newMoveTestLibrary(t *testing.T, sourceFile, sourceContents string) (*Library, *moveTestCalls) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, filepath.Dir(sourceFile)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, sourceFile), []byte(sourceContents), 0o600); err != nil {
		t.Fatal(err)
	}
	library, err := New(context.Background(), root, map[string]any{"read_only": false})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(library.Close)
	return library, &moveTestCalls{}
}

func TestMoveDirectoryUsesAdvertisedRemoteDirMoveWithoutCopy(t *testing.T) {
	library, calls := newMoveTestLibrary(t, "source/Movie.mkv", "large-media-placeholder")
	installRemoteMoveFeatures(t, library, calls, nil, nil)
	var logOutput bytes.Buffer
	previousLogger := log.Writer()
	log.SetOutput(&logOutput)
	defer log.SetOutput(previousLogger)
	if err := library.MoveDirectory(context.Background(), "source", "organized/Movie", false); err != nil {
		t.Fatalf("MoveDirectory() error = %v", err)
	}
	for _, expected := range []string{
		"rclonestore DirMove started: source=\"source\" destination=\"organized/Movie\"",
		"rclonestore DirMove complete: source=\"source\" destination=\"organized/Movie\"",
	} {
		if !bytes.Contains(logOutput.Bytes(), []byte(expected)) {
			t.Errorf("move log %q not found in %q", expected, logOutput.String())
		}
	}
	if calls.dirMove != 1 || calls.source != "source" || calls.dest != "organized/Movie" {
		t.Fatalf("DirMove calls = %#v, want original source and final destination", calls)
	}
	if calls.copy != 0 {
		t.Fatalf("Copy calls = %d, want 0", calls.copy)
	}
	if _, err := library.Stat("source"); !IsNotFound(err) {
		t.Fatalf("source stat error = %v, want source absent", err)
	}
	if info, err := library.Stat("organized/Movie"); err != nil || !info.IsDir() {
		t.Fatalf("destination = (%v, %v), want existing directory", info, err)
	}
}

func TestMoveDirectoryConflictDoesNotCallBackendOrCopy(t *testing.T) {
	library, calls := newMoveTestLibrary(t, "source/Movie.mkv", "media")
	if err := os.Mkdir(filepath.Join(library.remote, "destination"), 0o755); err != nil {
		t.Fatal(err)
	}
	installRemoteMoveFeatures(t, library, calls, nil, nil)
	if err := library.MoveDirectory(context.Background(), "source", "destination", false); err == nil {
		t.Fatal("MoveDirectory() succeeded with an existing destination")
	}
	if calls.dirMove != 0 || calls.copy != 0 {
		t.Fatalf("calls = %#v, want no backend move or copy after conflict", calls)
	}
	if _, err := library.Stat("source/Movie.mkv"); err != nil {
		t.Fatalf("source file was changed by conflict: %v", err)
	}
}

func TestMoveDirectoryBackendErrorDoesNotCopyOrDeleteSource(t *testing.T) {
	library, calls := newMoveTestLibrary(t, "source/Movie.mkv", "media")
	moveErr := fs.ErrorCantDirMove
	installRemoteMoveFeatures(t, library, calls, func(context.Context, fs.Fs, string, string) error { return moveErr }, nil)
	if err := library.MoveDirectory(context.Background(), "source", "destination", false); err == nil {
		t.Fatal("MoveDirectory() succeeded after backend DirMove error")
	}
	if calls.dirMove != 1 || calls.copy != 0 {
		t.Fatalf("calls = %#v, want one DirMove and no Copy fallback", calls)
	}
	if _, err := library.Stat("source/Movie.mkv"); err != nil {
		t.Fatalf("source file was deleted after failed DirMove: %v", err)
	}
	if _, err := library.Stat("destination"); !IsNotFound(err) {
		t.Fatalf("destination stat error = %v, want destination absent", err)
	}
}

func TestMoveDirectoryPartialStateNeedsRecoveryAndPreservesBothSides(t *testing.T) {
	library, calls := newMoveTestLibrary(t, "source/Movie.mkv", "media")
	installRemoteMoveFeatures(t, library, calls, func(_ context.Context, backend fs.Fs, source, destination string) error {
		if err := backend.Mkdir(context.Background(), destination); err != nil {
			return err
		}
		return fs.ErrorCantDirMove
	}, nil)
	if err := library.MoveDirectory(context.Background(), "source", "destination", false); err == nil {
		t.Fatal("MoveDirectory() succeeded after partial backend move")
	}
	if _, err := library.Stat("source/Movie.mkv"); err != nil {
		t.Fatalf("partial move lost source: %v", err)
	}
	if info, err := library.Stat("destination"); err != nil || !info.IsDir() {
		t.Fatalf("partial destination = (%v, %v), want preserved directory", info, err)
	}
	if err := library.MoveDirectory(context.Background(), "source", "destination", true); err == nil {
		t.Fatal("recovery accepted a destination that does not match the source")
	}
	if _, err := library.Stat("source/Movie.mkv"); err != nil {
		t.Fatalf("failed recovery deleted source: %v", err)
	}
}

func TestMoveDirectoryRecoveryDeletesOnlyAfterExactDestinationVerification(t *testing.T) {
	library, calls := newMoveTestLibrary(t, "source/Movie.mkv", "media")
	if err := os.MkdirAll(filepath.Join(library.remote, "destination"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(library.remote, "destination", "Movie.mkv"), []byte("media"), 0o600); err != nil {
		t.Fatal(err)
	}
	installRemoteMoveFeatures(t, library, calls, nil, nil)
	if err := library.MoveDirectory(context.Background(), "source", "destination", true); err != nil {
		t.Fatalf("verified recovery MoveDirectory() error = %v", err)
	}
	if calls.dirMove != 0 || calls.copy != 0 {
		t.Fatalf("calls = %#v, recovery should verify without another move or copy", calls)
	}
	if _, err := library.Stat("source"); !IsNotFound(err) {
		t.Fatalf("source stat error = %v, want verified source removed", err)
	}
}

func TestMoveDirectoryVerificationFailureDoesNotDeleteSource(t *testing.T) {
	library, calls := newMoveTestLibrary(t, "source/Movie.mkv", "media")
	installRemoteMoveFeatures(t, library, calls, func(context.Context, fs.Fs, string, string) error { return nil }, nil)
	if err := library.MoveDirectory(context.Background(), "source", "destination", false); err == nil {
		t.Fatal("MoveDirectory() succeeded when backend reported success without moving")
	}
	if calls.dirMove != 1 || calls.copy != 0 {
		t.Fatalf("calls = %#v, want one DirMove and no Copy", calls)
	}
	if _, err := library.Stat("source/Movie.mkv"); err != nil {
		t.Fatalf("source file was deleted after verification failure: %v", err)
	}
}

func TestMoveFileUsesAdvertisedRemoteMoveWithoutCopy(t *testing.T) {
	library, calls := newMoveTestLibrary(t, "Movie.mkv", "media")
	installRemoteMoveFeatures(t, library, calls, nil, nil)
	if err := library.MoveFile(context.Background(), "Movie.mkv", "Movie (2024).mkv", false); err != nil {
		t.Fatalf("MoveFile() error = %v", err)
	}
	if calls.move != 1 || calls.source != "Movie.mkv" || calls.dest != "Movie (2024).mkv" {
		t.Fatalf("Move calls = %#v, want original source and final destination", calls)
	}
	if calls.copy != 0 {
		t.Fatalf("Copy calls = %d, want 0", calls.copy)
	}
	if _, err := library.Stat("Movie.mkv"); !IsNotFound(err) {
		t.Fatalf("source stat error = %v, want source absent", err)
	}
	if _, err := library.Stat("Movie (2024).mkv"); err != nil {
		t.Fatalf("destination missing after Move: %v", err)
	}
}

func TestMoveFileBackendErrorDoesNotCopyOrDeleteSource(t *testing.T) {
	library, calls := newMoveTestLibrary(t, "Movie.mkv", "media")
	installRemoteMoveFeatures(t, library, calls, nil, func(context.Context, fs.Object, string) (fs.Object, error) {
		return nil, fs.ErrorCantMove
	})
	if err := library.MoveFile(context.Background(), "Movie.mkv", "renamed.mkv", false); err == nil {
		t.Fatal("MoveFile() succeeded after backend Move error")
	}
	if calls.move != 1 || calls.copy != 0 {
		t.Fatalf("calls = %#v, want one Move and no Copy fallback", calls)
	}
	if _, err := library.Stat("Movie.mkv"); err != nil {
		t.Fatalf("source file was deleted after failed Move: %v", err)
	}
}

func TestMoveFileConflictDoesNotCallBackendOrCopy(t *testing.T) {
	library, calls := newMoveTestLibrary(t, "Movie.mkv", "media")
	if err := os.WriteFile(filepath.Join(library.remote, "renamed.mkv"), []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}
	installRemoteMoveFeatures(t, library, calls, nil, nil)
	if err := library.MoveFile(context.Background(), "Movie.mkv", "renamed.mkv", false); err == nil {
		t.Fatal("MoveFile() succeeded with an existing destination")
	}
	if calls.move != 0 || calls.copy != 0 {
		t.Fatalf("calls = %#v, want no backend move or copy after conflict", calls)
	}
	if _, err := library.Stat("Movie.mkv"); err != nil {
		t.Fatalf("source file was changed by conflict: %v", err)
	}
}

func TestMoveFileVerificationFailureDoesNotDeleteSource(t *testing.T) {
	library, calls := newMoveTestLibrary(t, "Movie.mkv", "media")
	installRemoteMoveFeatures(t, library, calls, nil, func(context.Context, fs.Object, string) (fs.Object, error) { return nil, nil })
	if err := library.MoveFile(context.Background(), "Movie.mkv", "renamed.mkv", false); err == nil {
		t.Fatal("MoveFile() succeeded when backend reported success without moving")
	}
	if _, err := library.Stat("Movie.mkv"); err != nil {
		t.Fatalf("source file was deleted after verification failure: %v", err)
	}
}

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
	if mode := library.InventoryListingMode(); mode != "directory_walk" {
		t.Fatalf("local inventory listing mode = %q, want directory_walk", mode)
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
	if mode := library.InventoryListingMode(); mode != "rclone_recursive" {
		t.Fatalf("advertised inventory listing mode = %q, want rclone_recursive", mode)
	}
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

func TestListRecursiveRejectsUnsafePathsInsteadOfCommittingPartialInventory(t *testing.T) {
	library, err := New(context.Background(), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer library.Close()
	features := *library.backend.Features()
	features.ListR = func(ctx context.Context, _ string, callback fs.ListRCallback) error {
		return callback(fs.DirEntries{fs.NewDir("../outside", time.Unix(1, 0))})
	}
	library.backend = recursiveFsWrapper{Fs: library.backend, features: &features}
	if _, supported, err := library.ListRecursive(context.Background()); err == nil || !supported {
		t.Fatalf("ListRecursive() = (supported %v, err %v), want supported listing failure", supported, err)
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
