// OpenDir is the collection scanner's read-only directory interface over an
// rclone-backed library.
package collection

import (
	"errors"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/adhikariprivate/rcfin-server/internal/rclonestore"
)

type Dir struct {
	store   *rclonestore.Library
	name    string
	entries []FileInfo
	read    bool
	index   int
}

type FileInfo struct {
	info os.FileInfo
}

var ErrNotDirectory = errors.New("not a directory")

func OpenDir(store *rclonestore.Library, name string) (*Dir, error) {
	if store == nil {
		return nil, errors.New("collection has no rclone filesystem")
	}
	return &Dir{store: store, name: name}, nil
}

// OpenInventoryDir builds an in-memory directory view from the completed
// library snapshot. It avoids a second Rclone listing while the Jellyfin model
// is rebuilt after an inventory cycle.
func OpenInventoryDir(name string, inventory []InventoryEntry) *Dir {
	children := map[string]FileInfo{}
	prefix := strings.Trim(strings.TrimSpace(name), "/")
	if prefix != "" {
		prefix += "/"
	}
	for _, entry := range inventory {
		if !strings.HasPrefix(entry.Path, prefix) {
			continue
		}
		relative := strings.TrimPrefix(entry.Path, prefix)
		if relative == "" {
			continue
		}
		first, _, hasNested := strings.Cut(relative, "/")
		if hasNested {
			if _, exists := children[first]; !exists {
				children[first] = FileInfo{info: inventoryFileInfo{name: first, isDir: true}}
			}
			continue
		}
		children[first] = FileInfo{info: inventoryFileInfo{name: first, size: entry.Size, modTime: time.Unix(0, entry.ModTime), isDir: entry.IsDir}}
	}
	names := make([]string, 0, len(children))
	for child := range children {
		names = append(names, child)
	}
	sort.Strings(names)
	entries := make([]FileInfo, 0, len(names))
	for _, child := range names {
		entries = append(entries, children[child])
	}
	return &Dir{name: name, entries: entries, read: true}
}

type inventoryFileInfo struct {
	name    string
	size    int64
	modTime time.Time
	isDir   bool
}

func (i inventoryFileInfo) Name() string       { return i.name }
func (i inventoryFileInfo) Size() int64        { return i.size }
func (i inventoryFileInfo) ModTime() time.Time { return i.modTime }
func (i inventoryFileInfo) IsDir() bool        { return i.isDir }
func (i inventoryFileInfo) Sys() any           { return nil }
func (i inventoryFileInfo) Mode() os.FileMode {
	if i.isDir {
		return os.ModeDir | 0o755
	}
	return 0o644
}

func (dir *Dir) Close() error { return nil }

func (dir *Dir) Stat() (os.FileInfo, error) {
	return dir.store.Stat(dir.name)
}

func (dir *Dir) load() error {
	if dir.read {
		return nil
	}
	dir.read = true
	entries, err := dir.store.ReadDir(dir.name)
	if err != nil {
		return err
	}
	dir.entries = make([]FileInfo, len(entries))
	for i, entry := range entries {
		dir.entries[i] = FileInfo{info: entry}
	}
	return nil
}

func (dir *Dir) Readdirnames(n int) ([]string, error) {
	if err := dir.load(); err != nil {
		return nil, err
	}
	end := len(dir.entries)
	if n > 0 && end-dir.index > n {
		end = dir.index + n
	}
	names := make([]string, 0, end-dir.index)
	for _, entry := range dir.entries[dir.index:end] {
		names = append(names, entry.Name())
	}
	dir.index = end
	return names, nil
}

func (dir *Dir) Readdir(n int) ([]FileInfo, error) {
	if err := dir.load(); err != nil {
		return nil, err
	}
	end := len(dir.entries)
	if n > 0 && end-dir.index > n {
		end = dir.index + n
	}
	entries := append([]FileInfo(nil), dir.entries[dir.index:end]...)
	dir.index = end
	return entries, nil
}

func (fi FileInfo) Name() string       { return fi.info.Name() }
func (fi FileInfo) Size() int64        { return fi.info.Size() }
func (fi FileInfo) Mode() os.FileMode  { return fi.info.Mode() }
func (fi FileInfo) Modtime() time.Time { return fi.info.ModTime() }
func (fi FileInfo) Createtime() time.Time {
	// Object stores do not expose a portable creation timestamp. Use the
	// modification timestamp for the library's date-derived metadata.
	return fi.info.ModTime()
}
func (fi FileInfo) CreatetimeMS() int64 { return fi.Createtime().UnixMilli() }
func (fi FileInfo) IsDir() bool         { return fi.info.IsDir() }
