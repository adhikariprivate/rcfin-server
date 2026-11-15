package subtitles

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"

	"github.com/adhikariprivate/rcfin-server/internal/collection"
	"github.com/adhikariprivate/rcfin-server/internal/database"
	"github.com/adhikariprivate/rcfin-server/internal/database/model"
)

type RefreshOptions struct {
	InboxOnly         bool
	ItemPaths         []string
	Journal           database.MediaOperationRepo
	Languages         []string
	Formats           []string
	HearingImpaired   bool
	Forced            bool
	OverwriteExisting bool
	Write             bool
	MinimumConfidence float64
}

type SubtitleAction struct {
	Source      string                   `json:"source"`
	Destination string                   `json:"destination,omitempty"`
	Language    string                   `json:"language"`
	ProviderID  string                   `json:"provider_id,omitempty"`
	Confidence  float64                  `json:"confidence,omitempty"`
	Candidate   *SubtitleCandidateReport `json:"candidate,omitempty"`
	State       string                   `json:"state"`
	Reason      string                   `json:"reason,omitempty"`
}

type SubtitleCandidateReport struct {
	Title           string `json:"title,omitempty"`
	Year            int    `json:"year,omitempty"`
	Language        string `json:"language"`
	Format          string `json:"format"`
	HearingImpaired bool   `json:"hearing_impaired"`
	Forced          bool   `json:"forced"`
	MovieHashMatch  bool   `json:"movie_hash_match"`
	Season          int    `json:"season,omitempty"`
	Episode         int    `json:"episode,omitempty"`
	TMDbID          string `json:"tmdb_id,omitempty"`
	IMDbID          string `json:"imdb_id,omitempty"`
	DownloadCount   int64  `json:"download_count,omitempty"`
}

type SubtitleReport struct {
	CollectionID string           `json:"collection_id"`
	Actions      []SubtitleAction `json:"actions"`
}

func Refresh(ctx context.Context, coll *collection.Collection, client *OpenSubtitles, opts RefreshOptions) (SubtitleReport, error) {
	if coll == nil || coll.Storage == nil || client == nil {
		return SubtitleReport{}, fmt.Errorf("collection storage and subtitle provider are required")
	}
	worker := database.NewMediaOperationWorkerID("subtitle-refresh")
	ctx, leaseKeeper, stopLeaseKeeper := database.StartMediaOperationLeaseKeeper(ctx, opts.Journal, worker, 15*time.Minute)
	defer stopLeaseKeeper()
	report := SubtitleReport{CollectionID: coll.ID}
	var journalErr error
	emit := func(action SubtitleAction, video collection.InventoryEntry, opts RefreshOptions, old *model.MediaOperation) {
		action.Reason = subtitleError(action.Reason)
		report.Actions = append(report.Actions, action)
		if opts.Journal != nil && action.State != "deferred" && action.Reason != "refresh interval has not elapsed" {
			if err := recordSubtitleOperation(ctx, opts.Journal, coll.ID, video, opts, old, action, worker, leaseKeeper); err != nil && journalErr == nil {
				journalErr = err
			}
		}
	}
	inventory, err := collection.InventoryCollection(ctx, coll, coll.InboxPath, opts.InboxOnly)
	if err != nil {
		return report, err
	}
	itemPaths := make(map[string]struct{}, len(opts.ItemPaths))
	for _, itemPath := range opts.ItemPaths {
		itemPaths[itemPath] = struct{}{}
	}
	for _, video := range inventory {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		if video.Role != collection.InventoryVideo {
			continue
		}
		if len(itemPaths) > 0 {
			if _, ok := itemPaths[video.Path]; !ok {
				continue
			}
		}
		for _, lang := range opts.Languages {
			var oldOperation *model.MediaOperation
			if opts.Journal != nil {
				actionName := "subtitle_refresh:" + strings.ToLower(lang)
				var journalLookupErr error
				oldOperation, journalLookupErr = opts.Journal.GetMediaOperation(ctx, coll.ID, video.Path, actionName)
				if journalLookupErr != nil {
					return report, journalLookupErr
				}
				if oldOperation != nil && subtitleJournalSignature(oldOperation.Evidence) == subtitleJobSignature(video, lang, opts) {
					switch oldOperation.Status {
					case "needs_review", "failed_terminal":
						report.Actions = append(report.Actions, SubtitleAction{Source: video.Path, Language: lang, ProviderID: oldOperation.ProviderID, Confidence: oldOperation.Confidence, State: oldOperation.Status, Reason: oldOperation.Error})
						continue
					case "failed_retryable":
						if time.Now().Before(oldOperation.NextAttempt) {
							report.Actions = append(report.Actions, SubtitleAction{Source: video.Path, Language: lang, State: "deferred", Reason: "retry backoff is active"})
							continue
						}
					case "complete":
						if time.Since(oldOperation.UpdatedAt) < 24*time.Hour {
							report.Actions = append(report.Actions, SubtitleAction{Source: video.Path, Language: lang, State: "skipped", Reason: "refresh interval has not elapsed"})
							continue
						}
					case "running":
						if time.Now().Before(oldOperation.LeaseUntil) {
							report.Actions = append(report.Actions, SubtitleAction{Source: video.Path, Language: lang, State: "deferred", Reason: "another worker holds the subtitle lease"})
							continue
						}
					}
				}
				claimed, err := beginSubtitleOperation(ctx, opts.Journal, coll.ID, video, lang, opts, oldOperation, worker, leaseKeeper)
				if err != nil {
					return report, err
				}
				if !claimed {
					report.Actions = append(report.Actions, SubtitleAction{Source: video.Path, Language: lang, State: "deferred", Reason: "another worker claimed the subtitle operation"})
					continue
				}
			}
			if video.IdentityConflict {
				emit(SubtitleAction{Source: video.Path, Language: lang, State: "needs_review", Reason: "provider IDs conflict between the path and local metadata"}, video, opts, oldOperation)
				continue
			}
			if !validLanguage(lang) {
				emit(SubtitleAction{Source: video.Path, Language: lang, State: "failed", Reason: "invalid configured language"}, video, opts, oldOperation)
				continue
			}
			if !opts.OverwriteExisting && hasMatchingSubtitle(inventory, video, lang, opts.Forced, opts.HearingImpaired) {
				emit(SubtitleAction{Source: video.Path, Language: lang, State: "skipped", Reason: "matching local subtitle already exists"}, video, opts, oldOperation)
				continue
			}
			if client.QuotaExceeded() {
				emit(SubtitleAction{Source: video.Path, Language: lang, State: "quota_exhausted", Reason: ErrDownloadQuotaExceeded.Error()}, video, opts, oldOperation)
				continue
			}
			query := makeSearchQuery(coll, video, lang)
			candidates, searchErr := searchCandidates(ctx, client, coll, video, query, opts)
			if searchErr != nil {
				state := "failed"
				if errors.Is(searchErr, ErrAuthentication) {
					state = "failed_terminal"
				}
				emit(SubtitleAction{Source: video.Path, Language: lang, State: state, Reason: searchErr.Error()}, video, opts, oldOperation)
				continue
			}
			selected, confidence, reason, ok := rankSubtitle(query, candidates, opts)
			if !ok {
				emit(SubtitleAction{Source: video.Path, Language: lang, ProviderID: selected.ID, Confidence: confidence, Candidate: subtitleCandidateReport(selected), State: "needs_review", Reason: reason}, video, opts, oldOperation)
				continue
			}
			ext := candidateExtension(selected)
			if !allowedFormat(ext, opts.Formats) {
				emit(SubtitleAction{Source: video.Path, Language: lang, State: "skipped", ProviderID: selected.ID, Confidence: confidence, Candidate: subtitleCandidateReport(selected), Reason: "best match has an unsupported subtitle format"}, video, opts, oldOperation)
				continue
			}
			destination := subtitleDestination(video.Path, lang, opts.Forced, opts.HearingImpaired, ext)
			action := SubtitleAction{Source: video.Path, Destination: destination, Language: lang, ProviderID: selected.ID, Confidence: confidence, Candidate: subtitleCandidateReport(selected)}
			if !opts.Write {
				action.State = "planned"
				action.Reason = "would download and validate matching subtitle"
				emit(action, video, opts, oldOperation)
				continue
			}
			data, _, downloadErr := client.Download(ctx, selected.FileID)
			if downloadErr != nil {
				action.State = "failed"
				if errors.Is(downloadErr, ErrDownloadQuotaExceeded) {
					action.State = "quota_exhausted"
				} else if errors.Is(downloadErr, ErrAuthentication) {
					action.State = "failed_terminal"
				}
				action.Reason = subtitleError(downloadErr.Error())
				emit(action, video, opts, oldOperation)
				continue
			}
			validated, validateErr := validateSubtitle(data, ext)
			if validateErr != nil {
				action.State = "needs_review"
				action.Reason = validateErr.Error()
				emit(action, video, opts, oldOperation)
				continue
			}
			if writeErr := coll.Storage.WriteSidecar(destination, validated, opts.OverwriteExisting); writeErr != nil {
				action.State = "failed"
				action.Reason = writeErr.Error()
			} else {
				action.State = "written"
			}
			emit(action, video, opts, oldOperation)
		}
	}
	if journalErr != nil {
		return report, journalErr
	}
	return report, nil
}

type subtitleJobEvidence struct {
	Signature     string          `json:"signature"`
	ConfigVersion int             `json:"config_version"`
	Configuration json.RawMessage `json:"configuration"`
	Detail        SubtitleAction  `json:"detail"`
}

const subtitleJobConfigVersion = 1

func subtitleJobConfiguration(opts RefreshOptions) json.RawMessage {
	configuration, _ := json.Marshal(struct {
		InboxOnly                                  bool
		Languages                                  []string
		Formats                                    []string
		HearingImpaired, Forced, OverwriteExisting bool
		MinimumConfidence                          float64
	}{opts.InboxOnly, opts.Languages, opts.Formats, opts.HearingImpaired, opts.Forced, opts.OverwriteExisting, opts.MinimumConfidence})
	return configuration
}

func subtitleJobSignature(video collection.InventoryEntry, language string, opts RefreshOptions) string {
	config, _ := json.Marshal(struct {
		Language                           string
		Formats                            []string
		HearingImpaired, Forced, Overwrite bool
		InboxOnly                          bool
	}{language, opts.Formats, opts.HearingImpaired, opts.Forced, opts.OverwriteExisting, opts.InboxOnly})
	data, _ := json.Marshal(struct {
		Path          string
		Size, ModTime int64
		Config        string
	}{video.Path, video.Size, video.ModTime, string(config)})
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func subtitleJournalSignature(evidence string) string {
	var value subtitleJobEvidence
	if json.Unmarshal([]byte(evidence), &value) != nil {
		return ""
	}
	return value.Signature
}

func beginSubtitleOperation(ctx context.Context, journal database.MediaOperationRepo, collectionID string, video collection.InventoryEntry, language string, opts RefreshOptions, old *model.MediaOperation, worker string, leaseKeeper *database.MediaOperationLeaseKeeper) (bool, error) {
	action := "subtitle_refresh:" + strings.ToLower(language)
	identity := collectionID + "\x00" + video.Path + "\x00" + action
	hash := sha256.Sum256([]byte(identity))
	now := time.Now().UTC()
	encoded, _ := json.Marshal(subtitleJobEvidence{Signature: subtitleJobSignature(video, language, opts), ConfigVersion: subtitleJobConfigVersion, Configuration: subtitleJobConfiguration(opts)})
	op := model.MediaOperation{ID: hex.EncodeToString(hash[:]), CollectionID: collectionID, ItemKey: video.Path, SourcePath: video.Path, Action: action, Status: "planned", Evidence: string(encoded), LeaseUntil: now, NextAttempt: now, CreatedAt: now, UpdatedAt: now}
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

func recordSubtitleOperation(ctx context.Context, journal database.MediaOperationRepo, collectionID string, video collection.InventoryEntry, opts RefreshOptions, old *model.MediaOperation, action SubtitleAction, worker string, leaseKeeper *database.MediaOperationLeaseKeeper) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	name := "subtitle_refresh:" + strings.ToLower(action.Language)
	identity := collectionID + "\x00" + video.Path + "\x00" + name
	hash := sha256.Sum256([]byte(identity))
	now := time.Now().UTC()
	encoded, _ := json.Marshal(subtitleJobEvidence{Signature: subtitleJobSignature(video, action.Language, opts), ConfigVersion: subtitleJobConfigVersion, Configuration: subtitleJobConfiguration(opts), Detail: action})
	op := model.MediaOperation{ID: hex.EncodeToString(hash[:]), CollectionID: collectionID, ItemKey: video.Path, SourcePath: video.Path, DestinationPath: action.Destination, Action: name, Status: "complete", ProviderID: action.ProviderID, Confidence: action.Confidence, Evidence: string(encoded), LeaseOwner: worker, CreatedAt: now, UpdatedAt: now, NextAttempt: now, LeaseUntil: now}
	if old != nil {
		op.CreatedAt, op.Attempts = old.CreatedAt, old.Attempts
	}
	switch action.State {
	case "needs_review":
		op.Status, op.Error = "needs_review", subtitleError(action.Reason)
	case "failed_terminal":
		op.Status, op.Error = "failed_terminal", subtitleError(action.Reason)
	case "failed":
		if op.Attempts >= 8 {
			op.Status = "failed_terminal"
		} else {
			op.Status = "failed_retryable"
			op.NextAttempt = subtitleRetryAtWithJitter(now, op.Attempts)
		}
		op.Error = subtitleError(action.Reason)
	case "quota_exhausted":
		op.Status = "failed_retryable"
		op.NextAttempt = now.Add(24 * time.Hour)
		op.Error = subtitleError(action.Reason)
	case "planned":
		op.Status = "planned"
	default:
		op.Status = "complete"
	}
	if err := journal.UpsertMediaOperation(ctx, &op); err != nil {
		return err
	}
	leaseKeeper.Done(op.ID)
	return nil
}

func subtitleRetryAtWithJitter(now time.Time, attempts int) time.Time {
	delay := time.Duration(math.Min(math.Pow(2, float64(attempts)), 360)) * time.Minute
	jitter := time.Duration(rand.Int64N(int64(delay/4) + 1))
	return now.Add(delay + jitter)
}

var subtitleURL = regexp.MustCompile(`https?://[^\s"']+`)

func subtitleError(value string) string {
	value = subtitleURL.ReplaceAllString(value, "[redacted URL]")
	if len(value) > 2048 {
		value = value[:2048]
	}
	return value
}

type queryContext struct {
	SearchQuery
	Hash         string
	Title        string
	Year         int
	IsEpisode    bool
	MultiEpisode bool
	Season       int
	Episode      int
	TMDbID       string
	IMDbID       string
}

func makeSearchQuery(coll *collection.Collection, video collection.InventoryEntry, language string) queryContext {
	kind := "movie"
	if coll.Type == collection.CollectionTypeShows {
		kind = "episode"
	}
	q := queryContext{SearchQuery: SearchQuery{Title: video.Title, Year: video.Year, Language: language, MediaType: kind, Season: video.Season, HasSeason: len(video.Episodes) > 0}, Title: video.Title, Year: video.Year, IsEpisode: kind == "episode", Season: video.Season, TMDbID: video.ProviderIDs["tmdb"], IMDbID: video.ProviderIDs["imdb"]}
	if len(video.Episodes) == 1 {
		q.Episode = video.Episodes[0]
		q.SearchQuery.Episode = q.Episode
		q.SearchQuery.HasEpisode = true
	}
	if len(video.Episodes) > 1 {
		q.Episode = -1
		q.MultiEpisode = true
	}
	if q.IsEpisode { // OpenSubtitles expects parent IDs; inventory provider IDs may include TVDb too.
		q.SearchQuery.TMDbID = q.TMDbID
	}
	return q
}

func searchCandidates(ctx context.Context, client *OpenSubtitles, coll *collection.Collection, video collection.InventoryEntry, q queryContext, opts RefreshOptions) ([]Candidate, error) {
	if q.IsEpisode && !q.HasEpisode && !q.MultiEpisode {
		return nil, fmt.Errorf("episode numbering or a supported date mapping is required for automatic subtitle selection")
	}
	var all []Candidate
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if q.MultiEpisode {
		hash, err := coll.Storage.OpenSubtitlesHash(video.Path)
		if err != nil {
			if strings.Contains(err.Error(), "file is too small for OpenSubtitles hash") {
				return nil, nil
			}
			return nil, err
		}
		hashQuery := SearchQuery{Language: q.Language, MediaType: "episode", MovieHash: hash}
		rows, searchErr := client.Search(ctx, hashQuery)
		if searchErr != nil {
			return nil, searchErr
		}
		for _, row := range rows {
			if row.MovieHashMatch {
				all = append(all, row)
			}
		}
		return all, nil
	}
	if hash, err := coll.Storage.OpenSubtitlesHash(video.Path); err == nil && hash != "" {
		hashQuery := SearchQuery{Language: q.Language, MediaType: q.SearchQuery.MediaType, MovieHash: hash}
		rows, searchErr := client.Search(ctx, hashQuery)
		if searchErr != nil {
			return nil, searchErr
		}
		all = append(all, rows...)
		if _, _, _, ok := rankSubtitle(q, rows, opts); ok {
			return rows, nil
		}
	}
	if q.TMDbID != "" || q.IMDbID != "" {
		idQuery := SearchQuery{Language: q.Language, MediaType: q.SearchQuery.MediaType, Season: q.Season, Episode: q.Episode, HasSeason: q.SearchQuery.HasSeason, HasEpisode: q.SearchQuery.HasEpisode}
		idQuery.TMDbID = q.TMDbID
		idQuery.IMDbID = q.IMDbID
		rows, err := client.Search(ctx, idQuery)
		if err != nil {
			return nil, err
		}
		all = append(all, rows...)
		if _, _, _, ok := rankSubtitle(q, rows, opts); ok {
			return rows, nil
		}
	}
	rows, err := client.Search(ctx, q.SearchQuery)
	if err != nil {
		return nil, err
	}
	all = append(all, rows...)
	return dedupeCandidates(all), nil
}

func dedupeCandidates(candidates []Candidate) []Candidate {
	seen := make(map[string]struct{}, len(candidates))
	result := make([]Candidate, 0, len(candidates))
	for _, candidate := range candidates {
		key := fmt.Sprintf("%s:%d", candidate.ID, candidate.FileID)
		if key == ":0" {
			result = append(result, candidate)
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, candidate)
	}
	return result
}

func rankSubtitle(q queryContext, candidates []Candidate, opts RefreshOptions) (Candidate, float64, string, bool) {
	if len(candidates) == 0 {
		return Candidate{}, 0, "provider search returned no subtitle candidates", false
	}
	minimum := opts.MinimumConfidence
	type scored struct {
		c     Candidate
		score float64
	}
	var scores []scored
	for _, c := range candidates {
		if !subtitleLanguageMatches(c.Language, q.Language) || c.HearingImpaired != opts.HearingImpaired || c.Forced != opts.Forced {
			continue
		}
		if !allowedFormat(candidateExtension(c), opts.Formats) {
			continue
		}
		if q.MultiEpisode && !c.MovieHashMatch {
			continue
		}
		score := .25 // requested language and flags match
		if q.TMDbID != "" && c.TMDbID == q.TMDbID {
			score += .35
		}
		if q.IMDbID != "" && c.IMDbID == q.IMDbID {
			score += .35
		}
		if c.MovieHashMatch {
			if q.MultiEpisode {
				score += .65
			} else {
				score += .55
			}
		}
		if q.Year > 0 && c.Year > 0 {
			if q.Year == c.Year {
				score += .15
			} else {
				score -= .2
			}
		}
		if q.IsEpisode && q.HasSeason && q.HasEpisode {
			if c.Season == q.Season && c.Episode == q.Episode {
				score += .45
			} else {
				score -= .3
			}
		}
		if q.Title != "" && c.Title != "" {
			score += .2 * titleSimilarity(q.Title, c.Title)
		}
		if score > 1 {
			score = 1
		}
		if score < 0 {
			score = 0
		}
		scores = append(scores, scored{c, score})
	}
	sort.Slice(scores, func(i, j int) bool {
		if scores[i].score == scores[j].score {
			return scores[i].c.ID < scores[j].c.ID
		}
		return scores[i].score > scores[j].score
	})
	if len(scores) == 0 {
		return Candidate{}, 0, "no candidate matches requested language, flags, and configured subtitle formats", false
	}
	if scores[0].score < minimum {
		return scores[0].c, scores[0].score, "best subtitle match is below confidence threshold", false
	}
	if len(scores) > 1 && scores[0].score-scores[1].score < .07 {
		return scores[0].c, scores[0].score, "top subtitle candidates are too close", false
	}
	return scores[0].c, scores[0].score, "", true
}

func titleSimilarity(a, b string) float64 {
	a = strings.ToLower(strings.Join(strings.Fields(a), " "))
	b = strings.ToLower(strings.Join(strings.Fields(b), " "))
	if a == b && a != "" {
		return 1
	}
	if a == "" || b == "" {
		return 0
	}
	if strings.Contains(a, b) || strings.Contains(b, a) {
		return .75
	}
	return 0
}
func hasMatchingSubtitle(entries []collection.InventoryEntry, video collection.InventoryEntry, language string, forced, hi bool) bool {
	base := strings.TrimSuffix(path.Base(video.Path), path.Ext(video.Path))
	for _, entry := range entries {
		if entry.Role != collection.InventorySubtitle || path.Dir(entry.Path) != path.Dir(video.Path) {
			continue
		}
		stem := strings.TrimSuffix(path.Base(entry.Path), path.Ext(entry.Path))
		stemRunes, baseRunes := []rune(stem), []rune(base)
		if len(stemRunes) <= len(baseRunes) || stemRunes[len(baseRunes)] != '.' || !strings.EqualFold(string(stemRunes[:len(baseRunes)]), base) {
			continue
		}
		parts := strings.Split(string(stemRunes[len(baseRunes)+1:]), ".")
		if len(parts) == 0 || !subtitleLanguageMatches(parts[0], language) {
			continue
		}
		foundForced, foundHI := false, false
		for _, flag := range parts[1:] {
			switch strings.ToLower(flag) {
			case "forced", "foreign":
				foundForced = true
			case "sdh", "hi", "cc":
				foundHI = true
			}
		}
		if foundForced == forced && foundHI == hi {
			return true
		}
	}
	return false
}

func subtitleLanguageMatches(candidate, requested string) bool {
	candidate = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(candidate), "_", "-"))
	requested = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(requested), "_", "-"))
	if candidate == requested {
		return candidate != ""
	}
	// A base-language policy such as "en" accepts a regional sidecar such as
	// "en-US". A region-specific request remains exact to avoid substituting a
	// different locale silently.
	return requested != "" && !strings.Contains(requested, "-") && strings.HasPrefix(candidate, requested+"-")
}

func candidateExtension(c Candidate) string {
	ext := strings.ToLower(strings.TrimPrefix(path.Ext(c.FileName), "."))
	if ext == "" {
		ext = strings.TrimPrefix(strings.ToLower(c.Format), ".")
	}
	return ext
}

func subtitleCandidateReport(candidate Candidate) *SubtitleCandidateReport {
	if candidate.ID == "" && candidate.Title == "" && candidate.FileID == 0 {
		return nil
	}
	return &SubtitleCandidateReport{
		Title:           candidate.Title,
		Year:            candidate.Year,
		Language:        candidate.Language,
		Format:          strings.ToLower(strings.TrimPrefix(candidate.Format, ".")),
		HearingImpaired: candidate.HearingImpaired,
		Forced:          candidate.Forced,
		MovieHashMatch:  candidate.MovieHashMatch,
		Season:          candidate.Season,
		Episode:         candidate.Episode,
		TMDbID:          candidate.TMDbID,
		IMDbID:          candidate.IMDbID,
		DownloadCount:   candidate.DownloadCount,
	}
}

func allowedFormat(ext string, configured []string) bool {
	if ext != "srt" && ext != "vtt" {
		return false
	}
	if len(configured) == 0 {
		return ext == "srt"
	}
	for _, v := range configured {
		if strings.EqualFold(strings.TrimPrefix(v, "."), ext) {
			return true
		}
	}
	return false
}
func subtitleDestination(video, lang string, forced, hi bool, ext string) string {
	base := strings.TrimSuffix(video, path.Ext(video)) + "." + lang
	if forced {
		base += ".forced"
	}
	if hi {
		base += ".sdh"
	}
	return base + "." + ext
}

var srtTime = regexp.MustCompile(`(?m)\d{2}:\d{2}:\d{2}[,.]\d{3}\s+-->\s+\d{2}:\d{2}:\d{2}[,.]\d{3}`)
var vttTime = regexp.MustCompile(`(?m)(?:\d{2}:)?\d{2}:\d{2}\.\d{3}\s+-->\s+(?:\d{2}:)?\d{2}:\d{2}\.\d{3}`)

func validateSubtitle(data []byte, ext string) ([]byte, error) {
	if len(data) == 0 || len(data) > 20<<20 {
		return nil, fmt.Errorf("subtitle payload size is invalid")
	}
	text, err := decodeSubtitleText(data)
	if err != nil {
		return nil, err
	}
	for _, char := range text {
		if (char < 0x20 && char != '\n' && char != '\r' && char != '\t') || (char >= 0x7f && char <= 0x9f) {
			return nil, fmt.Errorf("subtitle contains unsupported control characters")
		}
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	trimmed := strings.TrimSpace(text)
	if ext == "vtt" {
		if !strings.HasPrefix(trimmed, "WEBVTT") || !vttTime.MatchString(trimmed) {
			return nil, fmt.Errorf("download is not a valid WebVTT subtitle")
		}
	} else if ext == "srt" {
		if !srtTime.MatchString(trimmed) {
			return nil, fmt.Errorf("download is not a valid SRT subtitle")
		}
	} else {
		return nil, fmt.Errorf("unsupported subtitle format %q", ext)
	}
	return []byte(text), nil
}
func decodeSubtitleText(data []byte) (string, error) {
	if bytes.HasPrefix(data, []byte{0xef, 0xbb, 0xbf}) {
		data = data[3:]
		if !utf8.Valid(data) {
			return "", fmt.Errorf("UTF-8 subtitle contains invalid byte sequences")
		}
		return string(data), nil
	}
	if bytes.HasPrefix(data, []byte{0xff, 0xfe}) || bytes.HasPrefix(data, []byte{0xfe, 0xff}) {
		if (len(data)-2)%2 != 0 {
			return "", fmt.Errorf("UTF-16 subtitle has an incomplete code unit")
		}
		little := data[0] == 0xff
		body := data[2 : len(data)&^1]
		units := make([]uint16, len(body)/2)
		for i := range units {
			if little {
				units[i] = binary.LittleEndian.Uint16(body[i*2:])
			} else {
				units[i] = binary.BigEndian.Uint16(body[i*2:])
			}
		}
		return string(utf16.Decode(units)), nil
	}
	if !utf8.Valid(data) {
		// OpenSubtitles commonly supplies legacy Windows-1252 text. Decode it
		// only after strict UTF-8 and BOM-marked UTF-16 checks fail; the caller
		// still validates SRT/VTT structure before any write.
		maxDecodedBytes := int64(len(data))*3 + 1
		reader := charmap.Windows1252.NewDecoder().Reader(bytes.NewReader(data))
		decoded, err := io.ReadAll(io.LimitReader(reader, maxDecodedBytes))
		if err != nil {
			return "", fmt.Errorf("decode Windows-1252 subtitle: %w", err)
		}
		if int64(len(decoded)) >= maxDecodedBytes {
			return "", fmt.Errorf("decoded subtitle exceeds the bounded text size")
		}
		return string(decoded), nil
	}
	return string(data), nil
}
