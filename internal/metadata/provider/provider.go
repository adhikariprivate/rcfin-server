// Package provider contains provider-neutral metadata types and clients.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"math/rand/v2"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"
)

func decodeLimited(r io.Reader, limit int64, target any) error {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return err
	}
	if int64(len(b)) > limit {
		return fmt.Errorf("TMDb response exceeds %d bytes", limit)
	}
	if err := json.Unmarshal(b, target); err != nil {
		return fmt.Errorf("decode TMDb response: %w", err)
	}
	return nil
}

func stringValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if n, ok := v.(float64); ok && n != 0 {
		return fmt.Sprintf("%.0f", n)
	}
	return ""
}
func numberValue(v any) float64 { n, _ := v.(float64); return n }
func fieldValue(v map[string]any, keys ...string) string {
	for _, key := range keys {
		if s := stringValue(v[key]); s != "" {
			return s
		}
	}
	return ""
}
func arrayValue(v any) []map[string]any {
	raw, _ := v.([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}
func dateYear(s string) int {
	if len(s) >= 4 {
		year := 0
		for _, c := range s[:4] {
			if c < '0' || c > '9' {
				return 0
			}
			year = year*10 + int(c-'0')
		}
		return year
	}
	return 0
}
func firstInt(v any) int {
	values, _ := v.([]any)
	for _, x := range values {
		if n := numberValue(x); n > 0 {
			return int(n)
		}
	}
	return 0
}

type Match struct {
	Candidate   Candidate `json:"candidate"`
	Confidence  float64   `json:"confidence"`
	Evidence    []string  `json:"evidence"`
	NeedsReview bool      `json:"needs_review"`
}

// RankCandidates scores all candidates and marks weak or close results for
// review. It never resolves ties by provider result order.
func RankCandidates(title string, year int, candidates []Candidate, minimum, tieMargin float64) []Match {
	if minimum <= 0 {
		minimum = 0.82
	}
	if tieMargin <= 0 {
		tieMargin = 0.08
	}
	matches := make([]Match, 0, len(candidates))
	for _, candidate := range candidates {
		sim := titleSimilarity(title, candidate.Title)
		if candidate.OriginalTitle != "" && titleSimilarity(title, candidate.OriginalTitle) > sim {
			sim = titleSimilarity(title, candidate.OriginalTitle)
		}
		score := sim * 0.8
		evidence := []string{fmt.Sprintf("title similarity %.2f", sim)}
		if year > 0 && candidate.Year > 0 {
			if year == candidate.Year {
				score += 0.2
				evidence = append(evidence, "release year matches")
			} else {
				score -= 0.15
				evidence = append(evidence, "release year differs")
			}
		}
		if score < 0 {
			score = 0
		}
		if score > 1 {
			score = 1
		}
		matches = append(matches, Match{Candidate: candidate, Confidence: score, Evidence: evidence})
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].Confidence == matches[j].Confidence {
			return matches[i].Candidate.ID < matches[j].Candidate.ID
		}
		return matches[i].Confidence > matches[j].Confidence
	})
	for i := range matches {
		matches[i].NeedsReview = matches[i].Confidence < minimum
		if i == 0 && len(matches) > 1 && matches[0].Confidence-matches[1].Confidence < tieMargin {
			matches[i].NeedsReview = true
			matches[i].Evidence = append(matches[i].Evidence, "top candidates are too close")
		}
	}
	return matches
}

func titleSimilarity(a, b string) float64 {
	normalize := func(s string) string {
		var out strings.Builder
		space := false
		for _, r := range strings.ToLower(s) {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
				out.WriteRune(r)
				space = false
			} else if !space {
				out.WriteByte(' ')
				space = true
			}
		}
		return strings.TrimSpace(out.String())
	}
	a, b = normalize(a), normalize(b)
	if a == "" || b == "" {
		return 0
	}
	if a == b {
		return 1
	}
	// Token Dice similarity is stable for punctuation and word-order changes.
	left, right := strings.Fields(a), strings.Fields(b)
	counts := map[string]int{}
	for _, v := range left {
		counts[v]++
	}
	common := 0
	for _, v := range right {
		if counts[v] > 0 {
			common++
			counts[v]--
		}
	}
	return 2 * float64(common) / float64(len(left)+len(right))
}

type MediaType string

const (
	Movie   MediaType = "movie"
	Show    MediaType = "show"
	Season  MediaType = "season"
	Episode MediaType = "episode"
)

type Candidate struct {
	Provider      string    `json:"provider"`
	Type          MediaType `json:"type"`
	ID            string    `json:"id"`
	Title         string    `json:"title"`
	OriginalTitle string    `json:"original_title,omitempty"`
	Year          int       `json:"year,omitempty"`
	Overview      string    `json:"overview,omitempty"`
	PosterPath    string    `json:"poster_path,omitempty"`
	BackdropPath  string    `json:"backdrop_path,omitempty"`
	Popularity    float64   `json:"popularity,omitempty"`
}

type Metadata struct {
	Provider       string            `json:"provider"`
	Type           MediaType         `json:"type"`
	ID             string            `json:"id"`
	Title          string            `json:"title"`
	OriginalTitle  string            `json:"original_title,omitempty"`
	SortTitle      string            `json:"sort_title,omitempty"`
	Year           int               `json:"year,omitempty"`
	ReleaseDate    time.Time         `json:"release_date,omitempty"`
	Overview       string            `json:"overview,omitempty"`
	Genres         []string          `json:"genres,omitempty"`
	Studios        []string          `json:"studios,omitempty"`
	RuntimeMinutes int               `json:"runtime_minutes,omitempty"`
	Rating         float32           `json:"rating,omitempty"`
	ContentRating  string            `json:"content_rating,omitempty"`
	Directors      []string          `json:"directors,omitempty"`
	Writers        []string          `json:"writers,omitempty"`
	Actors         map[string]string `json:"actors,omitempty"`
	PosterPath     string            `json:"poster_path,omitempty"`
	BackdropPath   string            `json:"backdrop_path,omitempty"`
	ExternalIDs    map[string]string `json:"external_ids,omitempty"`
	SeasonNumber   int               `json:"season_number,omitempty"`
	EpisodeNumber  int               `json:"episode_number,omitempty"`
}

// Provider is intentionally provider-neutral so future sources can be added
// without leaking their response types into collection code.
type Provider interface {
	Search(context.Context, MediaType, string, int) ([]Candidate, error)
	Get(context.Context, MediaType, string, string) (Metadata, error)
	FindExternalID(context.Context, string, string, MediaType, string) (Metadata, error)
}

type ArtworkProvider interface {
	DownloadArtwork(context.Context, string, string, int64) ([]byte, string, error)
}

// EpisodeMetadata is provider-confirmed identity for one numbered TV episode.
type EpisodeMetadata struct {
	ID             string            `json:"id,omitempty"`
	SeasonNumber   int               `json:"season_number"`
	EpisodeNumber  int               `json:"episode_number"`
	Name           string            `json:"name"`
	AirDate        string            `json:"air_date,omitempty"`
	Overview       string            `json:"overview,omitempty"`
	RuntimeMinutes int               `json:"runtime_minutes,omitempty"`
	Rating         float32           `json:"rating,omitempty"`
	Directors      []string          `json:"directors,omitempty"`
	Writers        []string          `json:"writers,omitempty"`
	Actors         map[string]string `json:"actors,omitempty"`
}

type SeasonMetadata struct {
	ID           string   `json:"id,omitempty"`
	SeasonNumber int      `json:"season_number"`
	Name         string   `json:"name"`
	AirDate      string   `json:"air_date,omitempty"`
	Overview     string   `json:"overview,omitempty"`
	PosterPath   string   `json:"poster_path,omitempty"`
	Rating       float32  `json:"rating,omitempty"`
	Networks     []string `json:"networks,omitempty"`
}

type SeasonDetails struct {
	Metadata SeasonMetadata    `json:"metadata"`
	Episodes []EpisodeMetadata `json:"episodes"`
}

// SeasonEpisodeProvider supplies exact episode names for normalized TV paths.
type SeasonEpisodeProvider interface {
	GetSeasonEpisodes(context.Context, string, int, string) ([]EpisodeMetadata, error)
}

type SeasonProvider interface {
	GetSeason(context.Context, string, int, string) (SeasonDetails, error)
}

type TMDb struct {
	token      string
	client     *http.Client
	baseURL    string
	limiter    *rate.Limiter
	requests   chan struct{}
	retryLimit int
}

// TMDb clients are created per library, but provider rate limits apply to the
// whole RCFin process. Share pacing and in-flight request bounds across them.
var tmdbProcessLimiter = rate.NewLimiter(rate.Every(300*time.Millisecond), 1)
var tmdbProcessRequests = make(chan struct{}, 4)

type releaseBody struct {
	io.ReadCloser
	release func()
}

func (b *releaseBody) Close() error { err := b.ReadCloser.Close(); b.release(); return err }

func NewTMDb(token string, client *http.Client) (*TMDb, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, fmt.Errorf("TMDb bearer token is required")
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	} else if client.Timeout <= 0 {
		copyClient := *client
		copyClient.Timeout = 10 * time.Second
		client = &copyClient
	}
	return &TMDb{token: token, client: client, baseURL: "https://api.themoviedb.org/3", limiter: tmdbProcessLimiter, requests: tmdbProcessRequests, retryLimit: 3}, nil
}

func (t *TMDb) SetRetryLimit(limit int) {
	if limit >= 1 && limit <= 10 {
		t.retryLimit = limit
	}
}

func (t *TMDb) do(ctx context.Context, request *http.Request) (*http.Response, error) {
	var last error
	limit := t.retryLimit
	if limit <= 0 {
		limit = 3
	}
	for attempt := 0; attempt < limit; attempt++ {
		select {
		case t.requests <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if err := t.limiter.Wait(ctx); err != nil {
			<-t.requests
			return nil, err
		}
		response, err := t.client.Do(request.Clone(ctx))
		if err != nil {
			<-t.requests
			last = err
		} else if response.StatusCode >= 200 && response.StatusCode < 300 {
			response.Body = &releaseBody{ReadCloser: response.Body, release: func() { <-t.requests }}
			return response, nil
		} else {
			status := response.StatusCode
			delay := retryDelay(response.Header.Get("Retry-After"), attempt)
			response.Body = &releaseBody{ReadCloser: response.Body, release: func() { <-t.requests }}
			response.Body.Close()
			if status != 429 && status < 500 {
				return nil, fmt.Errorf("TMDb request returned %s", response.Status)
			}
			last = fmt.Errorf("TMDb request returned %s", response.Status)
			if attempt == limit-1 {
				break
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
			continue
		}
		if attempt < limit-1 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(retryDelay("", attempt)):
			}
		}
	}
	return nil, last
}

func retryDelay(header string, attempt int) time.Duration {
	var delay time.Duration
	if seconds, err := strconv.Atoi(strings.TrimSpace(header)); err == nil && seconds > 0 {
		delay = time.Duration(seconds) * time.Second
		if delay > 5*time.Second {
			delay = 5 * time.Second
		}
	} else {
		delay = 200 * time.Millisecond * time.Duration(1<<attempt)
		if delay > 2*time.Second {
			delay = 2 * time.Second
		}
	}
	return delay + time.Duration(rand.Int64N(int64(delay/4)+1))
}

func (t *TMDb) endpoint(ctx context.Context, resource string, query url.Values, out any) error {
	u := t.baseURL + resource
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+t.token)
	req.Header.Set("Accept", "application/json")
	resp, err := t.do(ctx, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	const maxResponse = 8 << 20
	return decodeLimited(resp.Body, maxResponse, out)
}

// DownloadArtwork fetches a TMDb image path at a bounded size and validates
// its image signature and dimensions before returning the bytes.
func (t *TMDb) DownloadArtwork(ctx context.Context, imagePath, size string, maxBytes int64) ([]byte, string, error) {
	imagePath = strings.TrimSpace(imagePath)
	if imagePath == "" || !strings.HasPrefix(imagePath, "/") || strings.Contains(imagePath, "..") {
		return nil, "", fmt.Errorf("invalid TMDb image path")
	}
	if size != "w500" && size != "w780" && size != "original" {
		return nil, "", fmt.Errorf("unsupported TMDb image size %q", size)
	}
	if maxBytes <= 0 || maxBytes > 32<<20 {
		maxBytes = 12 << 20
	}
	u := "https://image.tmdb.org/t/p/" + size + imagePath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := t.do(ctx, req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	contentType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return nil, "", fmt.Errorf("TMDb artwork response has an invalid Content-Type")
	}
	contentType = strings.ToLower(contentType)
	if contentType == "image/jpg" {
		contentType = "image/jpeg"
	}
	if contentType != "image/jpeg" && contentType != "image/png" {
		return nil, "", fmt.Errorf("TMDb artwork response has unsupported Content-Type %q", contentType)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(data)) > maxBytes {
		return nil, "", fmt.Errorf("TMDb artwork exceeds %d bytes", maxBytes)
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("invalid TMDb image: %w", err)
	}
	if config.Width <= 0 || config.Height <= 0 || config.Width > 10000 || config.Height > 10000 || int64(config.Width)*int64(config.Height) > 50_000_000 {
		return nil, "", fmt.Errorf("TMDb artwork dimensions exceed limits")
	}
	mime := "image/jpeg"
	if format == "png" {
		mime = "image/png"
	} else if format != "jpeg" {
		return nil, "", fmt.Errorf("unsupported TMDb image format %q", format)
	}
	if contentType != mime {
		return nil, "", fmt.Errorf("TMDb artwork Content-Type does not match its image signature")
	}
	return data, mime, nil
}

func (t *TMDb) Search(ctx context.Context, kind MediaType, title string, year int) ([]Candidate, error) {
	resource, field := "/search/movie", "title"
	if kind == Show {
		resource, field = "/search/tv", "name"
	} else if kind != Movie {
		return nil, fmt.Errorf("unsupported media type %q", kind)
	}
	q := url.Values{"query": {title}, "include_adult": {"false"}}
	if year > 0 {
		if kind == Movie {
			q.Set("year", fmt.Sprint(year))
		} else {
			q.Set("first_air_date_year", fmt.Sprint(year))
		}
	}
	var response struct {
		Results []map[string]any `json:"results"`
	}
	if err := t.endpoint(ctx, resource, q, &response); err != nil {
		return nil, err
	}
	out := make([]Candidate, 0, len(response.Results))
	for _, raw := range response.Results {
		id := stringValue(raw["id"])
		name := stringValue(raw[field])
		if id == "" || name == "" {
			continue
		}
		date := stringValue(raw["release_date"])
		if kind == Show {
			date = stringValue(raw["first_air_date"])
		}
		out = append(out, Candidate{Provider: "tmdb", Type: kind, ID: id, Title: name, OriginalTitle: fieldValue(raw, "original_title", "original_name"), Year: dateYear(date), Overview: stringValue(raw["overview"]), PosterPath: stringValue(raw["poster_path"]), BackdropPath: stringValue(raw["backdrop_path"]), Popularity: numberValue(raw["popularity"])})
	}
	return out, nil
}

func (t *TMDb) Get(ctx context.Context, kind MediaType, id, language string) (Metadata, error) {
	resource, field, dateField := "/movie/", "title", "release_date"
	if kind == Show {
		resource, field, dateField = "/tv/", "name", "first_air_date"
	} else if kind != Movie {
		return Metadata{}, fmt.Errorf("unsupported media type %q", kind)
	}
	q := url.Values{}
	if language != "" {
		q.Set("language", language)
	}
	appendToResponse := "external_ids,credits,release_dates"
	if kind == Show {
		appendToResponse = "external_ids,credits,content_ratings"
	}
	q.Set("append_to_response", appendToResponse)
	var raw map[string]any
	if err := t.endpoint(ctx, resource+url.PathEscape(id), q, &raw); err != nil {
		return Metadata{}, err
	}
	date := stringValue(raw[dateField])
	parsed, _ := time.Parse("2006-01-02", date)
	title := stringValue(raw[field])
	meta := Metadata{Provider: "tmdb", Type: kind, ID: stringValue(raw["id"]), Title: title, OriginalTitle: fieldValue(raw, "original_title", "original_name"), SortTitle: title, Year: dateYear(date), ReleaseDate: parsed, Overview: stringValue(raw["overview"]), Rating: float32(numberValue(raw["vote_average"])), ContentRating: tmdbContentRating(raw, kind, language), PosterPath: stringValue(raw["poster_path"]), BackdropPath: stringValue(raw["backdrop_path"]), ExternalIDs: map[string]string{"tmdb": stringValue(raw["id"])}}
	if runtime, ok := raw["runtime"]; ok {
		meta.RuntimeMinutes = int(numberValue(runtime))
	}
	if kind == Show {
		meta.RuntimeMinutes = firstInt(raw["episode_run_time"])
	}
	for _, g := range arrayValue(raw["genres"]) {
		if n := stringValue(g["name"]); n != "" {
			meta.Genres = append(meta.Genres, n)
		}
	}
	for _, c := range arrayValue(raw["production_companies"]) {
		if n := stringValue(c["name"]); n != "" {
			meta.Studios = append(meta.Studios, n)
		}
	}
	if kind == Show {
		for _, network := range arrayValue(raw["networks"]) {
			if n := stringValue(network["name"]); n != "" {
				meta.Studios = append(meta.Studios, n)
			}
		}
	}
	if credits, ok := raw["credits"].(map[string]any); ok {
		meta.Actors = map[string]string{}
		for i, cast := range arrayValue(credits["cast"]) {
			if i >= 20 {
				break
			}
			name := stringValue(cast["name"])
			if name != "" {
				meta.Actors[name] = stringValue(cast["character"])
			}
		}
		for _, crew := range arrayValue(credits["crew"]) {
			name := stringValue(crew["name"])
			job := strings.ToLower(stringValue(crew["job"]))
			if name == "" {
				continue
			}
			if job == "director" {
				meta.Directors = appendUniqueString(meta.Directors, name)
			}
			if job == "writer" || job == "screenplay" || job == "story" {
				meta.Writers = appendUniqueString(meta.Writers, name)
			}
		}
	}
	if kind == Show {
		for _, creator := range arrayValue(raw["created_by"]) {
			if name := stringValue(creator["name"]); name != "" {
				meta.Writers = appendUniqueString(meta.Writers, name)
			}
		}
	}
	if ext, ok := raw["external_ids"].(map[string]any); ok {
		for _, key := range []string{"imdb_id", "tvdb_id"} {
			if value := stringValue(ext[key]); value != "" {
				meta.ExternalIDs[strings.TrimSuffix(key, "_id")] = value
			}
		}
	}
	return meta, nil
}

func tmdbContentRating(raw map[string]any, kind MediaType, language string) string {
	country := languageCountry(language)
	if country == "" {
		return ""
	}
	if kind == Show {
		ratings, _ := raw["content_ratings"].(map[string]any)
		for _, result := range arrayValue(ratings["results"]) {
			if strings.EqualFold(stringValue(result["iso_3166_1"]), country) {
				return strings.TrimSpace(stringValue(result["rating"]))
			}
		}
		return ""
	}
	dates, _ := raw["release_dates"].(map[string]any)
	for _, result := range arrayValue(dates["results"]) {
		if !strings.EqualFold(stringValue(result["iso_3166_1"]), country) {
			continue
		}
		var fallback string
		for _, release := range arrayValue(result["release_dates"]) {
			certification := strings.TrimSpace(stringValue(release["certification"]))
			if certification == "" {
				continue
			}
			if numberValue(release["type"]) == 3 {
				return certification
			}
			if fallback == "" {
				fallback = certification
			}
		}
		return fallback
	}
	return ""
}

func languageCountry(language string) string {
	parts := strings.Split(strings.ReplaceAll(strings.TrimSpace(language), "_", "-"), "-")
	for _, part := range parts[1:] {
		if len(part) != 2 {
			continue
		}
		if part[0] >= 'A' && part[0] <= 'Z' || part[0] >= 'a' && part[0] <= 'z' {
			if part[1] >= 'A' && part[1] <= 'Z' || part[1] >= 'a' && part[1] <= 'z' {
				return strings.ToUpper(part)
			}
		}
	}
	return ""
}

// GetSeasonEpisodes uses TMDb's TV season details endpoint. It returns only
// numbered episodes with names, which lets organization reject uncertain
// episode title guesses instead of fabricating normalized filenames.
func (t *TMDb) GetSeasonEpisodes(ctx context.Context, seriesID string, season int, language string) ([]EpisodeMetadata, error) {
	details, err := t.GetSeason(ctx, seriesID, season, language)
	return details.Episodes, err
}

func (t *TMDb) GetSeason(ctx context.Context, seriesID string, season int, language string) (SeasonDetails, error) {
	if strings.TrimSpace(seriesID) == "" || season < 0 {
		return SeasonDetails{}, fmt.Errorf("series ID and non-negative season number are required")
	}
	query := url.Values{}
	if language != "" {
		query.Set("language", language)
	}
	var response struct {
		ID           int64   `json:"id"`
		SeasonNumber int     `json:"season_number"`
		Name         string  `json:"name"`
		AirDate      string  `json:"air_date"`
		Overview     string  `json:"overview"`
		PosterPath   string  `json:"poster_path"`
		VoteAverage  float32 `json:"vote_average"`
		Networks     []struct {
			Name string `json:"name"`
		} `json:"networks"`
		Episodes []struct {
			SeasonNumber  int     `json:"season_number"`
			EpisodeNumber int     `json:"episode_number"`
			Name          string  `json:"name"`
			AirDate       string  `json:"air_date"`
			Overview      string  `json:"overview"`
			ID            int64   `json:"id"`
			Runtime       int     `json:"runtime"`
			VoteAverage   float32 `json:"vote_average"`
			Crew          []struct {
				Name string `json:"name"`
				Job  string `json:"job"`
			} `json:"crew"`
			GuestStars []struct {
				Name      string `json:"name"`
				Character string `json:"character"`
			} `json:"guest_stars"`
		} `json:"episodes"`
	}
	endpoint := "/tv/" + url.PathEscape(seriesID) + "/season/" + strconv.Itoa(season)
	if err := t.endpoint(ctx, endpoint, query, &response); err != nil {
		return SeasonDetails{}, err
	}
	seasonMetadata := SeasonMetadata{ID: fmt.Sprint(response.ID), SeasonNumber: response.SeasonNumber, Name: response.Name, AirDate: response.AirDate, Overview: response.Overview, PosterPath: response.PosterPath, Rating: response.VoteAverage}
	for _, network := range response.Networks {
		if name := strings.TrimSpace(network.Name); name != "" {
			seasonMetadata.Networks = appendUniqueString(seasonMetadata.Networks, name)
		}
	}
	out := SeasonDetails{Metadata: seasonMetadata, Episodes: make([]EpisodeMetadata, 0, len(response.Episodes))}
	for _, episode := range response.Episodes {
		if episode.EpisodeNumber <= 0 || strings.TrimSpace(episode.Name) == "" {
			continue
		}
		row := EpisodeMetadata{ID: fmt.Sprint(episode.ID), SeasonNumber: episode.SeasonNumber, EpisodeNumber: episode.EpisodeNumber, Name: episode.Name, AirDate: episode.AirDate, Overview: episode.Overview, RuntimeMinutes: episode.Runtime, Rating: episode.VoteAverage, Actors: map[string]string{}}
		for _, cast := range episode.GuestStars {
			if name := strings.TrimSpace(cast.Name); name != "" {
				row.Actors[name] = strings.TrimSpace(cast.Character)
			}
		}
		for _, crew := range episode.Crew {
			name, job := strings.TrimSpace(crew.Name), strings.ToLower(strings.TrimSpace(crew.Job))
			if name == "" {
				continue
			}
			switch job {
			case "director":
				row.Directors = appendUniqueString(row.Directors, name)
			case "writer", "screenplay", "story":
				row.Writers = appendUniqueString(row.Writers, name)
			}
		}
		if len(row.Actors) == 0 {
			row.Actors = nil
		}
		out.Episodes = append(out.Episodes, row)
	}
	return out, nil
}

func appendUniqueString(values []string, value string) []string {
	for _, existing := range values {
		if strings.EqualFold(existing, value) {
			return values
		}
	}
	return append(values, value)
}

func (t *TMDb) FindExternalID(ctx context.Context, source, externalID string, kind MediaType, language string) (Metadata, error) {
	param := map[string]string{"imdb": "imdb_id", "tvdb": "tvdb_id"}[strings.ToLower(source)]
	if param == "" {
		return Metadata{}, fmt.Errorf("TMDb external lookup does not support %q", source)
	}
	q := url.Values{"external_source": {param}}
	if language != "" {
		q.Set("language", language)
	}
	var raw struct {
		Movies []map[string]any `json:"movie_results"`
		Shows  []map[string]any `json:"tv_results"`
	}
	if err := t.endpoint(ctx, "/find/"+url.PathEscape(externalID), q, &raw); err != nil {
		return Metadata{}, err
	}
	rows := raw.Movies
	selected := Movie
	if kind == Show {
		rows = raw.Shows
		selected = Show
	} else if kind != Movie {
		return Metadata{}, fmt.Errorf("unsupported media type %q", kind)
	}
	if len(rows) == 0 {
		return Metadata{}, fmt.Errorf("no TMDb %s match for %s ID", kind, source)
	}
	return t.Get(ctx, selected, stringValue(rows[0]["id"]), language)
}
