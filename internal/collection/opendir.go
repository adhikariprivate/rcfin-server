// OpenDir is the collection scanner's read-only directory interface over an
// rclone-backed library.
package collection

import (
	"errors"
	"os"
	"path"
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

// InventoryDirectoryIndex provides sorted directory views from one inventory
// snapshot, avoiding repeated remote listings and repeated full-snapshot scans.
type InventoryDirectoryIndex struct {
	children map[string]map[string]FileInfo
}

func NewInventoryDirectoryIndex(inventory []InventoryEntry) *InventoryDirectoryIndex {
	index := &InventoryDirectoryIndex{children: map[string]map[string]FileInfo{".": {}}}
	add := func(parent string, child FileInfo) {
		if index.children[parent] == nil {
			index.children[parent] = map[string]FileInfo{}
		}
		if existing, ok := index.children[parent][child.Name()]; ok && existing.IsDir() && !child.IsDir() {
			return
		}
		index.children[parent][child.Name()] = child
	}
	for _, entry := range inventory {
		clean := path.Clean(strings.TrimSpace(entry.Path))
		if clean == "" || clean == "." || path.IsAbs(clean) || strings.HasPrefix(clean, "../") {
			continue
		}
		parent, base := path.Dir(clean), path.Base(clean)
		add(parent, FileInfo{info: inventoryFileInfo{name: base, size: entry.Size, modTime: time.Unix(0, entry.ModTime), isDir: entry.IsDir}})
		for ancestor := parent; ancestor != "." && ancestor != "/"; ancestor = path.Dir(ancestor) {
			ancestorParent, ancestorName := path.Dir(ancestor), path.Base(ancestor)
			add(ancestorParent, FileInfo{info: inventoryFileInfo{name: ancestorName, isDir: true}})
		}
	}
	return index
}

func (index *InventoryDirectoryIndex) OpenDir(name string) *Dir {
	clean := path.Clean(strings.Trim(strings.TrimSpace(name), "/"))
	if clean == "" {
		clean = "."
	}
	children := index.children[clean]
	names := make([]string, 0, len(children))
	for child := range children {
		names = append(names, child)
	}
	sort.Strings(names)
	entries := make([]FileInfo, 0, len(names))
	for _, child := range names {
		entries = append(entries, children[child])
	}
	return &Dir{name: clean, entries: entries, read: true}
}

// OpenInventoryDir is a convenience for callers that need one view. Library
// model rebuilds should construct and reuse an InventoryDirectoryIndex.
func OpenInventoryDir(name string, inventory []InventoryEntry) *Dir {
	return NewInventoryDirectoryIndex(inventory).OpenDir(name)
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
