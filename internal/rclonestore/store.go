// Package rclonestore embeds rclone's filesystem and VFS layers for media access.
//
// Each media library gets an independent rclone backend (Fs) plus a VFS
// instance rooted at the configured remote, for example "movies:Films" or a
// plain local path such as "/media/movies". The VFS layer supplies directory
// listings, file metadata and seekable read handles, so media is served
// straight from the remote: no FUSE mount and no pre-sync is required.
//
// Rclone's own configuration (remotes, credentials) stays global and is read
// once from the standard rclone config file. Everything that tunes how a
// library behaves — directory caching, read chunking, cache mode — is
// configured per library and documented in docs/rclone.md.
package rclonestore

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/rclone/rclone/backend/all"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configfile"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/vfs"
	"github.com/rclone/rclone/vfs/vfscommon"
)

// Configure selects the process-wide rclone config file and local VFS cache
// directory. Rclone config is global; each library still gets its own Fs/VFS.
//
// An empty configFile selects rclone's in-memory configuration, which is
// enough for plain local paths and for remotes fully described by
// RCLONE_CONFIG_* environment variables.
func Configure(configFile, cacheDir string) error {
	// The Rclone CLI installs this file-backed storage implementation from its
	// own command package. Embedded consumers must install it explicitly or
	// config.SetConfigPath has no effect and remotes remain memory-only.
	configfile.Install()
	if configFile != "" {
		if err := config.SetConfigPath(configFile); err != nil {
			return fmt.Errorf("set rclone config path: %w", err)
		}
	}
	if cacheDir != "" {
		if err := config.SetCacheDir(cacheDir); err != nil {
			return fmt.Errorf("set rclone cache directory: %w", err)
		}
	}
	return nil
}

// Library is an independent rclone filesystem and VFS instance for one media
// library. Paths passed to its methods are relative to the configured remote.
type Library struct {
	remote     string
	backend    fs.Fs
	vfs        *vfs.VFS
	writeMu    sync.Mutex
	mutationMu sync.Mutex
	openMu     sync.RWMutex
}

type Capabilities struct {
	Move           bool `json:"move"`
	DirMove        bool `json:"dir_move"`
	Put            bool `json:"put"`
	PutStream      bool `json:"put_stream"`
	Copy           bool `json:"copy"`
	Local          bool `json:"local"`
	SameFilesystem bool `json:"same_filesystem"`
}

const maxSidecarBytes = 32 << 20

func (l *Library) Capabilities() Capabilities {
	if l == nil || l.backend == nil || l.vfs == nil {
		return Capabilities{}
	}
	f := l.backend.Features()
	return Capabilities{
		Move:           f.Move != nil,
		DirMove:        f.DirMove != nil,
		Put:            !l.vfs.Opt.ReadOnly,
		PutStream:      f.PutStream != nil,
		Copy:           f.Copy != nil,
		Local:          f.IsLocal,
		SameFilesystem: true,
	}
}

// New creates an rclone-backed library rooted at remote, for example
// "movies:Films". A plain local directory path also works and needs no rclone
// configuration at all. VFS options use rclone's config names and value
// syntax; see VFSOptions.
func New(ctx context.Context, remote string, options map[string]any) (*Library, error) {
	if strings.TrimSpace(remote) == "" {
		return nil, fmt.Errorf("rclone remote path is required")
	}
	backend, err := fs.NewFs(ctx, remote)
	if err != nil {
		return nil, fmt.Errorf("create rclone filesystem for %q: %w", remote, err)
	}

	opt, err := VFSOptions(options)
	if err != nil {
		return nil, fmt.Errorf("invalid VFS options for %q: %w", remote, err)
	}
	v := vfs.New(ctx, backend, &opt)
	return &Library{remote: remote, backend: backend, vfs: v}, nil
}

// Close shuts down the VFS and its cache workers.
func (l *Library) Close() {
	if l != nil && l.vfs != nil {
		l.vfs.Shutdown()
	}
}

// Remote returns the configured remote path, without exposing credentials.
func (l *Library) Remote() string { return l.remote }

// FlushDirCache drops cached directory listings so the next scan or browse
// observes current remote state. The collection scanner calls this before
// every rescan, which is what makes long directory cache times safe: browsing
// stays cheap while library updates are never stale.
func (l *Library) FlushDirCache() {
	if l != nil && l.vfs != nil {
		l.vfs.FlushDirCache()
	}
}

// OpenFile opens a file relative to the library root. The returned VFS handle
// implements http.File, including seeking for HTTP range requests.
func (l *Library) OpenFile(name string) (http.File, error) {
	clean, err := relativePath(name)
	if err != nil {
		return nil, err
	}
	l.openMu.RLock()
	defer l.openMu.RUnlock()
	return l.vfs.OpenFile(clean, os.O_RDONLY, 0)
}

// ReadFile reads a small sidecar file, such as NFO metadata, from the remote.
func (l *Library) ReadFile(name string) ([]byte, error) {
	file, err := l.OpenFile(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	const maxSidecarSize = 16 << 20
	data, err := io.ReadAll(io.LimitReader(file, maxSidecarSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxSidecarSize {
		return nil, fmt.Errorf("sidecar file %q exceeds %d bytes", name, maxSidecarSize)
	}
	return data, nil
}

// OpenSubtitlesHash computes OpenSubtitles' 64-bit file hash by reading only
// the first and last 64 KiB plus the file size. It never buffers the video.
func (l *Library) OpenSubtitlesHash(name string) (string, error) {
	file, err := l.OpenFile(name)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if info.Size() < 128*1024 {
		return "", fmt.Errorf("file is too small for OpenSubtitles hash")
	}
	const chunkSize = 64 * 1024
	var sum uint64 = uint64(info.Size())
	buffer := make([]byte, chunkSize)
	for _, offset := range []int64{0, info.Size() - chunkSize} {
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			return "", err
		}
		if _, err := io.ReadFull(file, buffer); err != nil {
			return "", err
		}
		for i := 0; i < len(buffer); i += 8 {
			sum += binary.LittleEndian.Uint64(buffer[i : i+8])
		}
	}
	return fmt.Sprintf("%016x", sum), nil
}

// WriteSidecar writes a bounded sidecar through the VFS using a temporary
// sibling and promotion. It refuses read-only VFS configurations and existing
// destinations unless overwrite is explicitly enabled. The caller must still
// treat backend rename semantics as implementation-dependent.
func (l *Library) WriteSidecar(name string, data []byte, overwrite bool) error {
	if l == nil || l.vfs == nil {
		return fmt.Errorf("library storage is unavailable")
	}
	if l.vfs.Opt.ReadOnly {
		return fmt.Errorf("library VFS is read-only")
	}
	if len(data) > maxSidecarBytes {
		return fmt.Errorf("sidecar exceeds 32 MiB limit")
	}
	clean, err := safeMutationPath(name)
	if err != nil {
		return err
	}
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	l.mutationMu.Lock()
	defer l.mutationMu.Unlock()
	l.openMu.Lock()
	defer l.openMu.Unlock()
	_, statErr := l.vfs.Stat(clean)
	exists := statErr == nil
	if statErr != nil && !errors.Is(statErr, fs.ErrorObjectNotFound) && !errors.Is(statErr, fs.ErrorDirNotFound) {
		return fmt.Errorf("check sidecar destination %q: %w", clean, statErr)
	}
	if exists && !overwrite {
		return fmt.Errorf("sidecar destination %q already exists", clean)
	}
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return err
	}
	temp := path.Join(path.Dir(clean), ".rcfin-tmp-"+hex.EncodeToString(random)+path.Ext(clean))
	if err := l.vfs.WriteFile(temp, data, 0o644); err != nil {
		_ = l.vfs.Remove(temp)
		return fmt.Errorf("write temporary sidecar: %w", err)
	}
	l.vfs.WaitForWriters(30 * time.Second)
	tempInfo, err := l.vfs.Stat(temp)
	if err != nil {
		_ = l.vfs.Remove(temp)
		return fmt.Errorf("verify temporary sidecar: %w", err)
	}
	if tempInfo.Size() != int64(len(data)) {
		_ = l.vfs.Remove(temp)
		return fmt.Errorf("temporary sidecar size verification failed: expected=%d actual=%d", len(data), tempInfo.Size())
	}
	tempObject, err := l.backend.NewObject(context.Background(), temp)
	if err != nil {
		_ = l.vfs.Remove(temp)
		return fmt.Errorf("load temporary sidecar for verification: %w", err)
	}
	if err := verifySidecarData(tempObject, data); err != nil {
		_ = l.vfs.Remove(temp)
		return fmt.Errorf("temporary sidecar content verification failed: %w", err)
	}
	if !overwrite {
		if _, err := l.vfs.Stat(clean); err == nil {
			_ = l.vfs.Remove(temp)
			return fmt.Errorf("sidecar destination %q appeared before promotion", clean)
		} else if !errors.Is(err, fs.ErrorObjectNotFound) && !errors.Is(err, fs.ErrorDirNotFound) {
			_ = l.vfs.Remove(temp)
			return fmt.Errorf("recheck sidecar destination: %w", err)
		}
	}
	var backupPath string
	var backupObject fs.Object
	if exists && overwrite {
		backupPath = path.Join(path.Dir(clean), ".rcfin-backup-"+hex.EncodeToString(random))
		if l.backend.Features().IsLocal {
			if err := l.vfs.Rename(clean, backupPath); err != nil {
				_ = l.vfs.Remove(temp)
				return fmt.Errorf("stage existing sidecar for safe replacement: %w", err)
			}
			backupObject, err = l.backend.NewObject(context.Background(), backupPath)
		} else {
			if l.backend.Features().Copy == nil {
				_ = l.vfs.Remove(temp)
				return fmt.Errorf("safe overwrite requires backend Copy to preserve the existing sidecar")
			}
			oldObject, oldErr := l.backend.NewObject(context.Background(), clean)
			if oldErr != nil {
				_ = l.vfs.Remove(temp)
				return fmt.Errorf("reload existing sidecar before overwrite: %w", oldErr)
			}
			backupObject, err = l.backend.Features().Copy(context.Background(), oldObject, backupPath)
			if err == nil && backupObject == nil {
				backupObject, err = l.backend.NewObject(context.Background(), backupPath)
			}
			if err == nil {
				err = verifyObjectContent(context.Background(), oldObject, backupObject)
			}
		}
		if err != nil {
			_ = l.vfs.Remove(temp)
			if l.backend.Features().IsLocal {
				if _, destinationErr := l.vfs.Stat(clean); IsNotFound(destinationErr) {
					if restoreErr := l.vfs.Rename(backupPath, clean); restoreErr != nil {
						return fmt.Errorf("preserve existing sidecar failed: %v; prior sidecar remains at %q because restore failed: %w", err, backupPath, restoreErr)
					}
					l.vfs.FlushDirCache()
				} else if destinationErr != nil {
					return fmt.Errorf("preserve existing sidecar failed: %v; prior sidecar may remain at %q: inspect destination failed: %w", err, backupPath, destinationErr)
				}
			} else if backupPath != "" {
				_ = l.vfs.Remove(backupPath)
			}
			return fmt.Errorf("preserve existing sidecar before overwrite: %w", err)
		}
		if !l.backend.Features().IsLocal {
			currentObject, currentErr := l.backend.NewObject(context.Background(), clean)
			if currentErr != nil && !IsNotFound(currentErr) {
				_ = l.vfs.Remove(temp)
				return fmt.Errorf("recheck existing sidecar before replacement; verified backup retained at %q: %w", backupPath, currentErr)
			}
			if currentErr == nil {
				if verifyErr := verifyObjectContent(context.Background(), backupObject, currentObject); verifyErr != nil {
					_ = l.vfs.Remove(temp)
					return fmt.Errorf("existing sidecar changed before replacement; verified backup retained at %q: %w", backupPath, verifyErr)
				}
				if err := currentObject.Remove(context.Background()); err != nil {
					_ = l.vfs.Remove(temp)
					return fmt.Errorf("remove verified old sidecar for replacement; backup retained at %q: %w", backupPath, err)
				}
				l.vfs.FlushDirCache()
			}
			if _, err := l.vfs.Stat(clean); err == nil {
				_ = l.vfs.Remove(temp)
				return fmt.Errorf("sidecar destination raced with overwrite; verified old sidecar retained at %q", backupPath)
			} else if !errors.Is(err, fs.ErrorObjectNotFound) && !errors.Is(err, fs.ErrorDirNotFound) {
				_ = l.vfs.Remove(temp)
				return fmt.Errorf("recheck sidecar destination before overwrite; verified backup retained at %q: %w", backupPath, err)
			}
		}
	}
	if err := l.vfs.Rename(temp, clean); err != nil {
		_ = l.vfs.Remove(temp)
		return l.restoreSidecarBackup(clean, backupPath, backupObject, fmt.Errorf("promote sidecar %q: %w", clean, err))
	}
	l.vfs.FlushDirCache()
	destinationInfo, err := l.vfs.Stat(clean)
	if err != nil {
		return l.restoreSidecarBackup(clean, backupPath, backupObject, fmt.Errorf("sidecar may have been promoted but destination verification failed: %w", err))
	}
	destinationObject, objectErr := l.backend.NewObject(context.Background(), clean)
	if objectErr != nil {
		return l.restoreSidecarBackup(clean, backupPath, backupObject, fmt.Errorf("load promoted sidecar for verification: %w", objectErr))
	}
	if destinationInfo.Size() != int64(len(data)) || verifySidecarData(destinationObject, data) != nil {
		return l.restoreSidecarBackup(clean, backupPath, backupObject, fmt.Errorf("promoted sidecar content verification failed"))
	}
	if backupObject != nil {
		if err := backupObject.Remove(context.Background()); err != nil {
			return fmt.Errorf("new sidecar verified but old sidecar backup %q could not be removed: %w", backupPath, err)
		}
	}
	return nil
}

func verifySidecarData(object fs.Object, expected []byte) error {
	if object.Size() != int64(len(expected)) {
		return fmt.Errorf("sidecar size mismatch: expected=%d actual=%d", len(expected), object.Size())
	}
	reader, err := object.Open(context.Background())
	if err != nil {
		return err
	}
	defer reader.Close()
	actual, err := io.ReadAll(io.LimitReader(reader, int64(len(expected))+1))
	if err != nil {
		return err
	}
	if !bytes.Equal(actual, expected) {
		return fmt.Errorf("sidecar bytes differ from staged content")
	}
	return nil
}

func (l *Library) restoreSidecarBackup(destination, backupPath string, backup fs.Object, cause error) error {
	if backup == nil {
		return cause
	}
	if current, err := l.backend.NewObject(context.Background(), destination); err == nil {
		if verifyObjectContent(context.Background(), backup, current) == nil {
			if backupPath != "" && backup.Remote() != destination {
				_ = backup.Remove(context.Background())
			}
			return cause
		}
		return fmt.Errorf("%v; destination is occupied by unverified content and prior sidecar backup is retained at %q", cause, backupPath)
	} else if !IsNotFound(err) {
		return fmt.Errorf("%v; inspect destination before restoring backup: %w", cause, err)
	}
	if backup.Remote() != destination {
		if err := l.vfs.Rename(backupPath, destination); err != nil {
			return fmt.Errorf("%v; restore prior sidecar from %q: %w", cause, backupPath, err)
		}
	}
	l.vfs.FlushDirCache()
	return cause
}

// Stat reports metadata for a path relative to the library root.
func (l *Library) Stat(name string) (os.FileInfo, error) {
	clean, err := relativePath(name)
	if err != nil {
		return nil, err
	}
	node, err := l.vfs.Stat(clean)
	if err != nil {
		return nil, err
	}
	return node, nil
}

// IsNotFound reports whether an Rclone/VFS operation failed because the
// requested object or parent directory does not exist.
func IsNotFound(err error) bool {
	return err != nil && (errors.Is(err, fs.ErrorObjectNotFound) || errors.Is(err, fs.ErrorDirNotFound) || errors.Is(err, os.ErrNotExist))
}

// MoveFile stages a backend copy, verifies the final destination, and removes
// the original only after verification. The backend must advertise Copy and
// Move; RCFin does not use an unverified move or copy/delete fallback.
func (l *Library) MoveFile(ctx context.Context, source, destination string, recoverExisting bool) error {
	return l.move(ctx, source, destination, false, recoverExisting)
}

// MoveDirectory performs only an advertised backend DirMove. It never falls
// back to per-file copy/delete when the backend lacks directory move support.
func (l *Library) MoveDirectory(ctx context.Context, source, destination string, recoverExisting bool) error {
	return l.move(ctx, source, destination, true, recoverExisting)
}

// EnsureDirectory creates a path beneath the collection root using the VFS.
func (l *Library) EnsureDirectory(name string) error {
	if l == nil || l.vfs == nil {
		return fmt.Errorf("library storage is unavailable")
	}
	if l.vfs.Opt.ReadOnly {
		return fmt.Errorf("library VFS is read-only")
	}
	clean, err := safeMutationPath(name)
	if err != nil {
		return err
	}
	l.mutationMu.Lock()
	defer l.mutationMu.Unlock()
	l.openMu.Lock()
	defer l.openMu.Unlock()
	if err := l.vfs.MkdirAll(clean, 0o755); err != nil {
		return err
	}
	l.vfs.FlushDirCache()
	return nil
}

func (l *Library) move(ctx context.Context, source, destination string, directory, recoverExisting bool) error {
	if l == nil || l.vfs == nil || l.backend == nil {
		return fmt.Errorf("library storage is unavailable")
	}
	if l.vfs.Opt.ReadOnly {
		return fmt.Errorf("library VFS is read-only")
	}
	src, err := safeMutationPath(source)
	if err != nil {
		return err
	}
	dst, err := safeMutationPath(destination)
	if err != nil {
		return err
	}
	if src == dst {
		return fmt.Errorf("source and destination paths are identical")
	}
	l.mutationMu.Lock()
	defer l.mutationMu.Unlock()
	l.openMu.Lock()
	defer l.openMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if l.vfs.Busy() {
		return fmt.Errorf("VFS has open or pending files; retry after handles are idle")
	}
	l.vfs.FlushDirCache()
	features := l.backend.Features()
	if directory && features.DirMove == nil {
		return fmt.Errorf("backend does not advertise DirMove")
	}
	if directory && !features.IsLocal && features.Copy == nil {
		return fmt.Errorf("remote directory moves require advertised Copy for staged verification")
	}
	if !directory && (features.Move == nil || features.Copy == nil && !features.IsLocal) {
		return fmt.Errorf("backend must advertise Copy and Move for verified file moves, or be a local filesystem with atomic Move")
	}
	sourceInfo, err := l.vfs.Stat(src)
	if err != nil {
		return fmt.Errorf("stat move source %q: %w", src, err)
	}
	if destinationInfo, statErr := l.vfs.Stat(dst); statErr == nil {
		if directory {
			if !destinationInfo.IsDir() {
				return fmt.Errorf("move destination %q is not a directory", dst)
			}
			if !recoverExisting {
				return fmt.Errorf("move destination %q already exists", dst)
			}
			if err := l.verifyDirectorySubset(ctx, src, dst); err != nil {
				return fmt.Errorf("existing directory destination does not verify against source: %w", err)
			}
			if err := l.removeDirectoryTree(ctx, src, dst); err != nil {
				return fmt.Errorf("remove source after verified directory recovery: %w", err)
			}
			return nil
		}
		if destinationInfo.IsDir() {
			return fmt.Errorf("move destination %q already exists", dst)
		}
		if features.IsLocal && features.Copy == nil {
			return fmt.Errorf("local move found an existing destination and no Copy operation is available for recovery")
		}
		if !recoverExisting {
			return fmt.Errorf("move destination %q already exists", dst)
		}
		if err := l.moveFileVerified(ctx, features, src, dst, sourceInfo.Size(), recoverExisting); err != nil {
			return err
		}
		l.vfs.FlushDirCache()
		return nil
	} else if !IsNotFound(statErr) {
		return fmt.Errorf("check move destination %q: %w", dst, statErr)
	}
	var sourceTreeBytes int64
	if directory {
		sourceTreeBytes, err = l.DirectorySize(ctx, src)
		if err != nil {
			return fmt.Errorf("measure move source directory: %w", err)
		}
	}
	if !directory {
		if sourceInfo.IsDir() {
			return fmt.Errorf("file move source is a directory")
		}
		if features.IsLocal {
			sourceObject, objectErr := l.backend.NewObject(ctx, src)
			if objectErr != nil {
				return fmt.Errorf("load local move source object: %w", objectErr)
			}
			if _, moveErr := features.Move(ctx, sourceObject, dst); moveErr != nil {
				return fmt.Errorf("local atomic Move failed: %w", moveErr)
			}
		} else if err := l.moveFileVerified(ctx, features, src, dst, sourceInfo.Size(), recoverExisting); err != nil {
			return err
		}
	} else if features.IsLocal {
		if err := features.DirMove(ctx, l.backend, src, dst); err != nil {
			return fmt.Errorf("local directory Move failed: %w", err)
		}
	} else if err := l.moveDirectoryVerified(ctx, features, src, dst, sourceTreeBytes); err != nil {
		return fmt.Errorf("verified directory move failed: %w", err)
	}
	l.vfs.FlushDirCache()
	destinationInfo, err := l.vfs.Stat(dst)
	if err != nil {
		return fmt.Errorf("move may have partially completed; destination verification failed: %w", err)
	}
	if _, sourceErr := l.vfs.Stat(src); !IsNotFound(sourceErr) {
		if sourceErr == nil {
			return fmt.Errorf("move verification found both source and destination")
		}
		return fmt.Errorf("move may have partially completed; source verification failed: %w", sourceErr)
	}
	if directory && !destinationInfo.IsDir() {
		return fmt.Errorf("directory move destination is not a directory")
	}
	if directory {
		destinationTreeBytes, sizeErr := l.DirectorySize(ctx, dst)
		if sizeErr != nil {
			return fmt.Errorf("verify move destination directory size: %w", sizeErr)
		}
		if destinationTreeBytes != sourceTreeBytes {
			return fmt.Errorf("directory move size verification failed: source=%d destination=%d", sourceTreeBytes, destinationTreeBytes)
		}
	}
	return nil
}

type directorySnapshot struct {
	root  string
	files map[string]fs.Object
	dirs  []string
	bytes int64
}

func (l *Library) snapshotDirectory(ctx context.Context, root string) (directorySnapshot, error) {
	snapshot := directorySnapshot{root: root, files: map[string]fs.Object{}, dirs: []string{root}}
	var walk func(string) error
	walk = func(directory string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := l.ReadDir(directory)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			name := path.Join(directory, entry.Name())
			if entry.IsDir() {
				snapshot.dirs = append(snapshot.dirs, name)
				if err := walk(name); err != nil {
					return err
				}
				continue
			}
			if entry.Size() < 0 || snapshot.bytes > int64(^uint64(0)>>1)-entry.Size() {
				return fmt.Errorf("file size is unavailable or overflows at %q", name)
			}
			object, err := l.backend.NewObject(ctx, name)
			if err != nil {
				return fmt.Errorf("load directory file %q: %w", name, err)
			}
			snapshot.files[strings.TrimPrefix(name, root+"/")] = object
			snapshot.bytes += entry.Size()
		}
		return nil
	}
	if err := walk(root); err != nil {
		return directorySnapshot{}, err
	}
	sort.Strings(snapshot.dirs)
	return snapshot, nil
}

func (l *Library) verifyDirectoryContent(ctx context.Context, source, destination string) error {
	src, err := l.snapshotDirectory(ctx, source)
	if err != nil {
		return fmt.Errorf("snapshot source directory: %w", err)
	}
	dst, err := l.snapshotDirectory(ctx, destination)
	if err != nil {
		return fmt.Errorf("snapshot destination directory: %w", err)
	}
	return verifyDirectorySnapshots(ctx, src, dst, false)
}

// verifyDirectorySubset permits recovery after source cleanup removed some
// entries: every file and directory still present must match its verified copy.
func (l *Library) verifyDirectorySubset(ctx context.Context, source, destination string) error {
	src, err := l.snapshotDirectory(ctx, source)
	if err != nil {
		return fmt.Errorf("snapshot source directory: %w", err)
	}
	dst, err := l.snapshotDirectory(ctx, destination)
	if err != nil {
		return fmt.Errorf("snapshot destination directory: %w", err)
	}
	return verifyDirectorySnapshots(ctx, src, dst, true)
}

func verifyDirectorySnapshots(ctx context.Context, source, destination directorySnapshot, subset bool) error {
	if source.bytes > destination.bytes || len(source.files) > len(destination.files) {
		return fmt.Errorf("source directory is larger than verified destination")
	}
	if !subset && (source.bytes != destination.bytes || len(source.files) != len(destination.files)) {
		return fmt.Errorf("directory contents differ: source files=%d bytes=%d destination files=%d bytes=%d", len(source.files), source.bytes, len(destination.files), destination.bytes)
	}
	destinationDirs := make(map[string]bool, len(destination.dirs))
	for _, directory := range destination.dirs {
		destinationDirs[relativeDirectory(destination.root, directory)] = true
	}
	sourceDirs := make(map[string]bool, len(source.dirs))
	for _, directory := range source.dirs {
		relative := relativeDirectory(source.root, directory)
		sourceDirs[relative] = true
		if !destinationDirs[relative] {
			return fmt.Errorf("destination is missing directory %q", relative)
		}
	}
	if !subset && len(sourceDirs) != len(destinationDirs) {
		return fmt.Errorf("directory sets differ: source directories=%d destination directories=%d", len(sourceDirs), len(destinationDirs))
	}
	for relative, sourceObject := range source.files {
		destinationObject, ok := destination.files[relative]
		if !ok {
			return fmt.Errorf("destination is missing file %q", relative)
		}
		if err := verifyObjectContent(ctx, sourceObject, destinationObject); err != nil {
			return fmt.Errorf("verify file %q: %w", relative, err)
		}
	}
	return nil
}

func relativeDirectory(root, directory string) string {
	if directory == root {
		return ""
	}
	return strings.TrimPrefix(directory, root+"/")
}

func (l *Library) moveDirectoryVerified(ctx context.Context, features *fs.Features, source, destination string, expectedBytes int64) error {
	sourceSnapshot, err := l.snapshotDirectory(ctx, source)
	if err != nil {
		return fmt.Errorf("snapshot source directory: %w", err)
	}
	if sourceSnapshot.bytes != expectedBytes {
		return fmt.Errorf("source directory changed before staging: expected=%d actual=%d", expectedBytes, sourceSnapshot.bytes)
	}
	digest := sha256.Sum256([]byte(source + "\x00" + destination))
	tempRoot, err := safeMutationPath(path.Join(path.Dir(destination), ".rcfin-move-"+hex.EncodeToString(digest[:8])+".tmp"))
	if err != nil {
		return err
	}
	if _, err := l.vfs.Stat(tempRoot); IsNotFound(err) {
		if err := l.vfs.MkdirAll(tempRoot, 0o755); err != nil {
			return fmt.Errorf("create staged directory: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("check staged directory: %w", err)
	}
	for _, directory := range sourceSnapshot.dirs[1:] {
		relative := strings.TrimPrefix(directory, source+"/")
		if err := l.vfs.MkdirAll(path.Join(tempRoot, relative), 0o755); err != nil {
			return fmt.Errorf("create staged subdirectory %q: %w", relative, err)
		}
	}
	for relative, sourceObject := range sourceSnapshot.files {
		if err := ctx.Err(); err != nil {
			return err
		}
		tempPath := path.Join(tempRoot, relative)
		if _, err := l.vfs.Stat(path.Dir(tempPath)); IsNotFound(err) {
			if err := l.vfs.MkdirAll(path.Dir(tempPath), 0o755); err != nil {
				return fmt.Errorf("create staged parent for %q: %w", relative, err)
			}
		} else if err != nil {
			return fmt.Errorf("check staged parent for %q: %w", relative, err)
		}
		staged, stagedErr := l.backend.NewObject(ctx, tempPath)
		if stagedErr == nil {
			if err := verifyObjectContent(ctx, sourceObject, staged); err != nil {
				return fmt.Errorf("existing staged file %q does not verify: %w", relative, err)
			}
			continue
		}
		if !IsNotFound(stagedErr) {
			return fmt.Errorf("check staged file %q: %w", relative, stagedErr)
		}
		staged, err = features.Copy(ctx, sourceObject, tempPath)
		if err != nil {
			return fmt.Errorf("stage file %q: %w", relative, err)
		}
		if staged == nil {
			staged, err = l.backend.NewObject(ctx, tempPath)
			if err != nil {
				return fmt.Errorf("load staged file %q: %w", relative, err)
			}
		}
		if err := verifyObjectContent(ctx, sourceObject, staged); err != nil {
			return fmt.Errorf("staged file %q failed verification: %w", relative, err)
		}
	}
	if err := l.verifyDirectoryContent(ctx, source, tempRoot); err != nil {
		return fmt.Errorf("staged directory failed verification: %w", err)
	}
	if _, err := l.vfs.Stat(destination); err == nil {
		return fmt.Errorf("move destination appeared before promotion")
	} else if !IsNotFound(err) {
		return fmt.Errorf("recheck destination before promotion: %w", err)
	}
	if err := features.DirMove(ctx, l.backend, tempRoot, destination); err != nil {
		return fmt.Errorf("promote staged directory: %w", err)
	}
	l.vfs.FlushDirCache()
	if err := l.verifyDirectoryContent(ctx, source, destination); err != nil {
		return fmt.Errorf("promoted directory failed verification: %w", err)
	}
	return l.removeDirectoryTree(ctx, source, destination)
}

func (l *Library) removeDirectoryTree(ctx context.Context, root, verifiedDestination string) error {
	snapshot, err := l.snapshotDirectory(ctx, root)
	if err != nil {
		return fmt.Errorf("snapshot source before removal: %w", err)
	}
	if verifiedDestination != "" {
		destination, snapshotErr := l.snapshotDirectory(ctx, verifiedDestination)
		if snapshotErr != nil {
			return fmt.Errorf("snapshot verified destination before removal: %w", snapshotErr)
		}
		if err := verifyDirectorySnapshots(ctx, snapshot, destination, true); err != nil {
			return fmt.Errorf("reverify source before removal: %w", err)
		}
	}
	for relative := range snapshot.files {
		if err := ctx.Err(); err != nil {
			return err
		}
		object, objectErr := l.backend.NewObject(ctx, path.Join(root, relative))
		if objectErr != nil {
			return fmt.Errorf("reload source object %q before removal: %w", relative, objectErr)
		}
		if verifiedDestination != "" {
			destinationObject, destinationErr := l.backend.NewObject(ctx, path.Join(verifiedDestination, relative))
			if destinationErr != nil {
				return fmt.Errorf("reload verified destination object %q before source removal: %w", relative, destinationErr)
			}
			if err := verifyObjectContent(ctx, object, destinationObject); err != nil {
				return fmt.Errorf("source object %q changed before removal: %w", relative, err)
			}
		}
		if err := object.Remove(ctx); err != nil {
			return fmt.Errorf("remove source object %q: %w", object.Remote(), err)
		}
	}
	sort.Slice(snapshot.dirs, func(i, j int) bool { return len(snapshot.dirs[i]) > len(snapshot.dirs[j]) })
	for _, directory := range snapshot.dirs {
		if err := l.backend.Rmdir(ctx, directory); err != nil {
			return fmt.Errorf("remove source directory %q: %w", directory, err)
		}
	}
	l.vfs.FlushDirCache()
	if _, err := l.vfs.Stat(root); !IsNotFound(err) {
		if err == nil {
			return fmt.Errorf("source directory still exists after removal")
		}
		return fmt.Errorf("verify source directory removal: %w", err)
	}
	return nil
}

func (l *Library) moveFileVerified(ctx context.Context, features *fs.Features, source, destination string, expectedSize int64, recoverExisting bool) error {
	sourceObject, err := l.backend.NewObject(ctx, source)
	if err != nil {
		return fmt.Errorf("load move source object: %w", err)
	}
	if expectedSize < 0 || sourceObject.Size() < 0 {
		return fmt.Errorf("file move size verification unavailable for source")
	}
	if sourceObject.Size() != expectedSize {
		return fmt.Errorf("move source size changed before copy: expected=%d actual=%d", expectedSize, sourceObject.Size())
	}
	destinationObject, destinationErr := l.backend.NewObject(ctx, destination)
	if destinationErr == nil {
		if !recoverExisting {
			return fmt.Errorf("move destination appeared before copy promotion")
		}
		if err := verifyObjectContent(ctx, sourceObject, destinationObject); err != nil {
			return fmt.Errorf("existing destination does not verify against source: %w", err)
		}
		if err := sourceObject.Remove(ctx); err != nil {
			return fmt.Errorf("remove verified move source: %w", err)
		}
		l.vfs.FlushDirCache()
		if _, statErr := l.vfs.Stat(source); !IsNotFound(statErr) {
			if statErr == nil {
				return fmt.Errorf("verified destination exists but source removal did not complete")
			}
			return fmt.Errorf("verify removed move source: %w", statErr)
		}
		return nil
	}
	if !IsNotFound(destinationErr) {
		return fmt.Errorf("check move destination object: %w", destinationErr)
	}
	tempDigest := sha256.Sum256([]byte(source + "\x00" + destination))
	temp := path.Join(path.Dir(destination), ".rcfin-move-"+hex.EncodeToString(tempDigest[:8])+".tmp")
	temp, err = safeMutationPath(temp)
	if err != nil {
		return err
	}
	tempObject, tempErr := l.backend.NewObject(ctx, temp)
	if tempErr == nil {
		if err := verifyObjectContent(ctx, sourceObject, tempObject); err != nil {
			return fmt.Errorf("staged temporary object does not verify: %w", err)
		}
	} else if IsNotFound(tempErr) {
		tempObject, err = features.Copy(ctx, sourceObject, temp)
		if err != nil {
			return fmt.Errorf("stage verified file copy: %w", err)
		}
		if tempObject == nil {
			tempObject, err = l.backend.NewObject(ctx, temp)
			if err != nil {
				return fmt.Errorf("load staged temporary object: %w", err)
			}
		}
		if err := verifyObjectContent(ctx, sourceObject, tempObject); err != nil {
			return fmt.Errorf("staged temporary object failed verification: %w", err)
		}
	} else {
		return fmt.Errorf("check staged temporary object: %w", tempErr)
	}
	if _, err := l.backend.NewObject(ctx, destination); err == nil {
		return fmt.Errorf("move destination appeared before promotion")
	} else if !IsNotFound(err) {
		return fmt.Errorf("recheck move destination before promotion: %w", err)
	}
	if _, err := features.Move(ctx, tempObject, destination); err != nil {
		return fmt.Errorf("promote verified temporary file: %w", err)
	}
	finalObject, err := l.backend.NewObject(ctx, destination)
	if err != nil {
		return fmt.Errorf("load promoted destination for verification: %w", err)
	}
	if err := verifyObjectContent(ctx, sourceObject, finalObject); err != nil {
		return fmt.Errorf("promoted destination failed verification: %w", err)
	}
	if err := sourceObject.Remove(ctx); err != nil {
		return fmt.Errorf("remove source after destination verification: %w", err)
	}
	l.vfs.FlushDirCache()
	if _, err := l.vfs.Stat(source); !IsNotFound(err) {
		if err == nil {
			return fmt.Errorf("source still exists after verified destination promotion")
		}
		return fmt.Errorf("verify source removal after destination promotion: %w", err)
	}
	return nil
}

func verifyObjectContent(ctx context.Context, source, destination fs.Object) error {
	if source.Size() < 0 || destination.Size() < 0 {
		return fmt.Errorf("file move size verification unavailable")
	}
	if source.Size() != destination.Size() {
		return fmt.Errorf("object sizes differ: source=%d destination=%d", source.Size(), destination.Size())
	}
	hashType := source.Fs().Hashes().Overlap(destination.Fs().Hashes()).GetOne()
	if hashType == hash.None {
		return nil
	}
	sourceHash, err := source.Hash(ctx, hashType)
	if err != nil {
		return fmt.Errorf("read source %s checksum: %w", hashType, err)
	}
	destinationHash, err := destination.Hash(ctx, hashType)
	if err != nil {
		return fmt.Errorf("read destination %s checksum: %w", hashType, err)
	}
	if sourceHash != "" && destinationHash != "" && !strings.EqualFold(sourceHash, destinationHash) {
		return fmt.Errorf("%s checksum differs", hashType)
	}
	return nil
}

func safeMutationPath(name string) (string, error) {
	if strings.TrimSpace(name) == "" || path.IsAbs(name) || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") {
		return "", fmt.Errorf("mutation path %q must be relative", name)
	}
	clean := path.Clean(name)
	if clean != name || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("mutation path %q is not a safe library-relative path", name)
	}
	return clean, nil
}

// ReadDir lists one directory relative to the library root.
func (l *Library) ReadDir(name string) ([]os.FileInfo, error) {
	clean, err := relativePath(name)
	if err != nil {
		return nil, err
	}
	node, err := l.vfs.Stat(clean)
	if err != nil {
		return nil, err
	}
	dir, ok := node.(*vfs.Dir)
	if !ok {
		return nil, fmt.Errorf("%q is not a directory", name)
	}
	entries, err := dir.ReadDirAll()
	if err != nil {
		return nil, err
	}
	result := make([]os.FileInfo, 0, len(entries))
	for _, entry := range entries {
		result = append(result, entry)
	}
	return result, nil
}

// DirectorySize returns the byte total for files beneath a library-relative
// directory. It is used to verify same-library directory promotions.
func (l *Library) DirectorySize(ctx context.Context, name string) (int64, error) {
	if l == nil || l.vfs == nil {
		return 0, fmt.Errorf("library storage is unavailable")
	}
	clean, err := relativePath(name)
	if err != nil {
		return 0, err
	}
	var total int64
	var walk func(string) error
	walk = func(directory string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := l.ReadDir(directory)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			child := path.Join(directory, entry.Name())
			if entry.IsDir() {
				if err := walk(child); err != nil {
					return err
				}
				continue
			}
			if entry.Size() < 0 {
				return fmt.Errorf("size verification unavailable for %q", child)
			}
			if total > int64(^uint64(0)>>1)-entry.Size() {
				return fmt.Errorf("directory size overflows int64")
			}
			total += entry.Size()
		}
		return nil
	}
	if err := walk(clean); err != nil {
		return 0, err
	}
	return total, nil
}

// ContentChecksum returns backend-provided checksum evidence for a file or
// directory. An empty checksum means that the backend does not expose a
// checksum for every object in the requested tree; file contents are never
// downloaded solely to manufacture recovery evidence.
func (l *Library) ContentChecksum(ctx context.Context, name string) (string, error) {
	if l == nil || l.vfs == nil || l.backend == nil {
		return "", fmt.Errorf("library storage is unavailable")
	}
	clean, err := safeMutationPath(name)
	if err != nil {
		return "", err
	}
	info, err := l.vfs.Stat(clean)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		object, err := l.backend.NewObject(ctx, clean)
		if err != nil {
			return "", err
		}
		return backendObjectChecksum(ctx, object)
	}

	snapshot, err := l.snapshotDirectory(ctx, clean)
	if err != nil {
		return "", err
	}
	paths := make([]string, 0, len(snapshot.files))
	for relative := range snapshot.files {
		paths = append(paths, relative)
	}
	sort.Strings(paths)
	contents := sha256.New()
	_, _ = io.WriteString(contents, "rcfin-directory-checksum-v2\x00")
	directories := make([]string, 0, len(snapshot.dirs))
	for _, directory := range snapshot.dirs {
		directories = append(directories, relativeDirectory(clean, directory))
	}
	sort.Strings(directories)
	for _, relative := range directories {
		_, _ = contents.Write([]byte{'D'})
		_, _ = io.WriteString(contents, relative)
		_, _ = contents.Write([]byte{0})
	}
	for _, relative := range paths {
		object := snapshot.files[relative]
		checksum, err := backendObjectChecksum(ctx, object)
		if err != nil {
			return "", fmt.Errorf("checksum directory file %q: %w", relative, err)
		}
		if checksum == "" {
			return "", nil
		}
		_, _ = contents.Write([]byte{'F'})
		_, _ = io.WriteString(contents, relative)
		_, _ = contents.Write([]byte{0})
		_, _ = io.WriteString(contents, strconv.FormatInt(object.Size(), 10))
		_, _ = contents.Write([]byte{0})
		_, _ = io.WriteString(contents, checksum)
		_, _ = contents.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(contents.Sum(nil)), nil
}

func backendObjectChecksum(ctx context.Context, object fs.Object) (string, error) {
	if object == nil {
		return "", fmt.Errorf("checksum object is unavailable")
	}
	kind := object.Fs().Hashes().GetOne()
	if kind == hash.None {
		return "", nil
	}
	value, err := object.Hash(ctx, kind)
	if err != nil {
		return "", err
	}
	if value == "" {
		return "", nil
	}
	return kind.String() + ":" + strings.ToLower(value), nil
}

// VFSOptions builds the VFS configuration for one library. It starts from
// RCFin's media-serving defaults — chosen for how Jellyfin clients actually
// behave — and applies any explicitly configured rclone VFS options on top.
// Keys are rclone's config names (for example "dir_cache_time",
// "vfs_cache_mode") and values use rclone's value syntax. Unknown keys and
// invalid values are rejected at startup instead of silently ignored.
//
// The defaults assume clients browse directories, stream original files with
// occasional seeks, and fetch small artwork files:
//
//   - read_only: the server never writes to media storage.
//   - dir_cache_time (30m): directory listings are cached aggressively for
//     cheap browsing; the scanner flushes the cache before every rescan so
//     listings are never stale.
//   - poll_interval (0): no background change polling. Polling is unsupported
//     on many remotes and would just burn API calls; freshness comes from
//     the rescan interval plus the manual Library/Refresh endpoint.
//   - vfs_cache_mode (minimal): files stream straight from the remote
//     without a full download first, so playback starts fast and the local
//     disk footprint stays tiny. Switch a library to "full" for seek-heavy
//     playback over high-latency links, together with a sized cache dir.
//   - vfs_read_chunk_size (32M) growing to vfs_read_chunk_size_limit (256M):
//     a small first chunk keeps stream start latency low, then chunks grow
//     for sequential playback throughput while keeping seeks bounded.
//   - vfs_handle_caching (1m): keeps download workers warm across the quick
//     reopen pattern of artwork fetches and retried range requests.
//   - no_seek (false): seeking must stay enabled for HTTP range requests.
//
// Every other option mirrors rclone's own built-in default.
func VFSOptions(values map[string]any) (vfscommon.Options, error) {
	// Set every field explicitly: without rclone's CLI flag parsing the
	// package-global defaults stay at their zero values, so inheriting them
	// would silently misconfigure the VFS (for example a zero chunk size).
	options := vfscommon.Options{
		NoSeek:             false,
		NoChecksum:         false,
		ReadOnly:           true,
		Links:              false,
		NoModTime:          false,
		DirCacheTime:       fs.Duration(30 * time.Minute),
		Refresh:            false,
		PollInterval:       0,
		Umask:              vfscommon.FileMode(0o022),
		UID:                uint32(os.Getuid()),
		GID:                uint32(os.Getgid()),
		DirPerms:           vfscommon.FileMode(0o777),
		FilePerms:          vfscommon.FileMode(0o666),
		LinkPerms:          vfscommon.FileMode(0o666),
		ChunkSize:          32 * fs.Mebi,
		ChunkSizeLimit:     256 * fs.Mebi,
		ChunkStreams:       0,
		CacheMode:          vfscommon.CacheModeMinimal,
		CacheMaxAge:        fs.Duration(time.Hour),
		CacheMaxSize:       fs.SizeSuffix(-1),
		CacheMinFreeSpace:  fs.SizeSuffix(-1),
		CachePollInterval:  fs.Duration(60 * time.Second),
		CaseInsensitive:    false,
		BlockNormDupes:     false,
		WriteWait:          fs.Duration(time.Second),
		ReadWait:           fs.Duration(20 * time.Millisecond),
		WriteBack:          fs.Duration(5 * time.Second),
		ReadAhead:          0,
		UsedIsSize:         false,
		FastFingerprint:    false,
		DiskSpaceTotalSize: fs.SizeSuffix(-1),
		HandleCaching:      fs.Duration(time.Minute),
		MetadataExtension:  "",
	}

	fields := make(map[string]int)
	t := reflect.TypeOf(options)
	for i := 0; i < t.NumField(); i++ {
		if key := t.Field(i).Tag.Get("config"); key != "" {
			fields[key] = i
		}
	}

	for key, raw := range values {
		index, ok := fields[key]
		if !ok {
			return options, fmt.Errorf("unknown VFS option %q", key)
		}
		value := strings.TrimSpace(fmt.Sprint(raw))
		field := reflect.ValueOf(&options).Elem().Field(index)
		if setter, ok := field.Addr().Interface().(interface{ Set(string) error }); ok {
			if err := setter.Set(value); err != nil {
				return options, fmt.Errorf("%s: %w", key, err)
			}
			continue
		}
		if err := setScalar(field, value); err != nil {
			return options, fmt.Errorf("%s: %w", key, err)
		}
	}
	return options, nil
}

// setScalar parses plain bool/int/uint/string option values. Types with
// rclone value syntax (durations, sizes, enums, file modes) implement
// Set(string) and are handled by the caller.
func setScalar(field reflect.Value, value string) error {
	switch field.Kind() {
	case reflect.Bool:
		parsed, err := strconv.ParseBool(value)
		if err == nil {
			field.SetBool(parsed)
		}
		return err
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		parsed, err := strconv.ParseInt(value, 0, field.Type().Bits())
		if err == nil {
			field.SetInt(parsed)
		}
		return err
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		parsed, err := strconv.ParseUint(value, 0, field.Type().Bits())
		if err == nil {
			field.SetUint(parsed)
		}
		return err
	case reflect.String:
		field.SetString(value)
		return nil
	default:
		return fmt.Errorf("unsupported value type %s", field.Type())
	}
}

func relativePath(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == "/" {
		return "", nil
	}
	name = strings.TrimLeft(name, "/")
	clean := path.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("path %q escapes library root", name)
	}
	return strings.TrimPrefix(clean, "./"), nil
}
