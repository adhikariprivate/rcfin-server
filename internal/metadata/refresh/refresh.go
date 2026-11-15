// Package refresh connects read-only inventory, provider matching, and NFO
// sidecar refresh. It never renames or moves media files.
package refresh

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"math"
	"math/rand/v2"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/adhikariprivate/rcfin-server/internal/collection"
	collectionmetadata "github.com/adhikariprivate/rcfin-server/internal/collection/metadata"
	"github.com/adhikariprivate/rcfin-server/internal/database"
	"github.com/adhikariprivate/rcfin-server/internal/database/model"
	"github.com/adhikariprivate/rcfin-server/internal/metadata/nfo"
	"github.com/adhikariprivate/rcfin-server/internal/metadata/provider"
	"github.com/adhikariprivate/rcfin-server/internal/rclonestore"
)

type Options struct {
	Write                bool
	InboxOnly            bool
	ItemPaths            []string
	Journal              database.MediaOperationRepo
	FetchNFO             bool
	FetchArtwork         bool
	OverwriteExisting    bool
	OverwriteArtwork     bool
	Language             string
	FallbackLanguages    []string
	ArtworkMaxBytes      int64
	ArtworkMaxWidth      int
	ArtworkMaxHeight     int
	ArtworkFormats       []string
	ArtworkPosterSize    string
	ArtworkBackdropSize  string
	MinimumConfidence    float64
	TieMargin            float64
	RefreshExistingAfter time.Duration
}

type ItemReport struct {
	Source           string             `json:"source"`
	Destination      string             `json:"destination,omitempty"`
	ProviderID       string             `json:"provider_id,omitempty"`
	Confidence       float64            `json:"confidence,omitempty"`
	Evidence         []string           `json:"evidence,omitempty"`
	ProviderMetadata *provider.Metadata `json:"provider_metadata,omitempty"`
	State            string             `json:"state"`
	Reason           string             `json:"reason,omitempty"`
	Files            []FileAction       `json:"files,omitempty"`
}

type FileAction struct {
	Destination string `json:"destination"`
	State       string `json:"state"`
	Reason      string `json:"reason,omitempty"`
}

type Report struct {
	CollectionID string       `json:"collection_id"`
	Items        []ItemReport `json:"items"`
}

// Run refreshes existing library metadata in place. When Write is false it
// produces a dry-run report and performs no remote writes.
func Run(ctx context.Context, coll *collection.Collection, p provider.Provider, opts Options) (Report, error) {
	if coll == nil || coll.Storage == nil {
		return Report{}, fmt.Errorf("collection and storage are required")
	}
	worker := database.NewMediaOperationWorkerID("metadata-refresh")
	ctx, leaseKeeper, stopLeaseKeeper := database.StartMediaOperationLeaseKeeper(ctx, opts.Journal, worker, 15*time.Minute)
	defer stopLeaseKeeper()
	if strings.TrimSpace(opts.Language) == "" {
		opts.Language = "en-US"
	}
	report := Report{CollectionID: coll.ID}
	inventory, err := collection.InventoryCollection(ctx, coll, coll.InboxPath, opts.InboxOnly)
	if err != nil {
		return report, err
	}
	kind := provider.Movie
	if coll.Type == collection.CollectionTypeShows {
		kind = provider.Show
	}
	seen := map[string]struct{}{}
	itemPaths := make(map[string]struct{}, len(opts.ItemPaths))
	for _, itemPath := range opts.ItemPaths {
		itemPaths[itemPath] = struct{}{}
	}
	for _, entry := range inventory {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		movieDisc := kind == provider.Movie && entry.Role == collection.InventoryDisc && collection.IsMovieDiscStructure(entry, inventory)
		if entry.Role != collection.InventoryVideo && !movieDisc {
			continue
		}
		if len(itemPaths) > 0 {
			if _, ok := itemPaths[entry.Path]; !ok {
				continue
			}
		}
		sourceDir := path.Dir(entry.Path)
		if kind == provider.Show && metadataSeasonDirectory.MatchString(path.Base(sourceDir)) {
			sourceDir = path.Dir(sourceDir)
		}
		// A show-level record and a movie folder are both refreshed once, even
		// when the directory contains multiple episodes or cuts.
		if _, ok := seen[sourceDir]; ok {
			continue
		}
		seen[sourceDir] = struct{}{}
		nfoName := "movie.nfo"
		if kind == provider.Show {
			nfoName = "tvshow.nfo"
		}
		destination := path.Join(sourceDir, nfoName)
		forceRefresh := false
		if opts.Journal != nil {
			existing, journalErr := opts.Journal.GetMediaOperation(ctx, coll.ID, entry.Path, "metadata_refresh")
			if journalErr != nil {
				return report, journalErr
			}
			if existing != nil {
				signature := metadataJobSignature(entry, opts)
				forceRefresh = existing.Status == "planned" && journalSignature(existing.Evidence) == signature
				if journalSignature(existing.Evidence) == signature {
					switch existing.Status {
					case "needs_review", "failed_terminal":
						report.Items = append(report.Items, ItemReport{Source: entry.Path, Destination: destination, ProviderID: existing.ProviderID, Confidence: existing.Confidence, State: existing.Status, Reason: existing.Error})
						continue
					case "failed_retryable":
						if time.Now().Before(existing.NextAttempt) {
							report.Items = append(report.Items, ItemReport{Source: entry.Path, Destination: destination, ProviderID: existing.ProviderID, Confidence: existing.Confidence, State: "deferred", Reason: "retry backoff is active"})
							continue
						}
					case "complete":
						refreshAfter := opts.RefreshExistingAfter
						if refreshAfter <= 0 {
							refreshAfter = 24 * time.Hour
						}
						if time.Since(existing.UpdatedAt) < refreshAfter {
							report.Items = append(report.Items, ItemReport{Source: entry.Path, Destination: destination, ProviderID: existing.ProviderID, Confidence: existing.Confidence, State: "fresh", Reason: "refresh interval has not elapsed"})
							continue
						}
					case "running":
						if time.Now().Before(existing.LeaseUntil) {
							report.Items = append(report.Items, ItemReport{Source: entry.Path, Destination: destination, ProviderID: existing.ProviderID, Confidence: existing.Confidence, State: "deferred", Reason: "another worker holds the refresh lease"})
							continue
						}
					}
				}
			}
			claimed, err := beginMetadataOperation(ctx, opts.Journal, coll.ID, entry, opts, existing, worker, leaseKeeper)
			if err != nil {
				return report, err
			}
			if !claimed {
				report.Items = append(report.Items, ItemReport{Source: entry.Path, Destination: destination, State: "deferred", Reason: "another worker claimed the refresh operation"})
				continue
			}
		}
		if entry.IdentityConflict {
			report.Items = append(report.Items, ItemReport{Source: entry.Path, Destination: destination, State: "needs_review", Reason: "provider IDs conflict between the path and local metadata"})
			continue
		}
		ids := entry.ProviderIDs
		metadataFresh := false
		artworkFresh := false
		title, folderYear := parseFolderTitle(path.Base(sourceDir))
		year := folderYear
		if title == "" || title == "." {
			title = entry.Title
		}
		if year == 0 {
			year = entry.Year
		}
		for _, candidate := range nfoCandidates(sourceDir, kind, entry.Path) {
			data, readErr := coll.Storage.ReadFile(candidate)
			if readErr != nil {
				continue
			}
			local := collectionMetadata(candidate, data)
			if candidate == destination {
				provenance, provenanceErr := nfo.ReadProvenance(data)
				refreshAfter := opts.RefreshExistingAfter
				if refreshAfter <= 0 {
					refreshAfter = 24 * time.Hour
				}
				idMatches := ids["tmdb"] == "" || provenance.ProviderID == ids["tmdb"]
				policyMatches := provenance.Provider == "tmdb" && provenance.Language == opts.Language && provenance.OverwriteExisting == opts.OverwriteExisting && idMatches
				withinInterval := !provenance.UpdatedAt.IsZero() && time.Since(provenance.UpdatedAt) < refreshAfter
				episodesFresh := kind != provider.Show || !opts.FetchNFO || provenance.EpisodeMetadataChecked
				if !forceRefresh && provenanceErr == nil && policyMatches && withinInterval && episodesFresh {
					metadataFresh = true
					artworkFresh = provenance.ArtworkChecked
				}
			}
			for name, id := range local.IDs {
				if ids == nil {
					ids = map[string]string{}
				}
				if ids[name] == "" {
					ids[name] = id
				}
			}
			if title == "" {
				title = local.Title
			}
			if year == 0 {
				year = local.Year
			}
		}
		if metadataFresh && (!opts.FetchArtwork || artworkFresh) {
			report.Items = append(report.Items, ItemReport{Source: entry.Path, Destination: destination, ProviderID: ids["tmdb"], State: "fresh", Reason: "metadata was refreshed within the configured interval"})
			continue
		}
		if title == "" {
			title = strings.TrimSpace(path.Base(sourceDir))
		}
		resolution, resolveErr := provider.Resolve(ctx, p, kind, title, year, ids, opts.Language, opts.MinimumConfidence, opts.TieMargin)
		item := ItemReport{Source: entry.Path, Destination: destination, Evidence: resolution.Evidence}
		if resolution.Match != nil {
			item.ProviderID = resolution.Match.Candidate.ID
			item.Confidence = resolution.Match.Confidence
		}
		if resolveErr != nil {
			item.State = "failed"
			item.Reason = resolveErr.Error()
			report.Items = append(report.Items, item)
			continue
		}
		if resolution.NeedsReview {
			item.State = "needs_review"
			item.Reason = "identity is ambiguous or below the configured confidence threshold"
			report.Items = append(report.Items, item)
			continue
		}
		if resolution.Match == nil && resolution.Metadata.ID != "" {
			resolution.Match = &provider.Match{Candidate: provider.Candidate{ID: resolution.Metadata.ID}, Confidence: 1}
		}
		resolution.Metadata = metadataFallback(ctx, p, kind, resolution.Metadata, opts.Language, opts.FallbackLanguages)
		if !opts.Write {
			proposed := resolution.Metadata
			item.ProviderMetadata = &proposed
		}
		if opts.FetchNFO {
			confidence := 1.0
			if resolution.Match != nil {
				confidence = resolution.Match.Confidence
			}
			prov := nfo.Provenance{Provider: resolution.Metadata.Provider, ProviderID: resolution.Metadata.ID, Language: opts.Language, OverwriteExisting: opts.OverwriteExisting, Confidence: confidence, Evidence: resolution.Evidence, UpdatedAt: time.Now().UTC(), ArtworkChecked: artworkFresh || !opts.FetchArtwork, EpisodeMetadataChecked: kind != provider.Show || !opts.FetchNFO}
			if kind == provider.Show {
				episodeActions := refreshEpisodeNFOs(ctx, coll, inventory, sourceDir, resolution.Metadata, p, opts, prov)
				item.Files = append(item.Files, episodeActions...)
				prov.EpisodeMetadataChecked = artworkActionsComplete(episodeActions)
			}
			if opts.FetchArtwork && !artworkFresh {
				artActions := refreshArtwork(ctx, coll, inventory, sourceDir, resolution.Metadata, p, opts)
				item.Files = append(item.Files, artActions...)
				prov.ArtworkChecked = artworkActionsComplete(artActions)
			} else if opts.FetchArtwork {
				item.Files = append(item.Files, FileAction{Destination: sourceDir, State: "unchanged", Reason: "artwork was refreshed within the configured interval"})
			}
			item.Files = append(item.Files, refreshNFO(ctx, coll, destination, resolution.Metadata, opts, prov)...)
		} else if opts.FetchArtwork && !artworkFresh {
			item.Files = append(item.Files, refreshArtwork(ctx, coll, inventory, sourceDir, resolution.Metadata, p, opts)...)
		}
		item.State = aggregateState(item.Files)
		if len(item.Files) == 0 {
			item.State = "matched"
			item.Reason = "NFO and artwork fetching are disabled"
		}
		report.Items = append(report.Items, item)
	}
	if opts.Journal != nil {
		if err := recordMetadataOperations(ctx, opts.Journal, coll.ID, report.Items, inventory, opts, worker, leaseKeeper); err != nil {
			return report, err
		}
	}
	return report, nil
}

func metadataFallback(ctx context.Context, p provider.Provider, kind provider.MediaType, metadata provider.Metadata, primary string, languages []string) provider.Metadata {
	if metadata.ID == "" || len(languages) == 0 {
		return metadata
	}
	for _, language := range languages {
		language = strings.TrimSpace(language)
		if language == "" || strings.EqualFold(language, primary) {
			continue
		}
		needsFallback := metadata.Title == "" || metadata.OriginalTitle == "" || metadata.SortTitle == "" || metadata.Year == 0 || metadata.ReleaseDate.IsZero() || metadata.Overview == "" || len(metadata.Genres) == 0 || len(metadata.Studios) == 0 || metadata.RuntimeMinutes == 0 || metadata.Rating == 0 || metadata.ContentRating == "" || len(metadata.Directors) == 0 || len(metadata.Writers) == 0 || len(metadata.Actors) == 0 || metadata.PosterPath == "" || metadata.BackdropPath == ""
		if !needsFallback || ctx.Err() != nil {
			break
		}
		fallback, err := p.Get(ctx, kind, metadata.ID, language)
		if err != nil {
			continue
		}
		if metadata.Title == "" {
			metadata.Title = fallback.Title
		}
		if metadata.OriginalTitle == "" {
			metadata.OriginalTitle = fallback.OriginalTitle
		}
		if metadata.SortTitle == "" {
			metadata.SortTitle = fallback.SortTitle
		}
		if metadata.Year == 0 {
			metadata.Year = fallback.Year
		}
		if metadata.ReleaseDate.IsZero() {
			metadata.ReleaseDate = fallback.ReleaseDate
		}
		if metadata.Overview == "" {
			metadata.Overview = fallback.Overview
		}
		if len(metadata.Genres) == 0 {
			metadata.Genres = fallback.Genres
		}
		if len(metadata.Studios) == 0 {
			metadata.Studios = fallback.Studios
		}
		if metadata.RuntimeMinutes == 0 {
			metadata.RuntimeMinutes = fallback.RuntimeMinutes
		}
		if metadata.Rating == 0 {
			metadata.Rating = fallback.Rating
		}
		if metadata.ContentRating == "" {
			metadata.ContentRating = fallback.ContentRating
		}
		if len(metadata.Directors) == 0 {
			metadata.Directors = fallback.Directors
		}
		if len(metadata.Writers) == 0 {
			metadata.Writers = fallback.Writers
		}
		if len(metadata.Actors) == 0 {
			metadata.Actors = fallback.Actors
		}
		if metadata.PosterPath == "" {
			metadata.PosterPath = fallback.PosterPath
		}
		if metadata.BackdropPath == "" {
			metadata.BackdropPath = fallback.BackdropPath
		}
		for key, value := range fallback.ExternalIDs {
			if metadata.ExternalIDs == nil {
				metadata.ExternalIDs = map[string]string{}
			}
			if metadata.ExternalIDs[key] == "" {
				metadata.ExternalIDs[key] = value
			}
		}
	}
	return metadata
}

type metadataJournalEvidence struct {
	Signature     string          `json:"signature"`
	ConfigVersion int             `json:"config_version"`
	Configuration json.RawMessage `json:"configuration"`
	Detail        json.RawMessage `json:"detail,omitempty"`
}

const metadataJobConfigVersion = 1

func metadataJobConfiguration(opts Options) json.RawMessage {
	configuration, _ := json.Marshal(struct {
		InboxOnly                                                   bool
		FetchNFO, FetchArtwork, OverwriteExisting, OverwriteArtwork bool
		Language                                                    string
		FallbackLanguages                                           []string
		ArtworkMaxBytes                                             int64
		ArtworkMaxWidth, ArtworkMaxHeight                           int
		ArtworkFormats                                              []string
		ArtworkPosterSize, ArtworkBackdropSize                      string
		MinimumConfidence, TieMargin                                float64
		RefreshExistingAfter                                        time.Duration
	}{opts.InboxOnly, opts.FetchNFO, opts.FetchArtwork, opts.OverwriteExisting, opts.OverwriteArtwork, opts.Language, opts.FallbackLanguages, opts.ArtworkMaxBytes, opts.ArtworkMaxWidth, opts.ArtworkMaxHeight, opts.ArtworkFormats, opts.ArtworkPosterSize, opts.ArtworkBackdropSize, opts.MinimumConfidence, opts.TieMargin, opts.RefreshExistingAfter})
	return configuration
}

func metadataJobSignature(entry collection.InventoryEntry, opts Options) string {
	config, _ := json.Marshal(struct {
		InboxOnly                                                   bool
		FetchNFO, FetchArtwork, OverwriteExisting, OverwriteArtwork bool
		Language                                                    string
		FallbackLanguages                                           []string
		ArtworkMaxBytes                                             int64
		ArtworkMaxWidth, ArtworkMaxHeight                           int
		ArtworkFormats                                              []string
		ArtworkPosterSize, ArtworkBackdropSize                      string
		MinimumConfidence, TieMargin                                float64
		RefreshAfter                                                int64
	}{opts.InboxOnly, opts.FetchNFO, opts.FetchArtwork, opts.OverwriteExisting, opts.OverwriteArtwork, opts.Language, opts.FallbackLanguages, opts.ArtworkMaxBytes, opts.ArtworkMaxWidth, opts.ArtworkMaxHeight, opts.ArtworkFormats, opts.ArtworkPosterSize, opts.ArtworkBackdropSize, opts.MinimumConfidence, opts.TieMargin, int64(opts.RefreshExistingAfter)})
	data, _ := json.Marshal(struct {
		Path    string
		Size    int64
		ModTime int64
		Config  string
	}{entry.Path, entry.Size, entry.ModTime, string(config)})
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func journalSignature(evidence string) string {
	var parsed metadataJournalEvidence
	if json.Unmarshal([]byte(evidence), &parsed) != nil {
		return ""
	}
	return parsed.Signature
}

func recordMetadataOperations(ctx context.Context, journal database.MediaOperationRepo, collectionID string, items []ItemReport, inventory []collection.InventoryEntry, opts Options, worker string, leaseKeeper *database.MediaOperationLeaseKeeper) error {
	entries := make(map[string]collection.InventoryEntry, len(inventory))
	for _, entry := range inventory {
		if entry.Role == collection.InventoryVideo || entry.Role == collection.InventoryDisc && collection.IsMovieDiscStructure(entry, inventory) {
			entries[entry.Path] = entry
		}
	}
	for _, item := range items {
		if item.State == "deferred" || item.State == "failed_terminal" || item.State == "fresh" && item.Reason == "refresh interval has not elapsed" {
			continue
		}
		entry, ok := entries[item.Source]
		if !ok {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		identity := collectionID + "\x00" + item.Source + "\x00metadata_refresh"
		hash := sha256.Sum256([]byte(identity))
		op := model.MediaOperation{ID: hex.EncodeToString(hash[:]), CollectionID: collectionID, ItemKey: item.Source, SourcePath: item.Source, Action: "metadata_refresh", ProviderID: item.ProviderID, Confidence: item.Confidence, Evidence: marshalMetadataEvidence(metadataJobSignature(entry, opts), opts, item), LeaseOwner: worker, CreatedAt: time.Now().UTC()}
		if old, err := journal.GetMediaOperation(ctx, collectionID, item.Source, op.Action); err != nil {
			return err
		} else if old != nil {
			op.CreatedAt, op.Attempts = old.CreatedAt, old.Attempts
		}
		now := time.Now().UTC()
		op.UpdatedAt, op.NextAttempt, op.LeaseUntil = now, now, now
		switch item.State {
		case "needs_review":
			op.Status = "needs_review"
			op.Error = sanitizeJobError(item.Reason)
		case "failed":
			if op.Attempts >= 8 {
				op.Status = "failed_terminal"
			} else {
				op.Status = "failed_retryable"
				op.NextAttempt = retryAtWithJitter(now, op.Attempts)
			}
			op.Error = sanitizeJobError(item.Reason)
		case "planned":
			op.Status = "planned"
		default:
			op.Status = "complete"
		}
		if err := journal.UpsertMediaOperation(ctx, &op); err != nil {
			return err
		}
		leaseKeeper.Done(op.ID)
	}
	return nil
}

func retryAtWithJitter(now time.Time, attempts int) time.Time {
	delay := time.Duration(math.Min(math.Pow(2, float64(attempts)), 360)) * time.Minute
	jitter := time.Duration(rand.Int64N(int64(delay/4) + 1))
	return now.Add(delay + jitter)
}

func beginMetadataOperation(ctx context.Context, journal database.MediaOperationRepo, collectionID string, entry collection.InventoryEntry, opts Options, old *model.MediaOperation, worker string, leaseKeeper *database.MediaOperationLeaseKeeper) (bool, error) {
	now := time.Now().UTC()
	identity := collectionID + "\x00" + entry.Path + "\x00metadata_refresh"
	hash := sha256.Sum256([]byte(identity))
	op := model.MediaOperation{ID: hex.EncodeToString(hash[:]), CollectionID: collectionID, ItemKey: entry.Path, SourcePath: entry.Path, Action: "metadata_refresh", Status: "planned", Evidence: marshalMetadataEvidence(metadataJobSignature(entry, opts), opts, ItemReport{Source: entry.Path}), LeaseUntil: now, NextAttempt: now, CreatedAt: now, UpdatedAt: now}
	if old != nil {
		op.CreatedAt, op.Attempts = old.CreatedAt, old.Attempts
	}
	if err := journal.UpsertMediaOperation(ctx, &op); err != nil {
		return false, err
	}
	claimed, err := journal.ClaimMediaOperation(ctx, op.ID, worker, now, now.Add(15*time.Minute))
	if err == nil && claimed {
		leaseKeeper.Track(op.ID)
	}
	return claimed, err
}

func marshalMetadataEvidence(signature string, opts Options, item ItemReport) string {
	b, _ := json.Marshal(struct {
		Signature     string          `json:"signature"`
		ConfigVersion int             `json:"config_version"`
		Configuration json.RawMessage `json:"configuration"`
		Detail        ItemReport      `json:"detail"`
	}{signature, metadataJobConfigVersion, metadataJobConfiguration(opts), item})
	return string(b)
}

var jobURL = regexp.MustCompile(`https?://[^\s"']+`)

func sanitizeJobError(value string) string {
	value = jobURL.ReplaceAllString(value, "[redacted URL]")
	if len(value) > 2048 {
		value = value[:2048]
	}
	return value
}

func refreshNFO(ctx context.Context, coll *collection.Collection, destination string, meta provider.Metadata, opts Options, provenance nfo.Provenance) []FileAction {
	if err := ctx.Err(); err != nil {
		return []FileAction{{Destination: destination, State: "failed", Reason: err.Error()}}
	}
	existing, readErr := coll.Storage.ReadFile(destination)
	if readErr != nil && !rclonestore.IsNotFound(readErr) {
		return []FileAction{{Destination: destination, State: "failed", Reason: "read existing NFO: " + readErr.Error()}}
	}
	merged, err := nfo.MergeWithProvenance(existing, meta, opts.OverwriteExisting, provenance)
	if err != nil {
		return []FileAction{{Destination: destination, State: "needs_review", Reason: err.Error()}}
	}
	if bytes.Equal(existing, merged) {
		return []FileAction{{Destination: destination, State: "unchanged"}}
	}
	if !opts.Write {
		return []FileAction{{Destination: destination, State: "planned", Reason: "would create or merge metadata NFO"}}
	}
	if err := coll.Storage.WriteSidecar(destination, merged, true); err != nil {
		return []FileAction{{Destination: destination, State: "failed", Reason: err.Error()}}
	}
	return []FileAction{{Destination: destination, State: "written"}}
}

// refreshEpisodeNFOs writes authoritative episode metadata beside TV files.
// It accepts standard season/episode and uniquely mapped air-date identities;
// unsupported numbering remains visible for review.
func refreshEpisodeNFOs(ctx context.Context, coll *collection.Collection, inventory []collection.InventoryEntry, showDir string, show provider.Metadata, p provider.Provider, opts Options, provenance nfo.Provenance) []FileAction {
	seasonProvider, ok := p.(provider.SeasonEpisodeProvider)
	if !ok {
		return []FileAction{{Destination: showDir, State: "needs_review", Reason: "provider does not expose authoritative episode metadata"}}
	}
	seasons := map[int][]provider.EpisodeMetadata{}
	seasonMetadata := map[int]provider.SeasonMetadata{}
	seasonDirectories := map[int]string{}
	seasonErrors := map[int]error{}
	detailsProvider, hasSeasonDetails := p.(provider.SeasonProvider)
	var actions []FileAction
	for _, entry := range inventory {
		if entry.Role != collection.InventoryVideo || (path.Dir(entry.Path) != showDir && !strings.HasPrefix(entry.Path, showDir+"/")) {
			continue
		}
		season, validDir := metadataSeasonNumber(path.Base(path.Dir(entry.Path)))
		if !validDir {
			continue
		}
		seasonDirectories[season] = path.Dir(entry.Path)
		if _, loaded := seasons[season]; loaded || seasonErrors[season] != nil {
			continue
		}
		if hasSeasonDetails {
			details, err := getSeasonDetails(ctx, detailsProvider, show.ID, season, opts.Language, opts.FallbackLanguages)
			if err != nil {
				seasonErrors[season] = err
				continue
			}
			seasons[season] = details.Episodes
			seasonMetadata[season] = details.Metadata
		} else {
			rows, err := getEpisodeRows(ctx, seasonProvider, show.ID, season, opts.Language, opts.FallbackLanguages)
			if err != nil {
				seasonErrors[season] = err
				continue
			}
			seasons[season] = rows
		}
	}
	for _, entry := range inventory {
		if entry.Role != collection.InventoryVideo || (path.Dir(entry.Path) != showDir && !strings.HasPrefix(entry.Path, showDir+"/")) {
			continue
		}
		season, validDir := metadataSeasonNumber(path.Base(path.Dir(entry.Path)))
		if !validDir {
			actions = append(actions, FileAction{Destination: entry.Path, State: "needs_review", Reason: "episode file is outside a recognized season directory"})
			continue
		}
		if len(entry.Episodes) == 0 && entry.AirDate == "" || len(entry.Episodes) > 8 {
			actions = append(actions, FileAction{Destination: entry.Path, State: "needs_review", Reason: "episode numbering is missing or ambiguous"})
			continue
		}
		if entry.Season > 0 && entry.Season != season {
			actions = append(actions, FileAction{Destination: entry.Path, State: "needs_review", Reason: "filename season conflicts with its season directory"})
			continue
		}
		if err := seasonErrors[season]; err != nil {
			actions = append(actions, FileAction{Destination: entry.Path, State: "failed", Reason: "load provider season metadata: " + err.Error()})
			continue
		}
		numbers := append([]int(nil), entry.Episodes...)
		if len(numbers) == 0 {
			episodeNumber := 0
			for _, candidate := range seasons[season] {
				if candidate.AirDate == entry.AirDate {
					if episodeNumber != 0 {
						episodeNumber = -1
						break
					}
					episodeNumber = candidate.EpisodeNumber
				}
			}
			if episodeNumber > 0 {
				numbers = append(numbers, episodeNumber)
			}
		}
		if len(numbers) == 0 {
			actions = append(actions, FileAction{Destination: entry.Path, State: "needs_review", Reason: "episode identity does not map to exactly one provider episode"})
			continue
		}
		validNumbers := len(numbers) > 0 && len(numbers) <= 8
		for i, number := range numbers {
			if number <= 0 || (i > 0 && number <= numbers[i-1]) {
				validNumbers = false
			}
		}
		if !validNumbers {
			actions = append(actions, FileAction{Destination: entry.Path, State: "needs_review", Reason: "multi-episode numbering must be a strictly increasing list of at most eight episodes"})
			continue
		}
		matchedEpisodes := make([]provider.EpisodeMetadata, 0, len(numbers))
		for _, episodeNumber := range numbers {
			var matched *provider.EpisodeMetadata
			for i := range seasons[season] {
				candidate := &seasons[season][i]
				if candidate.SeasonNumber == season && candidate.EpisodeNumber == episodeNumber {
					matched = candidate
					break
				}
			}
			if matched == nil || matched.Name == "" {
				matchedEpisodes = nil
				break
			}
			matchedEpisodes = append(matchedEpisodes, *matched)
		}
		if len(matchedEpisodes) != len(numbers) {
			actions = append(actions, FileAction{Destination: entry.Path, State: "needs_review", Reason: "provider has no authoritative title for every episode in this file"})
			continue
		}
		for _, matched := range matchedEpisodes {
			destination := episodeNFOPath(entry.Path, season, matched.EpisodeNumber, len(numbers) > 1)
			actions = append(actions, refreshEpisodeNFO(ctx, coll, destination, season, matched, opts, provenance))
		}
	}
	if hasSeasonDetails {
		seasonNumbers := make([]int, 0, len(seasonMetadata))
		for season := range seasonMetadata {
			seasonNumbers = append(seasonNumbers, season)
		}
		sort.Ints(seasonNumbers)
		for _, season := range seasonNumbers {
			seasonData := seasonMetadata[season]
			if seasonData.Name == "" && seasonData.Overview == "" && seasonData.AirDate == "" {
				actions = append(actions, FileAction{Destination: seasonDirectories[season], State: "needs_review", Reason: "provider returned no season title, overview, or air date"})
				continue
			}
			seasonMedia := provider.Metadata{Provider: "tmdb", Type: provider.Season, ID: seasonData.ID, Title: seasonData.Name, SortTitle: seasonData.Name, Overview: seasonData.Overview, Rating: seasonData.Rating, Studios: seasonData.Networks, SeasonNumber: season, ExternalIDs: map[string]string{}}
			if seasonData.ID != "" {
				seasonMedia.ExternalIDs["tmdb_season"] = seasonData.ID
			}
			if date, err := time.Parse("2006-01-02", seasonData.AirDate); err == nil {
				seasonMedia.ReleaseDate, seasonMedia.Year = date, date.Year()
			}
			seasonProv := provenance
			seasonProv.ProviderID = seasonData.ID
			seasonProv.Evidence = append(append([]string(nil), provenance.Evidence...), fmt.Sprintf("TMDb season %d", season))
			seasonDir := seasonDirectories[season]
			seasonActions := refreshNFO(ctx, coll, path.Join(seasonDir, "season.nfo"), seasonMedia, opts, seasonProv)
			actions = append(actions, seasonActions...)
			if opts.FetchArtwork && seasonData.PosterPath != "" {
				seasonMedia.PosterPath = seasonData.PosterPath
				artworkActions := refreshArtwork(ctx, coll, inventory, seasonDir, seasonMedia, p, opts)
				actions = append(actions, artworkActions...)
			}
		}
	}
	return actions
}

func getSeasonDetails(ctx context.Context, seasonProvider provider.SeasonProvider, showID string, season int, primary string, fallbacks []string) (provider.SeasonDetails, error) {
	if primary == "" {
		primary = "en-US"
	}
	languages := append([]string{primary}, fallbacks...)
	var merged provider.SeasonDetails
	var lastErr error
	seen := map[string]bool{}
	loaded := false
	for _, language := range languages {
		language = strings.TrimSpace(language)
		if language == "" || seen[strings.ToLower(language)] {
			continue
		}
		seen[strings.ToLower(language)] = true
		details, err := seasonProvider.GetSeason(ctx, showID, season, language)
		if err != nil {
			lastErr = err
			continue
		}
		if !loaded {
			merged = details
			loaded = true
			continue
		}
		merged = mergeSeasonDetails(merged, details)
	}
	if !loaded {
		return provider.SeasonDetails{}, lastErr
	}
	return merged, nil
}

func getEpisodeRows(ctx context.Context, seasonProvider provider.SeasonEpisodeProvider, showID string, season int, primary string, fallbacks []string) ([]provider.EpisodeMetadata, error) {
	if primary == "" {
		primary = "en-US"
	}
	languages := append([]string{primary}, fallbacks...)
	var merged []provider.EpisodeMetadata
	var lastErr error
	seen := map[string]bool{}
	loaded := false
	for _, language := range languages {
		language = strings.TrimSpace(language)
		if language == "" || seen[strings.ToLower(language)] {
			continue
		}
		seen[strings.ToLower(language)] = true
		rows, err := seasonProvider.GetSeasonEpisodes(ctx, showID, season, language)
		if err != nil {
			lastErr = err
			continue
		}
		if !loaded {
			merged = append(merged, rows...)
			loaded = true
			continue
		}
		merged = mergeEpisodeRows(merged, rows)
	}
	if !loaded {
		return nil, lastErr
	}
	return merged, nil
}

func mergeSeasonDetails(primary, fallback provider.SeasonDetails) provider.SeasonDetails {
	if primary.Metadata.ID == "" {
		primary.Metadata.ID = fallback.Metadata.ID
	}
	if primary.Metadata.Name == "" {
		primary.Metadata.Name = fallback.Metadata.Name
	}
	if primary.Metadata.AirDate == "" {
		primary.Metadata.AirDate = fallback.Metadata.AirDate
	}
	if primary.Metadata.Overview == "" {
		primary.Metadata.Overview = fallback.Metadata.Overview
	}
	if primary.Metadata.PosterPath == "" {
		primary.Metadata.PosterPath = fallback.Metadata.PosterPath
	}
	if primary.Metadata.Rating == 0 {
		primary.Metadata.Rating = fallback.Metadata.Rating
	}
	primary.Metadata.Networks = mergeUniqueNames(primary.Metadata.Networks, fallback.Metadata.Networks)
	primary.Episodes = mergeEpisodeRows(primary.Episodes, fallback.Episodes)
	return primary
}

func mergeEpisodeRows(primary, fallback []provider.EpisodeMetadata) []provider.EpisodeMetadata {
	merged := append([]provider.EpisodeMetadata(nil), primary...)
	indices := make(map[int]int, len(merged))
	for i := range merged {
		indices[merged[i].EpisodeNumber] = i
	}
	for _, row := range fallback {
		if i, ok := indices[row.EpisodeNumber]; ok {
			if merged[i].ID == "" {
				merged[i].ID = row.ID
			}
			if merged[i].Name == "" {
				merged[i].Name = row.Name
			}
			if merged[i].AirDate == "" {
				merged[i].AirDate = row.AirDate
			}
			if merged[i].Overview == "" {
				merged[i].Overview = row.Overview
			}
			if merged[i].RuntimeMinutes == 0 {
				merged[i].RuntimeMinutes = row.RuntimeMinutes
			}
			if merged[i].Rating == 0 {
				merged[i].Rating = row.Rating
			}
			merged[i].Directors = mergeUniqueNames(merged[i].Directors, row.Directors)
			merged[i].Writers = mergeUniqueNames(merged[i].Writers, row.Writers)
			if merged[i].Actors == nil && len(row.Actors) > 0 {
				merged[i].Actors = map[string]string{}
			}
			for name, role := range row.Actors {
				if _, ok := merged[i].Actors[name]; !ok {
					merged[i].Actors[name] = role
				}
			}
		} else {
			indices[row.EpisodeNumber] = len(merged)
			merged = append(merged, row)
		}
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].EpisodeNumber < merged[j].EpisodeNumber })
	return merged
}

func mergeUniqueNames(primary, fallback []string) []string {
	merged := append([]string(nil), primary...)
	for _, name := range fallback {
		found := false
		for _, existing := range merged {
			if strings.EqualFold(existing, name) {
				found = true
				break
			}
		}
		if !found && strings.TrimSpace(name) != "" {
			merged = append(merged, name)
		}
	}
	return merged
}

func episodeNFOPath(videoPath string, season, episode int, multi bool) string {
	stem := strings.TrimSuffix(path.Base(videoPath), path.Ext(videoPath))
	if multi {
		stem += fmt.Sprintf("-S%02dE%02d", season, episode)
	}
	return path.Join(path.Dir(videoPath), stem+".nfo")
}

func refreshEpisodeNFO(ctx context.Context, coll *collection.Collection, destination string, season int, episode provider.EpisodeMetadata, opts Options, provenance nfo.Provenance) FileAction {
	metadata := provider.Metadata{Provider: "tmdb", Type: provider.Episode, ID: episode.ID, Title: episode.Name, SortTitle: episode.Name, Overview: episode.Overview, RuntimeMinutes: episode.RuntimeMinutes, Rating: episode.Rating, Directors: episode.Directors, Writers: episode.Writers, Actors: episode.Actors, SeasonNumber: season, EpisodeNumber: episode.EpisodeNumber, ExternalIDs: map[string]string{}}
	if episode.ID != "" {
		metadata.ExternalIDs["tmdb_episode"] = episode.ID
	}
	if date, err := time.Parse("2006-01-02", episode.AirDate); err == nil {
		metadata.ReleaseDate, metadata.Year = date, date.Year()
	}
	existing, readErr := coll.Storage.ReadFile(destination)
	if readErr != nil && !rclonestore.IsNotFound(readErr) {
		return FileAction{Destination: destination, State: "failed", Reason: "read existing episode NFO: " + readErr.Error()}
	}
	episodeProvenance := provenance
	episodeProvenance.ProviderID = episode.ID
	episodeProvenance.Evidence = append(append([]string(nil), provenance.Evidence...), fmt.Sprintf("TMDb season %d episode %d", season, episode.EpisodeNumber))
	merged, err := nfo.MergeWithProvenance(existing, metadata, opts.OverwriteExisting, episodeProvenance)
	if err != nil {
		return FileAction{Destination: destination, State: "needs_review", Reason: err.Error()}
	}
	if bytes.Equal(existing, merged) {
		return FileAction{Destination: destination, State: "unchanged"}
	}
	if !opts.Write {
		return FileAction{Destination: destination, State: "planned", Reason: "would create or merge episode NFO"}
	}
	if err := coll.Storage.WriteSidecar(destination, merged, true); err != nil {
		return FileAction{Destination: destination, State: "failed", Reason: err.Error()}
	}
	return FileAction{Destination: destination, State: "written"}
}

var metadataSeasonName = regexp.MustCompile(`(?i)^(?:season\s*0*(\d+)|s0*(\d+))$`)

func metadataSeasonNumber(name string) (int, bool) {
	if strings.EqualFold(name, "specials") {
		return 0, true
	}
	match := metadataSeasonName.FindStringSubmatch(name)
	if len(match) == 0 {
		return 0, false
	}
	value := match[1]
	if value == "" {
		value = match[2]
	}
	number := 0
	for _, digit := range value {
		number = number*10 + int(digit-'0')
	}
	return number, true
}

func refreshArtwork(ctx context.Context, coll *collection.Collection, inventory []collection.InventoryEntry, dir string, meta provider.Metadata, p provider.Provider, opts Options) []FileAction {
	art, ok := p.(provider.ArtworkProvider)
	if !ok {
		return []FileAction{{State: "skipped", Reason: "provider does not support artwork downloads"}}
	}
	var actions []FileAction
	posterSize, backdropSize := opts.ArtworkPosterSize, opts.ArtworkBackdropSize
	if posterSize == "" {
		posterSize = "w500"
	}
	if backdropSize == "" {
		backdropSize = "w780"
	}
	for _, artwork := range []struct{ kind, path, size string }{{"poster", meta.PosterPath, posterSize}, {"backdrop", meta.BackdropPath, backdropSize}} {
		if artwork.path == "" {
			continue
		}
		destination := path.Join(dir, artwork.kind+".jpg")
		present := false
		for _, entry := range inventory {
			if path.Dir(entry.Path) == dir && entry.Role == collection.InventoryArtwork {
				n := strings.ToLower(strings.TrimSuffix(path.Base(entry.Path), path.Ext(entry.Path)))
				if n == artwork.kind || (artwork.kind == "poster" && (n == "folder" || n == "cover" || n == "movie")) || (artwork.kind == "backdrop" && (n == "fanart" || n == "background")) {
					present = true
					break
				}
			}
		}
		if present && !opts.OverwriteArtwork {
			actions = append(actions, FileAction{Destination: destination, State: "skipped", Reason: "local artwork already exists"})
			continue
		}
		if !opts.Write {
			actions = append(actions, FileAction{Destination: destination, State: "planned", Reason: "would download and validate provider artwork"})
			continue
		}
		data, mime, err := art.DownloadArtwork(ctx, artwork.path, artwork.size, opts.ArtworkMaxBytes)
		if err != nil {
			state := "failed"
			message := strings.ToLower(err.Error())
			if strings.Contains(message, "exceeds") || strings.Contains(message, "invalid tmdb image") || strings.Contains(message, "unsupported tmdb image format") || strings.Contains(message, "content-type") || strings.Contains(message, "image signature") {
				state = "needs_review"
			}
			actions = append(actions, FileAction{Destination: destination, State: state, Reason: err.Error()})
			continue
		}
		format := strings.TrimPrefix(strings.ToLower(mime), "image/")
		if format == "jpg" {
			format = "jpeg"
		}
		if !allowedArtworkFormat(format, opts.ArtworkFormats) {
			actions = append(actions, FileAction{Destination: destination, State: "needs_review", Reason: "provider artwork format is not allowed by library policy"})
			continue
		}
		config, _, decodeErr := image.DecodeConfig(bytes.NewReader(data))
		if decodeErr != nil {
			actions = append(actions, FileAction{Destination: destination, State: "needs_review", Reason: "provider artwork failed image dimension validation"})
			continue
		}
		if opts.ArtworkMaxWidth > 0 && config.Width > opts.ArtworkMaxWidth || opts.ArtworkMaxHeight > 0 && config.Height > opts.ArtworkMaxHeight {
			actions = append(actions, FileAction{Destination: destination, State: "needs_review", Reason: "provider artwork exceeds configured dimensions"})
			continue
		}
		if format == "png" {
			destination = path.Join(dir, artwork.kind+".png")
		}
		if err := coll.Storage.WriteSidecar(destination, data, opts.OverwriteArtwork); err != nil {
			actions = append(actions, FileAction{Destination: destination, State: "failed", Reason: err.Error()})
		} else {
			actions = append(actions, FileAction{Destination: destination, State: "written"})
		}
	}
	return actions
}

func allowedArtworkFormat(format string, configured []string) bool {
	if len(configured) == 0 {
		return format == "jpeg" || format == "png"
	}
	for _, value := range configured {
		value = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(value)), ".")
		if value == "jpg" {
			value = "jpeg"
		}
		if value == format {
			return true
		}
	}
	return false
}

func aggregateState(actions []FileAction) string {
	if len(actions) == 0 {
		return "matched"
	}
	state := "unchanged"
	for _, action := range actions {
		switch action.State {
		case "failed":
			return "failed"
		case "needs_review":
			return "needs_review"
		case "written":
			state = "written"
		case "planned":
			if state != "written" {
				state = "planned"
			}
		case "skipped":
			if state == "unchanged" {
				state = "skipped"
			}
		}
	}
	return state
}

func artworkActionsComplete(actions []FileAction) bool {
	for _, action := range actions {
		if action.State == "failed" || action.State == "needs_review" || action.State == "planned" {
			return false
		}
	}
	return true
}

type localMetadata struct {
	Title string
	Year  int
	IDs   map[string]string
}

func collectionMetadata(filename string, data []byte) localMetadata {
	n := collectionmetadata.NewNfoFromData(filename, data)
	return localMetadata{Title: n.Title(), Year: n.Year(), IDs: n.ProviderIDs()}
}
func nfoCandidates(dir string, kind provider.MediaType, video string) []string {
	if kind == provider.Show {
		return []string{path.Join(dir, "tvshow.nfo")}
	}
	base := strings.TrimSuffix(path.Base(video), path.Ext(video))
	return []string{path.Join(path.Dir(video), base+".nfo"), path.Join(dir, "movie.nfo")}
}

var folderYearPattern = regexp.MustCompile(`\s*\((19\d{2}|20\d{2})\)\s*$`)
var folderIDPattern = regexp.MustCompile(`\s*\[(?:tmdb|tvdb|imdb)id-[^]]+\]\s*$`)
var metadataSeasonDirectory = regexp.MustCompile(`(?i)^(?:season\s*0*\d+|s0*\d+|specials)$`)

func parseFolderTitle(name string) (string, int) {
	name = folderIDPattern.ReplaceAllString(name, "")
	year := 0
	if match := folderYearPattern.FindStringSubmatch(name); len(match) > 0 {
		year = 0
		for _, digit := range match[1] {
			year = year*10 + int(digit-'0')
		}
		name = folderYearPattern.ReplaceAllString(name, "")
	}
	name = strings.NewReplacer(".", " ", "_", " ").Replace(name)
	return strings.Join(strings.Fields(name), " "), year
}
