// OpenDir is the collection scanner's read-only directory interface over an
// rclone-backed library.
package collection

import (
	"errors"
	"os"
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
