// Package webdav exposes RCFin's catalog as a read-only virtual WebDAV tree.
package webdav

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/net/webdav"

	"github.com/adhikariprivate/rcfin-server/internal/collection"
	"github.com/adhikariprivate/rcfin-server/internal/database"
	"github.com/adhikariprivate/rcfin-server/internal/database/model"
)

type LibraryConfig struct {
	Enabled     bool
	Views       []string
	Labels      map[string]string
	FileName    string
	TitleFolder string
	EpisodeName string
	ShowNFO     bool
	ShowArtwork bool
}

type Options struct {
	Path        string
	Collections *collection.CollectionRepo
	Repo        database.Repository
	Libraries   map[string]LibraryConfig
	CacheTTL    time.Duration
}

type requestKey struct{}

type requestTree struct {
	nodes         map[string]*node
	locations     map[string]map[string]model.LibraryInventoryEntry
	itemLocations map[string]map[string]string
}

type node struct {
	name     string
	dir      bool
	size     int64
	modTime  time.Time
	data     []byte
	open     func() (http.File, error)
	children map[string]*node
}

type Server struct {
	collections *collection.CollectionRepo
	repo        database.Repository
	libraries   map[string]LibraryConfig
	cacheTTL    time.Duration
	cacheMu     sync.Mutex
	cache       map[string]cachedTree
}

type cachedTree struct {
	version uint64
	expires time.Time
	tree    *requestTree
}

func New(options Options) http.Handler {
	base := strings.TrimSpace(options.Path)
	if base == "" {
		base = "/dav/"
	}
	if !strings.HasPrefix(base, "/") {
		base = "/" + base
	}
	base = path.Clean(base)
	if base == "." {
		base = "/dav"
	}
	if options.CacheTTL <= 0 {
		options.CacheTTL = time.Minute
	}
	s := &Server{collections: options.Collections, repo: options.Repo, libraries: options.Libraries, cacheTTL: options.CacheTTL, cache: map[string]cachedTree{}}
	h := &webdav.Handler{Prefix: base, FileSystem: readOnlyFS{}, LockSystem: webdav.NewMemLS()}
	return s.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "OPTIONS" {
			w.Header().Set("Allow", "OPTIONS, PROPFIND, GET, HEAD")
			w.Header().Set("DAV", "1")
			w.Header().Set("MS-Author-Via", "DAV")
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method != "PROPFIND" && r.Method != "GET" && r.Method != "HEAD" {
			w.Header().Set("Allow", "OPTIONS, PROPFIND, GET, HEAD")
			http.Error(w, "read-only WebDAV", http.StatusForbidden)
			return
		}
		tree, err := s.buildTree(r.Context(), r.Context().Value(requestKey{}).(*model.User))
		if err != nil {
			http.Error(w, "could not build library view", http.StatusInternalServerError)
			return
		}
		ctx := context.WithValue(r.Context(), requestKey{}, tree)
		h.ServeHTTP(w, r.WithContext(ctx))
	}))
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		if !ok || username == "" || password == "" {
			challenge(w)
			return
		}
		user, err := s.repo.GetUser(r.Context(), strings.ToLower(username))
		if err != nil || user == nil || user.Properties.Disabled || bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(password)) != nil {
			challenge(w)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestKey{}, user)))
	})
}

func challenge(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="RCFin WebDAV", charset="UTF-8"`)
	http.Error(w, "authentication required", http.StatusUnauthorized)
}

func (s *Server) buildTree(ctx context.Context, user *model.User) (*requestTree, error) {
	key := user.ID + "|"
	grants := append([]string(nil), user.Properties.EnabledFolders...)
	sort.Strings(grants)
	key += fmt.Sprint(user.Properties.Admin, ":", grants)
	version := s.collections.CatalogVersion()
	s.cacheMu.Lock()
	if cached, ok := s.cache[key]; ok && cached.version == version && time.Now().Before(cached.expires) {
		s.cacheMu.Unlock()
		return cached.tree, nil
	}
	s.cacheMu.Unlock()
	t := &requestTree{nodes: map[string]*node{"/": {name: "/", dir: true, modTime: time.Now().UTC(), children: map[string]*node{}}}, locations: map[string]map[string]model.LibraryInventoryEntry{}, itemLocations: map[string]map[string]string{}}
	for _, c := range s.collections.SnapshotCollections() {
		cfg := s.libraries[c.ID]
		if !cfg.Enabled || !canAccess(user, c.ID) {
			continue
		}
		if s.repo != nil {
			snapshot, err := s.repo.GetLibraryInventory(ctx, c.ID)
			if err != nil {
				return nil, fmt.Errorf("load inventory for %q: %w", c.ID, err)
			}
			if snapshot != nil {
				t.locations[c.ID] = make(map[string]model.LibraryInventoryEntry, len(snapshot.Entries))
				t.itemLocations[c.ID] = map[string]string{}
				for _, entry := range snapshot.Entries {
					t.locations[c.ID][entry.Path] = entry
					if entry.IsDir && entry.Scope == "library" && entry.JellyfinID != "" && entry.MissingSince == nil {
						t.itemLocations[c.ID][entry.JellyfinID] = entry.Path
					}
				}
			}
		}
		root := t.ensureDir(t.nodes["/"], safeName(c.Name))
		views := cfg.Views
		if len(views) == 0 {
			views = []string{"all", "genres", "actors", "directors", "years"}
		}
		for _, rawView := range views {
			view := strings.ToLower(strings.TrimSpace(rawView))
			if !validView(view) {
				continue
			}
			viewName := map[string]string{"all": "All", "genres": "Genres", "actors": "Actors", "directors": "Directors", "years": "Years", "studios": "Studios"}[view]
			if label := strings.TrimSpace(cfg.Labels[view]); label != "" {
				viewName = safeName(label)
			}
			viewDir := t.ensureDir(root, viewName)
			for _, item := range c.Items {
				if item == nil {
					continue
				}
				if show, ok := item.(*collection.Show); ok {
					s.addShow(t, c, cfg, view, viewDir, show)
				} else {
					s.addItem(t, c, cfg, view, viewDir, item, item.Path(), item.FileName())
				}
			}
		}
	}
	var index func(*node, string)
	index = func(n *node, current string) {
		t.nodes[current] = n
		for name, child := range n.children {
			childPath := path.Join(current, name)
			if current == "/" {
				childPath = "/" + name
			}
			index(child, childPath)
		}
	}
	index(t.nodes["/"], "/")
	s.cacheMu.Lock()
	s.cache[key] = cachedTree{version: version, expires: time.Now().Add(s.cacheTTL), tree: t}
	s.cacheMu.Unlock()
	return t, ctx.Err()
}

func validView(v string) bool {
	return v == "all" || v == "genres" || v == "actors" || v == "directors" || v == "years" || v == "studios"
}

func canAccess(user *model.User, id string) bool {
	if user == nil || user.Properties.Disabled {
		return false
	}
	if user.Properties.Admin {
		return true
	}
	for _, grant := range user.Properties.EnabledFolders {
		if grant == id {
			return true
		}
	}
	return false
}

func (s *Server) addShow(t *requestTree, c collection.Collection, cfg LibraryConfig, view string, parent *node, show *collection.Show) {
	showName := itemTitle(show)
	if view == "all" {
		dir := t.ensureDir(parent, showName)
		s.addShowSidecars(t, c, cfg, dir, show)
		for _, season := range show.Seasons {
			seasonDir := t.ensureDir(dir, season.Name())
			s.addSeasonSidecars(t, c, cfg, seasonDir, &season)
			for _, ep := range season.Episodes {
				s.addItem(t, c, cfg, view, seasonDir, &ep, ep.Path(), ep.FileName())
			}
		}
		return
	}
	for _, value := range viewValues(view, show) {
		d := t.ensureDir(parent, value)
		showDir := t.ensureDir(d, showName)
		s.addShowSidecars(t, c, cfg, showDir, show)
		for _, season := range show.Seasons {
			seasonDir := t.ensureDir(showDir, season.Name())
			s.addSeasonSidecars(t, c, cfg, seasonDir, &season)
			for _, ep := range season.Episodes {
				s.addItem(t, c, cfg, view, seasonDir, &ep, ep.Path(), ep.FileName())
			}
		}
	}
}

func (s *Server) addItem(t *requestTree, c collection.Collection, cfg LibraryConfig, view string, parent *node, item collection.Item, sourceDir, sourceFile string) {
	if sourceFile == "" || c.Storage == nil {
		return
	}
	values := []string{""}
	if view != "all" {
		values = viewValues(view, item)
	}
	for _, value := range values {
		base := parent
		if value != "" {
			base = t.ensureDir(base, value)
		}
		folderName := itemTitle(item)
		if cfg.TitleFolder != "" {
			folderName = formatName(cfg.TitleFolder, item, sourceFile)
		}
		dir := t.ensureDir(base, folderName)
		filename := sourceFile
		fileFormat := cfg.FileName
		if _, ok := item.(*collection.Episode); ok && cfg.EpisodeName != "" {
			fileFormat = cfg.EpisodeName
		}
		if fileFormat != "" {
			filename = formatName(fileFormat, item, sourceFile)
		}
		if _, ok := item.(*collection.Movie); ok {
			if location := t.itemLocations[c.ID][item.ID()]; location != "" {
				sourceDir = location
			}
		}
		source := path.Join(sourceDir, sourceFile)
		size := item.FileSize()
		if inventory := t.locations[c.ID]; inventory != nil {
			entry, found := inventory[source]
			if !found || entry.Scope != "library" || entry.Role != "video" || entry.IsDir || entry.MissingSince != nil {
				return
			}
			source, size = entry.Path, entry.Size
		}
		video := &node{name: filename, size: size, modTime: time.Now().UTC(), open: func() (http.File, error) { return c.Storage.OpenFile(source) }}
		t.add(dir, video)
		if !cfg.ShowNFO && !cfg.ShowArtwork {
			continue
		}
		if cfg.ShowNFO {
			nfoName := "movie.nfo"
			if _, ok := item.(*collection.Episode); ok {
				nfoName = strings.TrimSuffix(filename, path.Ext(filename)) + ".nfo"
			}
			nfoPath := path.Join(sourceDir, strings.TrimSuffix(sourceFile, path.Ext(sourceFile))+".nfo")
			if _, ok := item.(*collection.Movie); ok {
				nfoPath = path.Join(sourceDir, "movie.nfo")
			}
			if _, ok := item.(*collection.Episode); ok {
				nfoPath = path.Join(sourceDir, strings.TrimSuffix(sourceFile, path.Ext(sourceFile))+".nfo")
			}
			nfo := &node{name: nfoName, modTime: time.Now().UTC(), open: func() (http.File, error) {
				if f, e := c.Storage.OpenFile(nfoPath); e == nil {
					return f, nil
				}
				return memoryFile(nfoName, marshalNFO(item)), nil
			}}
			t.add(dir, nfo)
		}
		if cfg.ShowArtwork {
			poster, fanart := artworkPaths(item)
			if poster != "" {
				src := path.Join(sourceDir, poster)
				t.add(dir, &node{name: "poster" + safeExt(poster, ".jpg"), modTime: time.Now().UTC(), open: func() (http.File, error) { return c.Storage.OpenFile(src) }})
			}
			if fanart != "" {
				src := path.Join(sourceDir, fanart)
				t.add(dir, &node{name: "fanart" + safeExt(fanart, ".jpg"), modTime: time.Now().UTC(), open: func() (http.File, error) { return c.Storage.OpenFile(src) }})
			}
		}
	}
}

func (s *Server) addShowSidecars(t *requestTree, c collection.Collection, cfg LibraryConfig, dir *node, show *collection.Show) {
	if cfg.ShowNFO {
		nfoPath := path.Join(show.Path(), "tvshow.nfo")
		t.add(dir, &node{name: "tvshow.nfo", modTime: time.Now().UTC(), open: func() (http.File, error) {
			if f, err := c.Storage.OpenFile(nfoPath); err == nil {
				return f, nil
			}
			return memoryFile("tvshow.nfo", marshalNFO(show)), nil
		}})
	}
	if cfg.ShowArtwork && show.Poster() != "" {
		p := path.Join(show.Path(), show.Poster())
		t.add(dir, &node{name: "poster" + safeExt(show.Poster(), ".jpg"), modTime: time.Now().UTC(), open: func() (http.File, error) { return c.Storage.OpenFile(p) }})
	}
}

func (s *Server) addSeasonSidecars(t *requestTree, c collection.Collection, cfg LibraryConfig, dir *node, season *collection.Season) {
	if cfg.ShowNFO {
		nfoPath := path.Join(season.Path(), season.Name(), "season.nfo")
		t.add(dir, &node{name: "season.nfo", modTime: time.Now().UTC(), open: func() (http.File, error) {
			if f, err := c.Storage.OpenFile(nfoPath); err == nil {
				return f, nil
			}
			return memoryFile("season.nfo", marshalNFO(season)), nil
		}})
	}
	if cfg.ShowArtwork && season.Poster() != "" {
		p := season.Poster()
		if !strings.Contains(p, "/") {
			p = path.Join(season.Path(), season.Name(), p)
		}
		t.add(dir, &node{name: "poster" + safeExt(p, ".jpg"), modTime: time.Now().UTC(), open: func() (http.File, error) { return c.Storage.OpenFile(p) }})
	}
}

func viewValues(view string, item collection.Item) []string {
	var values []string
	switch view {
	case "genres":
		values = item.Genres()
	case "actors":
		for actor := range item.Actors() {
			values = append(values, actor)
		}
	case "directors":
		values = item.Directors()
	case "studios":
		values = item.Studios()
	case "years":
		if item.Year() > 0 {
			values = []string{fmt.Sprint(item.Year())}
		}
	}
	values = uniqueSorted(values)
	return values
}

func uniqueSorted(values []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}
func itemTitle(i collection.Item) string {
	title := strings.TrimSpace(i.Name())
	if title == "" {
		title = strings.TrimSpace(i.Title())
	}
	if title == "" {
		title = i.FileName()
	}
	return safeName(title)
}
func formatName(tpl string, item collection.Item, original string) string {
	title := strings.TrimSpace(item.Title())
	if title == "" {
		title = item.Name()
	}
	resolution := ""
	if item.VideoHeight() > 0 {
		resolution = fmt.Sprint(item.VideoHeight())
	}
	season, episode, series, episodeTitle := "", "", "", title
	if e, ok := item.(*collection.Episode); ok {
		season = fmt.Sprintf("%02d", e.SeasonNo)
		episode = fmt.Sprintf("%02d", e.EpisodeNo)
		series = path.Base(strings.TrimSuffix(e.Path(), "/"))
		if episodeTitle == "" {
			episodeTitle = e.Name()
		}
	}
	repl := strings.NewReplacer("{episode_title}", episodeTitle, "{series}", series, "{season:02}", season, "{episode:02}", episode, "{title}", title, "{name}", item.Name(), "{year}", yearString(item.Year()), "{ext}", strings.TrimPrefix(path.Ext(original), "."), "{resolution}", resolution)
	return safeName(repl.Replace(tpl))
}
func yearString(y int) string {
	if y <= 0 {
		return ""
	}
	return fmt.Sprint(y)
}
func safeName(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "/", "_"), "\\", "_"))
	if s == "" || s == "." || s == ".." {
		return "Untitled"
	}
	return s
}
func safeExt(s, fallback string) string {
	ext := strings.ToLower(path.Ext(s))
	if ext == "" {
		return fallback
	}
	return ext
}
func artworkPaths(i collection.Item) (string, string) {
	switch v := i.(type) {
	case *collection.Movie:
		return v.Poster(), v.Fanart()
	case *collection.Show:
		return v.Poster(), v.Fanart()
	case *collection.Season:
		return v.Poster(), v.Fanart()
	case *collection.Episode:
		return v.Poster(), ""
	}
	return "", ""
}

func (t *requestTree) ensureDir(parent *node, name string) *node {
	name = safeName(name)
	if n := parent.children[name]; n != nil && n.dir {
		return n
	}
	n := &node{name: name, dir: true, modTime: time.Now().UTC(), children: map[string]*node{}}
	if parent.children == nil {
		parent.children = map[string]*node{}
	}
	parent.children[name] = n
	return n
}
func (t *requestTree) add(parent *node, child *node) {
	if parent.children == nil {
		parent.children = map[string]*node{}
	}
	name := safeName(child.name)
	if existing := parent.children[name]; existing != nil {
		if existing.dir {
			return
		}
		ext := path.Ext(name)
		name = strings.TrimSuffix(name, ext) + "-" + strings.ToLower(fmt.Sprintf("%x", len(parent.children))) + ext
		child.name = name
	}
	parent.children[name] = child
}

type readOnlyFS struct{}

func tree(ctx context.Context) (*requestTree, error) {
	t, ok := ctx.Value(requestKey{}).(*requestTree)
	if !ok {
		return nil, os.ErrNotExist
	}
	return t, nil
}
func (readOnlyFS) Mkdir(context.Context, string, os.FileMode) error { return os.ErrPermission }
func (readOnlyFS) RemoveAll(context.Context, string) error          { return os.ErrPermission }
func (readOnlyFS) Rename(context.Context, string, string) error     { return os.ErrPermission }
func (readOnlyFS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	t, e := tree(ctx)
	if e != nil {
		return nil, e
	}
	n := t.nodes[cleanURLPath(name)]
	if n == nil {
		return nil, os.ErrNotExist
	}
	if !n.dir && n.size == 0 && n.open != nil {
		f, err := n.open()
		if err != nil {
			return nil, err
		}
		defer f.Close()
		info, statErr := f.Stat()
		if statErr != nil {
			return nil, statErr
		}
		return namedInfo{FileInfo: info, name: n.name}, nil
	}
	return nodeInfo{n}, nil
}
func (readOnlyFS) OpenFile(ctx context.Context, name string, flag int, _ os.FileMode) (webdav.File, error) {
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND) != 0 {
		return nil, os.ErrPermission
	}
	t, e := tree(ctx)
	if e != nil {
		return nil, e
	}
	n := t.nodes[cleanURLPath(name)]
	if n == nil {
		return nil, os.ErrNotExist
	}
	if n.dir {
		return &dirFile{info: nodeInfo{n}, children: childInfos(n)}, nil
	}
	if n.open != nil {
		f, e := n.open()
		if e != nil {
			return nil, e
		}
		return readFile{File: f}, nil
	}
	return memoryFile(n.name, n.data), nil
}
func cleanURLPath(name string) string { p := path.Clean("/" + strings.TrimPrefix(name, "/")); return p }

type nodeInfo struct{ n *node }

type namedInfo struct {
	os.FileInfo
	name string
}

func (i namedInfo) Name() string { return i.name }

func (i nodeInfo) Name() string { return i.n.name }
func (i nodeInfo) Size() int64 {
	if i.n.dir {
		return 0
	}
	if i.n.data != nil {
		return int64(len(i.n.data))
	}
	return i.n.size
}
func (i nodeInfo) Mode() os.FileMode {
	if i.n.dir {
		return os.ModeDir | 0555
	}
	return 0444
}
func (i nodeInfo) ModTime() time.Time { return i.n.modTime }
func (i nodeInfo) IsDir() bool        { return i.n.dir }
func (i nodeInfo) Sys() any           { return nil }

type dirFile struct {
	info     os.FileInfo
	children []os.FileInfo
	read     bool
}

func (f *dirFile) Close() error                   { return nil }
func (f *dirFile) Write([]byte) (int, error)      { return 0, os.ErrPermission }
func (f *dirFile) Read([]byte) (int, error)       { return 0, errors.New("is a directory") }
func (f *dirFile) Seek(int64, int) (int64, error) { return 0, errors.New("is a directory") }
func (f *dirFile) Stat() (os.FileInfo, error)     { return f.info, nil }
func (f *dirFile) Readdir(count int) ([]os.FileInfo, error) {
	if f.read {
		return nil, io.EOF
	}
	f.read = true
	if count > 0 && len(f.children) > count {
		return f.children[:count], nil
	}
	return f.children, nil
}
func childInfos(n *node) []os.FileInfo {
	out := make([]os.FileInfo, 0, len(n.children))
	for _, c := range n.children {
		out = append(out, nodeInfo{c})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

type memFile struct {
	*bytes.Reader
	info os.FileInfo
}

func memoryFile(name string, data []byte) *memFile {
	return &memFile{Reader: bytes.NewReader(data), info: nodeInfo{&node{name: name, data: data, modTime: time.Now().UTC()}}}
}
func (f *memFile) Close() error                       { return nil }
func (f *memFile) Stat() (os.FileInfo, error)         { return f.info, nil }
func (f *memFile) Readdir(int) ([]os.FileInfo, error) { return nil, errors.New("not a directory") }
func (f *memFile) Write([]byte) (int, error)          { return 0, os.ErrPermission }

type readFile struct{ http.File }

func (f readFile) Write([]byte) (int, error) { return 0, os.ErrPermission }

type nfoRecord struct {
	XMLName       xml.Name
	Title         string     `xml:"title"`
	OriginalTitle string     `xml:"originaltitle,omitempty"`
	Year          int        `xml:"year,omitempty"`
	Plot          string     `xml:"plot,omitempty"`
	Season        int        `xml:"season,omitempty"`
	Episode       int        `xml:"episode,omitempty"`
	Rating        float32    `xml:"rating,omitempty"`
	Genres        []string   `xml:"genre,omitempty"`
	Directors     []string   `xml:"director,omitempty"`
	Actors        []nfoActor `xml:"actor,omitempty"`
	UniqueIDs     []nfoID    `xml:"uniqueid,omitempty"`
}
type nfoActor struct {
	Name string `xml:"name"`
	Role string `xml:"role,omitempty"`
}
type nfoID struct {
	Value   string `xml:",chardata"`
	Type    string `xml:"type,attr"`
	Default bool   `xml:"default,attr,omitempty"`
}

func marshalNFO(i collection.Item) []byte {
	root := "movie"
	switch i.(type) {
	case *collection.Show:
		root = "tvshow"
	case *collection.Season:
		root = "season"
	case *collection.Episode:
		root = "episodedetails"
	}
	title := i.Title()
	if title == "" {
		title = i.Name()
	}
	m := nfoRecord{XMLName: xml.Name{Local: root}, Title: title, Year: i.Year(), Plot: i.Plot(), Rating: i.Rating(), Genres: i.Genres(), Directors: i.Directors()}
	if episode, ok := i.(*collection.Episode); ok {
		m.Season, m.Episode = episode.SeasonNo, episode.EpisodeNo
	}
	for name, role := range i.Actors() {
		m.Actors = append(m.Actors, nfoActor{Name: name, Role: role})
	}
	ids := map[string]string{}
	if meta, ok := i.(interface{ ProviderIDs() map[string]string }); ok {
		ids = meta.ProviderIDs()
	}
	for k, v := range ids {
		m.UniqueIDs = append(m.UniqueIDs, nfoID{Value: v, Type: k, Default: strings.EqualFold(k, "tmdb")})
	}
	sort.Slice(m.Actors, func(a, b int) bool { return m.Actors[a].Name < m.Actors[b].Name })
	sort.Slice(m.UniqueIDs, func(a, b int) bool { return m.UniqueIDs[a].Type < m.UniqueIDs[b].Type })
	data, _ := xml.MarshalIndent(m, "", "  ")
	return append([]byte(xml.Header), data...)
}
