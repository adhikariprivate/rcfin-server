package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/mux"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"tailscale.com/tsnet"

	"github.com/adhikariprivate/rcfin-server/internal/collection"
	"github.com/adhikariprivate/rcfin-server/internal/database"
	"github.com/adhikariprivate/rcfin-server/internal/database/postgres"
	"github.com/adhikariprivate/rcfin-server/internal/database/sqlite"
	"github.com/adhikariprivate/rcfin-server/internal/imageresize"
	"github.com/adhikariprivate/rcfin-server/internal/jellyfin"
	metadatarefresh "github.com/adhikariprivate/rcfin-server/internal/metadata/refresh"
	"github.com/adhikariprivate/rcfin-server/internal/muxnormalizer"
	"github.com/adhikariprivate/rcfin-server/internal/organization"
	"github.com/adhikariprivate/rcfin-server/internal/rclonestore"
	"github.com/adhikariprivate/rcfin-server/internal/subtitles"
	virtualdav "github.com/adhikariprivate/rcfin-server/internal/webdav"
)

// Set via ldflags by GoReleaser (see .goreleaser.yaml).
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

type configFile struct {
	Listen struct {
		Address   string `mapstructure:"address"`
		Port      string `mapstructure:"port"`
		TlsCert   string `mapstructure:"tls_cert"`
		TlsKey    string `mapstructure:"tls_key"`
		IPACL     string `mapstructure:"ip_acl"`
		Tailscale struct {
			Enabled    bool   `mapstructure:"enabled"`
			Hostname   string `mapstructure:"hostname"`
			StateDir   string `mapstructure:"state_dir"`
			AuthKeyEnv string `mapstructure:"auth_key_env"`
			ControlURL string `mapstructure:"control_url"`
			Port       string `mapstructure:"port"`
			HTTPS      *bool  `mapstructure:"https"`
		} `mapstructure:"tailscale"`
	}
	Cachedir string `mapstructure:"cachedir"`
	Database struct {
		Driver   string            `mapstructure:"driver"`
		Sqlite   sqlite.ConfigFile `mapstructure:"sqlite" yaml:"sqlite"`
		Postgres postgres.Config   `mapstructure:"postgres" yaml:"postgres"`
	} `yaml:"database"`
	Logfile        string
	ScanInterval   string `mapstructure:"scan_interval"`
	RescanInterval string `mapstructure:"rescan_interval"`
	Rclone         struct {
		Config   string `mapstructure:"config"`
		CacheDir string `mapstructure:"cache_dir"`
	} `mapstructure:"rclone"`
	Collections []collectionConfig `mapstructure:"collections"`
	WebDAV      struct {
		Enabled bool   `mapstructure:"enabled"`
		Path    string `mapstructure:"path"`
		Cache   struct {
			DirectoryTTL string `mapstructure:"directory_ttl"`
		} `mapstructure:"cache"`
	} `mapstructure:"webdav"`
	Jellyfin struct {
		ServerID           string `mapstructure:"server_id"`
		ServerName         string `mapstructure:"server_name"`
		QuickConnect       bool   `mapstructure:"quick_connect"`
		ImageQualityPoster int    `mapstructure:"image_quality_poster"`
	}
}

type collectionConfig struct {
	ID     string         `mapstructure:"id"`
	Name   string         `mapstructure:"name"`
	Type   string         `mapstructure:"type"`
	Remote string         `mapstructure:"remote"`
	VFS    map[string]any `mapstructure:"vfs"`
	Media  mediaConfig    `mapstructure:"media"`
	WebDAV struct {
		Enabled     bool              `mapstructure:"enabled"`
		Views       []string          `mapstructure:"views"`
		Labels      map[string]string `mapstructure:"labels"`
		FileName    string            `mapstructure:"file_name"`
		TitleFolder string            `mapstructure:"title_folder"`
		EpisodeName string            `mapstructure:"episode_file_name"`
		NFO         bool              `mapstructure:"nfo"`
		Artwork     bool              `mapstructure:"artwork"`
	} `mapstructure:"webdav"`
}

// Media options are intentionally opt-in. Organization stays disabled and
// dry-run stays enabled unless the operator explicitly changes both settings.
type mediaConfig struct {
	ScanInterval string `mapstructure:"scan_interval"`
	MissingGrace string `mapstructure:"missing_grace"`
	InboxPath    string `mapstructure:"inbox_path"`
	Inbox        struct {
		StableFor        string `mapstructure:"stable_for"`
		CompletionMarker string `mapstructure:"completion_marker"`
	} `mapstructure:"inbox"`
	MaxParallelJobs   int `mapstructure:"max_parallel_jobs"`
	NormalizeExisting struct {
		Enabled bool `mapstructure:"enabled"`
		DryRun  bool `mapstructure:"dry_run"`
	} `mapstructure:"normalize_existing"`
	Organize struct {
		Enabled                     bool     `mapstructure:"enabled"`
		DryRun                      bool     `mapstructure:"dry_run"`
		DiscFolders                 bool     `mapstructure:"disc_folders"`
		PreserveGroupingDirectories bool     `mapstructure:"preserve_grouping_directories"`
		ConflictPolicy              string   `mapstructure:"conflict_policy"`
		AllowCrossBackendCopyDelete bool     `mapstructure:"allow_cross_backend_copy_delete"`
		IncludeProviderID           bool     `mapstructure:"include_provider_id"`
		MinimumConfidence           float64  `mapstructure:"minimum_confidence"`
		FolderFormat                string   `mapstructure:"folder_format"`
		FileFormat                  string   `mapstructure:"file_format"`
		SeriesFormat                string   `mapstructure:"series_format"`
		SeasonFormat                string   `mapstructure:"season_format"`
		EpisodeFormat               string   `mapstructure:"episode_format"`
		EpisodeFallbackLanguages    []string `mapstructure:"episode_fallback_languages"`
		AllowedExtensions           []string `mapstructure:"allowed_extensions"`
		StableFor                   string   `mapstructure:"stable_for"`
		CompletionMarker            string   `mapstructure:"completion_marker"`
	} `mapstructure:"organize"`
	Metadata struct {
		Enabled              bool     `mapstructure:"enabled"`
		Provider             string   `mapstructure:"provider"`
		APIKeyEnv            string   `mapstructure:"api_key_env"`
		Language             string   `mapstructure:"language"`
		FallbackLanguages    []string `mapstructure:"fallback_languages"`
		ArtworkMaxBytes      int64    `mapstructure:"artwork_max_bytes"`
		ArtworkMaxWidth      int      `mapstructure:"artwork_max_width"`
		ArtworkMaxHeight     int      `mapstructure:"artwork_max_height"`
		ArtworkFormats       []string `mapstructure:"artwork_formats"`
		ArtworkPosterSize    string   `mapstructure:"artwork_poster_size"`
		ArtworkBackdropSize  string   `mapstructure:"artwork_backdrop_size"`
		RequestTimeout       string   `mapstructure:"request_timeout"`
		RetryLimit           int      `mapstructure:"retry_limit"`
		FetchArtwork         bool     `mapstructure:"fetch_artwork"`
		FetchNFO             bool     `mapstructure:"fetch_nfo"`
		OverwriteExisting    bool     `mapstructure:"overwrite_existing"`
		OverwriteArtwork     bool     `mapstructure:"overwrite_artwork"`
		MinimumConfidence    float64  `mapstructure:"minimum_confidence"`
		RefreshExistingAfter string   `mapstructure:"refresh_existing_after"`
	} `mapstructure:"metadata"`
	Subtitles struct {
		Enabled           bool     `mapstructure:"enabled"`
		Provider          string   `mapstructure:"provider"`
		APIKeyEnv         string   `mapstructure:"api_key_env"`
		UsernameEnv       string   `mapstructure:"username_env"`
		PasswordEnv       string   `mapstructure:"password_env"`
		Languages         []string `mapstructure:"languages"`
		HearingImpaired   bool     `mapstructure:"hearing_impaired"`
		Forced            bool     `mapstructure:"forced"`
		Formats           []string `mapstructure:"formats"`
		OverwriteExisting bool     `mapstructure:"overwrite_existing"`
		MinimumConfidence float64  `mapstructure:"minimum_confidence"`
		RequestTimeout    string   `mapstructure:"request_timeout"`
		RetryLimit        int      `mapstructure:"retry_limit"`
		MaxDownloadBytes  int64    `mapstructure:"max_download_bytes"`
	} `mapstructure:"subtitles"`
}

type mediaJobLimiter struct {
	mu    sync.Mutex
	slots map[string]chan struct{}
}

var perLibraryMediaJobs = mediaJobLimiter{slots: map[string]chan struct{}{}}

func (l *mediaJobLimiter) run(ctx context.Context, libraryID string, maximum int, job func() error) error {
	if maximum <= 0 {
		maximum = 1
	}
	l.mu.Lock()
	slots := l.slots[libraryID]
	if slots == nil {
		slots = make(chan struct{}, maximum)
		l.slots[libraryID] = slots
	}
	l.mu.Unlock()
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
		return job()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func applyLegacyJellyfinConfig(v *viper.Viper, config *configFile) {
	// Keep accepting the original compact key spellings used by pre-1.0.1
	// configs while documenting the underscore-separated names going forward.
	if !v.IsSet("jellyfin.server_name") && v.IsSet("jellyfin.servername") {
		config.Jellyfin.ServerName = v.GetString("jellyfin.servername")
	}
	if !v.IsSet("jellyfin.image_quality_poster") && v.IsSet("jellyfin.imagequalityposter") {
		config.Jellyfin.ImageQualityPoster = v.GetInt("jellyfin.imagequalityposter")
	}
}

func readConfig(filename string) (configFile, error) {
	v := viper.New()
	v.SetConfigType("yaml")
	v.SetConfigFile(filename)
	if err := v.ReadInConfig(); err != nil {
		return configFile{}, err
	}
	var config configFile
	if err := v.Unmarshal(&config); err != nil {
		return configFile{}, err
	}
	for i := range config.Collections {
		if !v.IsSet(fmt.Sprintf("collections.%d.webdav.enabled", i)) {
			config.Collections[i].WebDAV.Enabled = true
		}
		if !v.IsSet(fmt.Sprintf("collections.%d.webdav.views", i)) {
			config.Collections[i].WebDAV.Views = []string{"all", "genres", "actors", "directors", "years"}
		}
		if !v.IsSet(fmt.Sprintf("collections.%d.webdav.nfo", i)) {
			config.Collections[i].WebDAV.NFO = true
		}
		if !v.IsSet(fmt.Sprintf("collections.%d.webdav.artwork", i)) {
			config.Collections[i].WebDAV.Artwork = true
		}
		if !v.IsSet(fmt.Sprintf("collections.%d.media.metadata.overwrite_artwork", i)) {
			config.Collections[i].Media.Metadata.OverwriteArtwork = config.Collections[i].Media.Metadata.OverwriteExisting
		}
		if !v.IsSet(fmt.Sprintf("collections.%d.media.organize.dry_run", i)) {
			config.Collections[i].Media.Organize.DryRun = true
		}
		if !v.IsSet(fmt.Sprintf("collections.%d.media.organize.preserve_grouping_directories", i)) {
			config.Collections[i].Media.Organize.PreserveGroupingDirectories = true
		}
		if !v.IsSet(fmt.Sprintf("collections.%d.media.normalize_existing.dry_run", i)) {
			config.Collections[i].Media.NormalizeExisting.DryRun = true
		}
		if !v.IsSet(fmt.Sprintf("collections.%d.media.subtitles.minimum_confidence", i)) {
			config.Collections[i].Media.Subtitles.MinimumConfidence = .84
		}
	}
	applyLegacyJellyfinConfig(v, &config)
	if config.WebDAV.Enabled {
		davPath := config.WebDAV.Path
		if davPath == "" {
			davPath = "/dav/"
		}
		clean := path.Clean(davPath)
		if !strings.HasPrefix(davPath, "/") || clean == "/" || clean != strings.TrimSuffix(davPath, "/") {
			return configFile{}, fmt.Errorf("webdav.path must be a non-root absolute URL path without dot segments")
		}
		for i, lib := range config.Collections {
			for _, view := range lib.WebDAV.Views {
				switch strings.ToLower(view) {
				case "all", "genres", "actors", "directors", "years", "studios":
				default:
					return configFile{}, fmt.Errorf("collections[%d].webdav.views contains unsupported view %q", i, view)
				}
			}
		}
		if config.WebDAV.Cache.DirectoryTTL != "" {
			ttl, err := time.ParseDuration(config.WebDAV.Cache.DirectoryTTL)
			if err != nil || ttl <= 0 {
				return configFile{}, fmt.Errorf("webdav.cache.directory_ttl must be a positive duration")
			}
		}
	}
	return config, nil
}

var libraryIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func validateCollections(collections []collectionConfig) error {
	seen := make(map[string]struct{}, len(collections))
	for i, c := range collections {
		if !libraryIDPattern.MatchString(c.ID) {
			return fmt.Errorf("collections[%d].id is required and must contain only letters, digits, underscores, or hyphens", i)
		}
		if _, ok := seen[c.ID]; ok {
			return fmt.Errorf("duplicate collection ID %q", c.ID)
		}
		seen[c.ID] = struct{}{}
		if c.Name == "" || c.Remote == "" {
			return fmt.Errorf("collection %q requires a name and remote", c.ID)
		}
		if c.Media.MaxParallelJobs < 0 || c.Media.MaxParallelJobs > 32 {
			return fmt.Errorf("collection %q media.max_parallel_jobs must be between 0 and 32 (0 uses the default of 1)", c.ID)
		}
		if scanInterval := strings.TrimSpace(c.Media.ScanInterval); scanInterval != "" {
			parsed, err := time.ParseDuration(scanInterval)
			if err != nil || parsed <= 0 {
				return fmt.Errorf("collection %q has invalid media.scan_interval %q", c.ID, scanInterval)
			}
		}
		if missingGrace := strings.TrimSpace(c.Media.MissingGrace); missingGrace != "" {
			parsed, err := time.ParseDuration(missingGrace)
			if err != nil || parsed <= 0 {
				return fmt.Errorf("collection %q has invalid media.missing_grace %q", c.ID, missingGrace)
			}
		}
		if stableFor := strings.TrimSpace(c.Media.Inbox.StableFor); stableFor != "" {
			parsed, err := time.ParseDuration(stableFor)
			if err != nil || parsed <= 0 {
				return fmt.Errorf("collection %q has invalid media.inbox.stable_for %q", c.ID, stableFor)
			}
		}
		if marker := strings.TrimSpace(c.Media.Inbox.CompletionMarker); marker != "" && (path.Base(marker) != marker || marker == "." || marker == ".." || strings.Contains(marker, "\\")) {
			return fmt.Errorf("collection %q media.inbox.completion_marker must be a filename within the Inbox item", c.ID)
		}
		if c.Type != "movies" && c.Type != "shows" {
			return fmt.Errorf("collection %q has unsupported type %q", c.ID, c.Type)
		}
		if c.Media.Organize.DiscFolders && c.Type != "movies" {
			return fmt.Errorf("collection %q media.organize.disc_folders is supported only for movie libraries", c.ID)
		}
		if inbox := strings.TrimSpace(c.Media.InboxPath); inbox != "" {
			clean := path.Clean(inbox)
			if path.IsAbs(inbox) || clean != inbox || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(inbox, "\\") {
				return fmt.Errorf("collection %q has invalid relative media.inbox_path %q", c.ID, inbox)
			}
		}
		if c.Media.Metadata.Provider != "" && c.Media.Metadata.Provider != "tmdb" {
			return fmt.Errorf("collection %q has unsupported metadata provider %q", c.ID, c.Media.Metadata.Provider)
		}
		if c.Media.Subtitles.Provider != "" && c.Media.Subtitles.Provider != "opensubtitles" {
			return fmt.Errorf("collection %q has unsupported subtitle provider %q", c.ID, c.Media.Subtitles.Provider)
		}
		if lang := c.Media.Metadata.Language; lang != "" && !subtitles.ValidLanguage(lang) {
			return fmt.Errorf("collection %q has invalid metadata language %q", c.ID, lang)
		}
		if d := strings.TrimSpace(c.Media.Metadata.RefreshExistingAfter); d != "" {
			if parsed, err := time.ParseDuration(d); err != nil || parsed < 0 {
				return fmt.Errorf("collection %q has invalid metadata.refresh_existing_after %q", c.ID, d)
			}
		}
		if c.Media.Metadata.MinimumConfidence < 0 || c.Media.Metadata.MinimumConfidence > 1 {
			return fmt.Errorf("collection %q metadata.minimum_confidence must be between 0 and 1", c.ID)
		}
		if c.Media.Subtitles.MinimumConfidence < 0 || c.Media.Subtitles.MinimumConfidence > 1 {
			return fmt.Errorf("collection %q subtitles.minimum_confidence must be between 0 and 1", c.ID)
		}
		seenSubtitleLanguages := map[string]bool{}
		for _, lang := range c.Media.Subtitles.Languages {
			if !subtitles.ValidLanguage(lang) {
				return fmt.Errorf("collection %q has invalid subtitle language %q", c.ID, lang)
			}
			key := strings.ToLower(strings.TrimSpace(lang))
			if seenSubtitleLanguages[key] {
				return fmt.Errorf("collection %q repeats subtitle language %q", c.ID, lang)
			}
			seenSubtitleLanguages[key] = true
		}
		for _, languages := range []struct {
			name   string
			values []string
		}{{"metadata fallback", c.Media.Metadata.FallbackLanguages}, {"organization episode fallback", c.Media.Organize.EpisodeFallbackLanguages}} {
			seen := map[string]bool{}
			for _, lang := range languages.values {
				if !subtitles.ValidLanguage(lang) {
					return fmt.Errorf("collection %q has invalid %s language %q", c.ID, languages.name, lang)
				}
				key := strings.ToLower(lang)
				if seen[key] {
					return fmt.Errorf("collection %q repeats %s language %q", c.ID, languages.name, lang)
				}
				seen[key] = true
			}
		}
		for _, policy := range []struct {
			name, timeout string
			retries       int
		}{{"metadata", c.Media.Metadata.RequestTimeout, c.Media.Metadata.RetryLimit}, {"subtitles", c.Media.Subtitles.RequestTimeout, c.Media.Subtitles.RetryLimit}} {
			if timeout := strings.TrimSpace(policy.timeout); timeout != "" {
				parsed, err := time.ParseDuration(timeout)
				if err != nil || parsed <= 0 || parsed > 5*time.Minute {
					return fmt.Errorf("collection %q media.%s.request_timeout must be between 0 and 5m", c.ID, policy.name)
				}
			}
			if policy.retries < 0 || policy.retries > 10 {
				return fmt.Errorf("collection %q media.%s.retry_limit must be between 0 and 10", c.ID, policy.name)
			}
		}
		if c.Media.Subtitles.MaxDownloadBytes < 0 || c.Media.Subtitles.MaxDownloadBytes > 20<<20 {
			return fmt.Errorf("collection %q media.subtitles.max_download_bytes must be between 0 and 20971520", c.ID)
		}
		if c.Media.Metadata.ArtworkMaxBytes < 0 || c.Media.Metadata.ArtworkMaxBytes > 32<<20 {
			return fmt.Errorf("collection %q metadata.artwork_max_bytes must be between 0 and 33554432", c.ID)
		}
		for _, dimension := range []struct {
			name  string
			value int
		}{{"artwork_max_width", c.Media.Metadata.ArtworkMaxWidth}, {"artwork_max_height", c.Media.Metadata.ArtworkMaxHeight}} {
			if dimension.value < 0 || dimension.value > 10000 {
				return fmt.Errorf("collection %q metadata.%s must be between 0 and 10000", c.ID, dimension.name)
			}
		}
		for _, format := range c.Media.Metadata.ArtworkFormats {
			format = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(format)), ".")
			if format != "jpg" && format != "jpeg" && format != "png" {
				return fmt.Errorf("collection %q has unsupported artwork format %q", c.ID, format)
			}
		}
		for _, size := range []string{c.Media.Metadata.ArtworkPosterSize, c.Media.Metadata.ArtworkBackdropSize} {
			if size != "" && size != "w500" && size != "w780" && size != "original" {
				return fmt.Errorf("collection %q has unsupported TMDb artwork size %q", c.ID, size)
			}
		}
		for _, format := range c.Media.Subtitles.Formats {
			normalized := strings.TrimPrefix(strings.ToLower(format), ".")
			if normalized != "srt" && normalized != "vtt" {
				return fmt.Errorf("collection %q has unsupported subtitle format %q", c.ID, format)
			}
		}
		org := c.Media.Organize
		if stableFor := strings.TrimSpace(org.StableFor); stableFor != "" {
			if parsed, err := time.ParseDuration(stableFor); err != nil || parsed <= 0 {
				return fmt.Errorf("collection %q has invalid organize.stable_for %q", c.ID, stableFor)
			}
		}
		if marker := strings.TrimSpace(org.CompletionMarker); marker != "" && (path.Base(marker) != marker || marker == "." || marker == ".." || strings.Contains(marker, "\\")) {
			return fmt.Errorf("collection %q organize.completion_marker must be a filename within the Inbox item", c.ID)
		}
		if org.MinimumConfidence < 0 || org.MinimumConfidence > 1 {
			return fmt.Errorf("collection %q organize.minimum_confidence must be between 0 and 1", c.ID)
		}
		if org.ConflictPolicy != "" && org.ConflictPolicy != "skip" {
			return fmt.Errorf("collection %q has unsupported organize.conflict_policy %q", c.ID, org.ConflictPolicy)
		}
		if org.AllowCrossBackendCopyDelete {
			return fmt.Errorf("collection %q cross-backend copy/delete organization is not supported", c.ID)
		}
		if err := validateNamingTemplate(org.FolderFormat, "title", "year", "tmdb_id"); err != nil {
			return fmt.Errorf("collection %q folder_format: %w", c.ID, err)
		}
		if err := validateNamingTemplate(org.FileFormat, "folder", "quality", "resolution", "version", "edition", "release_group", "ext"); err != nil {
			return fmt.Errorf("collection %q file_format: %w", c.ID, err)
		}
		if err := validateNamingTemplate(org.SeriesFormat, "title", "year", "tmdb_id"); err != nil {
			return fmt.Errorf("collection %q series_format: %w", c.ID, err)
		}
		if err := validateNamingTemplate(org.SeasonFormat, "season", "season_number"); err != nil {
			return fmt.Errorf("collection %q season_format: %w", c.ID, err)
		}
		if err := validateNamingTemplate(org.EpisodeFormat, "series", "year", "season", "episode", "episode_title", "quality", "resolution", "version", "edition", "release_group", "ext"); err != nil {
			return fmt.Errorf("collection %q episode_format: %w", c.ID, err)
		}
		for _, ext := range org.AllowedExtensions {
			normalized := strings.TrimPrefix(strings.ToLower(ext), ".")
			if !regexp.MustCompile(`^[a-z0-9]{2,8}$`).MatchString(normalized) {
				return fmt.Errorf("collection %q has invalid organize.allowed_extensions entry %q", c.ID, ext)
			}
		}
	}
	return nil
}

func mediaHTTPClient(configuredTimeout string) *http.Client {
	if timeout := strings.TrimSpace(configuredTimeout); timeout != "" {
		if parsed, err := time.ParseDuration(timeout); err == nil && parsed > 0 {
			return &http.Client{Timeout: parsed}
		}
	}
	return nil
}

func validateNamingTemplate(format string, allowed ...string) error {
	allowedSet := map[string]bool{}
	for _, name := range allowed {
		allowedSet[name] = true
	}
	pattern := regexp.MustCompile(`\{([a-z_]+)\}`)
	for _, match := range pattern.FindAllStringSubmatch(format, -1) {
		if !allowedSet[match[1]] {
			return fmt.Errorf("unsupported token {%s}", match[1])
		}
	}
	remaining := pattern.ReplaceAllString(format, "")
	if strings.ContainsAny(remaining, "{}") {
		return fmt.Errorf("malformed template")
	}
	return nil
}

func main() {
	if err := initializeEnvironment(); err != nil {
		log.Printf("initialize environment: %v", err)
		os.Exit(1)
	}
	if args, configPath, ok := managementCommand(os.Args[1:]); ok {
		if err := runManagementCommand(configPath, args); err != nil {
			log.Printf("%v", err)
			os.Exit(1)
		}
		return
	}
	runServer()
}

func runServer() {
	const configFileNameKey = "config"

	// Set up viper for config file and command line flags
	viper.SetConfigType("yaml")
	viper.SetDefault("listen.port", "8096")
	viper.SetDefault("logfile", "stdout")

	pflag.String("config", "rcfin.yaml", "Path to configuration file.")
	pflag.Bool("version", false, "Print version information and exit.")
	viper.BindPFlag(configFileNameKey, pflag.Lookup("config"))
	pflag.Parse()

	if showVersion, _ := pflag.CommandLine.GetBool("version"); showVersion {
		fmt.Printf("rcfin %s (commit %s, built %s)\n", version, commit, date)
		os.Exit(0)
	}

	rootCtx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Read config file
	cf := viper.GetString(configFileNameKey)
	log.Printf("Using config file %s", cf)
	config, err := readConfig(cf)
	if err != nil {
		log.Fatalf("Error reading config file: %v", err)
	}

	// Set up logging
	logfile := viper.GetString("logfile")
	log.Printf("Setting logfile to %s", logfile)
	switch logfile {
	case "stdout":
		log.SetOutput(os.Stdout)
	case "stderr":
		log.SetOutput(os.Stderr)
	case "none":
		log.SetOutput(io.Discard)
	default:
		f, err := os.OpenFile(logfile, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0666)
		if err != nil {
			log.Fatalf("error opening file: %v", err)
		}
		defer f.Close()
		log.SetOutput(f)
	}

	if err := validateCollections(config.Collections); err != nil {
		log.Fatalf("invalid collections: %v", err)
	}
	log.Printf("dbinit")
	repo, err := openConfiguredDatabase(config)
	if err != nil {
		log.Fatalf("database.New: %s", err.Error())
	}
	defer repo.Close()
	repo.StartBackgroundJobs(rootCtx)
	if err := rclonestore.Configure(config.Rclone.Config, config.Rclone.CacheDir); err != nil {
		log.Fatalf("rclone configuration: %v", err)
	}
	rescanInterval := 30 * time.Minute
	configuredScanInterval := config.ScanInterval
	if configuredScanInterval == "" {
		configuredScanInterval = config.RescanInterval // legacy global spelling
	}
	if configuredScanInterval != "" {
		rescanInterval, err = time.ParseDuration(configuredScanInterval)
		if err != nil || rescanInterval <= 0 {
			log.Fatalf("invalid scan_interval %q: expected a positive duration", configuredScanInterval)
		}
	}

	// Initialize collection and add them to the collection manager
	collection := collection.New(&collection.Options{
		Repo:           repo,
		RescanInterval: rescanInterval,
	})
	for _, coll := range config.Collections {
		storage, err := rclonestore.New(rootCtx, coll.Remote, coll.VFS)
		if err != nil {
			log.Fatalf("collection %q: %v", coll.Name, err)
		}
		inbox := coll.Media.InboxPath
		if inbox == "" {
			inbox = ".Inbox"
		}
		collection.AddCollectionWithInbox(
			coll.Name,
			coll.ID,
			coll.Type,
			coll.Remote,
			storage,
			inbox,
		)
	}
	defer collection.Close()

	resizer := imageresize.New(imageresize.Options{
		Cachedir: config.Cachedir,
	})
	// Periodically evict stale entries from the local resized-image cache.

	log.Printf("building mux")

	r := mux.NewRouter()

	j := jellyfin.New(&jellyfin.Options{
		Collections:        collection,
		Repo:               repo,
		Imageresizer:       resizer,
		ServerPort:         config.Listen.Port,
		ServerID:           config.Jellyfin.ServerID,
		ServerName:         config.Jellyfin.ServerName,
		QuickConnect:       config.Jellyfin.QuickConnect,
		ImageQualityPoster: config.Jellyfin.ImageQualityPoster,
	})
	j.RegisterHandlers(r)
	if config.WebDAV.Enabled {
		davPath := config.WebDAV.Path
		if davPath == "" {
			davPath = "/dav/"
		}
		libraries := make(map[string]virtualdav.LibraryConfig, len(config.Collections))
		for _, lib := range config.Collections {
			libraries[lib.ID] = virtualdav.LibraryConfig{Enabled: lib.WebDAV.Enabled, Views: lib.WebDAV.Views, Labels: lib.WebDAV.Labels, FileName: lib.WebDAV.FileName, TitleFolder: lib.WebDAV.TitleFolder, EpisodeName: lib.WebDAV.EpisodeName, ShowNFO: lib.WebDAV.NFO, ShowArtwork: lib.WebDAV.Artwork}
		}
		cacheTTL := time.Minute
		if config.WebDAV.Cache.DirectoryTTL != "" {
			cacheTTL, _ = time.ParseDuration(config.WebDAV.Cache.DirectoryTTL)
		}
		webdavPrefix := strings.TrimSuffix(davPath, "/")
		if webdavPrefix == "" {
			webdavPrefix = "/dav"
		}
		r.PathPrefix(webdavPrefix).Handler(virtualdav.New(virtualdav.Options{Path: webdavPrefix, Collections: collection, Repo: repo, Libraries: libraries, CacheTTL: cacheTTL}))
		log.Printf("Read-only WebDAV enabled at %s", davPath)
	}

	r.Path("/robots.txt").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("User-agent: *\nDisallow: /\n"))
	})

	if err := collection.InitContext(rootCtx); err != nil {
		if rootCtx.Err() != nil {
			return
		}
		log.Printf("initial collection search index build failed: %v", err)
	}
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		libraryAutomationLoop(rootCtx, collection, repo, config)
	}()
	if config.Cachedir != "" {
		workers.Add(1)
		go func() { defer workers.Done(); resizer.CleanCacheContext(rootCtx, 7*24*time.Hour, time.Hour) }()
	}

	addr := net.JoinHostPort(config.Listen.Address, config.Listen.Port)

	// Add muxnormalizer middleware to canonicalize request paths and query parameters
	canon, err := muxnormalizer.New(r)
	if err != nil {
		log.Fatal(err)
	}
	// Logging and canonicalization are shared by both endpoints. The public
	// endpoint alone applies the configured IP allow list.
	baseHandler := HttpLog(canon.Middleware(r))
	publicHandler := IPACLmiddleware(config.Listen.IPACL, baseHandler)

	srv := &http.Server{Addr: addr, Handler: publicHandler}
	useTLS := config.Listen.TlsCert != "" && config.Listen.TlsKey != ""
	if useTLS {
		kpr, err := NewKeypairReloader(config.Listen.TlsCert, config.Listen.TlsKey)
		if err != nil {
			log.Fatalf("error loading keypair: %v", err)
		}
		srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: kpr.GetCertificateFunc()}
		log.Printf("Serving HTTPS on %s", addr)
	} else {
		log.Printf("Serving HTTP on %s", addr)
	}
	serveErrors := make(chan error, 2)
	go func() {
		var serveErr error
		listener, listenErr := net.Listen("tcp", addr)
		if listenErr != nil {
			serveErrors <- listenErr
			return
		}
		if useTLS {
			listener = tls.NewListener(listener, srv.TLSConfig)
		}
		serveErr = srv.Serve(listener)
		serveErrors <- serveErr
	}()
	var tsServer *tsnet.Server
	var tsHTTPServer *http.Server
	if config.Listen.Tailscale.Enabled {
		if strings.TrimSpace(config.Listen.Tailscale.Hostname) == "" || strings.TrimSpace(config.Listen.Tailscale.StateDir) == "" {
			log.Fatal("listen.tailscale.hostname and listen.tailscale.state_dir are required when Tailscale is enabled")
		}
		if err := os.MkdirAll(config.Listen.Tailscale.StateDir, 0700); err != nil {
			log.Fatalf("create Tailscale state directory: %v", err)
		}
		var authKey string
		authKeyEnv := config.Listen.Tailscale.AuthKeyEnv
		if authKeyEnv == "" {
			authKeyEnv = "RCFIN_TAILSCALE_AUTH_KEY"
		}
		authKey = os.Getenv(authKeyEnv)
		if strings.TrimSpace(authKey) == "" {
			log.Fatalf("Tailscale is enabled but auth key environment variable %s is empty", authKeyEnv)
		}
		tsServer = &tsnet.Server{
			Hostname:   config.Listen.Tailscale.Hostname,
			Dir:        config.Listen.Tailscale.StateDir,
			AuthKey:    authKey,
			ControlURL: config.Listen.Tailscale.ControlURL,
		}
		if err := tsServer.Start(); err != nil {
			log.Fatalf("start Tailscale: %v", err)
		}
		tsPort := strings.TrimSpace(config.Listen.Tailscale.Port)
		if tsPort == "" {
			tsPort = config.Listen.Port
			if config.Listen.Tailscale.HTTPS != nil && *config.Listen.Tailscale.HTTPS {
				tsPort = "443"
			}
		}
		tsHTTPS := useTLS
		if config.Listen.Tailscale.HTTPS != nil {
			tsHTTPS = *config.Listen.Tailscale.HTTPS
		}
		var tsListener net.Listener
		if config.Listen.Tailscale.HTTPS != nil && *config.Listen.Tailscale.HTTPS {
			tsListener, err = tsServer.ListenTLS("tcp", ":"+tsPort)
		} else {
			tsListener, err = tsServer.Listen("tcp", ":"+tsPort)
			if err == nil && tsHTTPS {
				tsListener = tls.NewListener(tsListener, srv.TLSConfig)
			}
		}
		if err != nil {
			_ = tsServer.Close()
			log.Fatalf("listen on Tailscale: %v", err)
		}
		tsHTTPServer = &http.Server{Handler: baseHandler}
		if tsHTTPS {
			log.Printf("Serving HTTPS on Tailscale as %s:%s", config.Listen.Tailscale.Hostname, tsPort)
		} else {
			log.Printf("Serving HTTP on Tailscale as %s:%s", config.Listen.Tailscale.Hostname, tsPort)
		}
		go func() { serveErrors <- tsHTTPServer.Serve(tsListener) }()
	}
	select {
	case serveErr := <-serveErrors:
		if serveErr != nil && serveErr != http.ErrServerClosed {
			log.Printf("HTTP server stopped: %v", serveErr)
		}
		cancel()
	case <-rootCtx.Done():
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("graceful HTTP shutdown failed: %v", err)
		_ = srv.Close()
	}
	if tsHTTPServer != nil {
		if err := tsHTTPServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("graceful Tailscale HTTP shutdown failed: %v", err)
			_ = tsHTTPServer.Close()
		}
	}
	if tsServer != nil {
		if err := tsServer.Close(); err != nil {
			log.Printf("close Tailscale server: %v", err)
		}
	}
	shutdownCancel()
	cancel()
	workers.Wait()
}

func organizationReportSubmitted(report organization.Report) bool {
	for _, item := range report.Items {
		if item.State == "submitted" {
			return true
		}
	}
	return false
}

func metadataReportMayHaveChangedStorage(report metadatarefresh.Report) bool {
	for _, item := range report.Items {
		for _, file := range item.Files {
			if file.State == "written" || file.State == "failed" {
				return true
			}
		}
	}
	return false
}

func subtitleReportMayHaveChangedStorage(report subtitles.SubtitleReport) bool {
	for _, action := range report.Actions {
		if action.State == "written" || action.State == "failed" {
			return true
		}
	}
	return false
}

func openConfiguredDatabase(config configFile) (database.Repository, error) {
	driver := strings.ToLower(strings.TrimSpace(config.Database.Driver))
	if driver == "" {
		// Preserve compatibility with existing configurations that only define
		// database.sqlite.filename.
		driver = "sqlite"
	}
	if driver == "postgres" && config.Database.Postgres.URL == "" {
		config.Database.Postgres.URL = os.Getenv("RCFIN_POSTGRES_URL")
	}
	switch driver {
	case "sqlite":
		return database.New(driver, config.Database.Sqlite)
	case "postgres":
		return database.New(driver, config.Database.Postgres)
	default:
		return nil, fmt.Errorf("unsupported database.driver %q (supported: sqlite, postgres)", driver)
	}
}

type keypairReloader struct {
	certMu   sync.RWMutex
	cert     *tls.Certificate
	certPath string
	keyPath  string
}

// NewKeypairReloader creates a new keypair reloader that will reload the TLS certificate
// and key from the specified paths every 15 seconds. If the certificate cannot be loaded,
// it will log an error and keep the old certificate in use.
func NewKeypairReloader(certPath, keyPath string) (*keypairReloader, error) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, err
	}
	result := &keypairReloader{
		certPath: certPath,
		keyPath:  keyPath,
		cert:     &cert,
	}

	go func() {
		for {
			// log.Printf("Attemping reloading TLS certificate and key from %q and %q", certPath, keyPath)
			time.Sleep(15 * time.Second)
			if err := result.maybeReload(); err != nil {
				log.Printf("Keeping old TLS certificate because the new one could not be loaded: %v", err)
			}
		}
	}()
	return result, nil
}

func (kpr *keypairReloader) GetCertificateFunc() func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return func(clientHello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		kpr.certMu.RLock()
		defer kpr.certMu.RUnlock()
		return kpr.cert, nil
	}
}

func (kpr *keypairReloader) maybeReload() error {
	newCert, err := tls.LoadX509KeyPair(kpr.certPath, kpr.keyPath)
	if err != nil {
		return err
	}
	kpr.certMu.Lock()
	defer kpr.certMu.Unlock()
	kpr.cert = &newCert
	return nil
}

// IPACLmiddleware is an HTTP middleware enforcing IP allow list, if set.
func IPACLmiddleware(acl string, next http.Handler) http.Handler {
	// Don't process ACL in case it's empty
	acl = strings.TrimSpace(acl)
	if acl == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Get client ip without port
		remote := r.RemoteAddr
		if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			remote = host
		}
		clientIP := net.ParseIP(remote)
		if clientIP == nil {
			http.Error(w, "403 forbidden", http.StatusForbidden)
			return
		}
		allow := false
		for e := range strings.SplitSeq(acl, ",") {
			e := strings.TrimSpace(e)
			if strings.Contains(e, "/") {
				if _, cidr, err := net.ParseCIDR(e); err == nil {
					if cidr.Contains(clientIP) {
						allow = true
						break
					}
				} else {
					log.Printf("listen.ipacl: invalid CIDR %q: %v", e, err)
				}
				continue

			}
			if ip := net.ParseIP(e); ip == nil {
				if ip.Equal(clientIP) {
					allow = true
					break
				}
			} else {
				log.Printf("listen.ipacl: invalid IP %q", e)
			}
		}
		if !allow {
			http.Error(w, "403 forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
