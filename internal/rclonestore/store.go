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
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"reflect"
	"strconv"
	"strings"
	"time"

	_ "github.com/rclone/rclone/backend/all"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configfile"
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
	remote string
	vfs    *vfs.VFS
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
	return &Library{remote: remote, vfs: v}, nil
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
