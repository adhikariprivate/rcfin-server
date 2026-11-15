// Package subtitles contains the OpenSubtitles.com client and subtitle
// acquisition workflow.
package subtitles

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

type OpenSubtitles struct {
	apiKey           string
	user             string
	password         string
	userAgent        string
	client           *http.Client
	token            string
	limiter          *rate.Limiter
	retryLimit       int
	maxDownloadBytes int64
	quotaExceeded    atomic.Bool
	authMu           sync.Mutex
	authError        error
}

var ErrDownloadQuotaExceeded = fmt.Errorf("OpenSubtitles daily download quota is exhausted")
var ErrAuthentication = errors.New("OpenSubtitles authentication failed")

// Share request pacing across per-library clients to honor one process-wide
// provider budget.
var openSubtitlesProcessLimiter = rate.NewLimiter(rate.Every(500*time.Millisecond), 1)

func NewOpenSubtitles(apiKey, user, password string, client *http.Client) (*OpenSubtitles, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("OpenSubtitles API key is required")
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	} else if client.Timeout <= 0 {
		clone := *client
		clone.Timeout = 15 * time.Second
		client = &clone
	}
	return &OpenSubtitles{apiKey: strings.TrimSpace(apiKey), user: user, password: password, userAgent: "RCFin/0.1.0", client: client, limiter: openSubtitlesProcessLimiter, retryLimit: 3, maxDownloadBytes: 20 << 20}, nil
}

func (c *OpenSubtitles) SetRetryLimit(limit int) {
	if limit >= 1 && limit <= 10 {
		c.retryLimit = limit
	}
}

func (c *OpenSubtitles) SetMaxDownloadBytes(limit int64) {
	if limit > 0 && limit <= 20<<20 {
		c.maxDownloadBytes = limit
	}
}

type SearchQuery struct {
	Title      string
	Year       int
	Language   string
	MediaType  string // movie or episode
	TMDbID     string
	IMDbID     string
	Season     int
	Episode    int
	HasSeason  bool
	HasEpisode bool
	MovieHash  string
}

type Candidate struct {
	ID              string
	FileID          int64
	FileName        string
	Release         string
	Language        string
	Format          string
	HearingImpaired bool
	Forced          bool
	MovieHashMatch  bool
	FromTrusted     bool
	Title           string
	Year            int
	Season          int
	Episode         int
	TMDbID          string
	IMDbID          string
	DownloadCount   int64
	Ratings         float64
}

// stringOrNumber handles OpenSubtitles IDs, which the API has returned as
// either JSON strings or numbers across responses.
type stringOrNumber string

func (s *stringOrNumber) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err == nil {
		*s = stringOrNumber(value)
		return nil
	}
	var number json.Number
	if err := json.Unmarshal(data, &number); err != nil {
		return err
	}
	*s = stringOrNumber(number.String())
	return nil
}

type searchResponse struct {
	Data []struct {
		ID         string `json:"id"`
		Attributes struct {
			Language         string  `json:"language"`
			HearingImpaired  bool    `json:"hearing_impaired"`
			ForeignPartsOnly bool    `json:"foreign_parts_only"`
			MovieHashMatch   bool    `json:"moviehash_match"`
			DownloadCount    int64   `json:"download_count"`
			Ratings          float64 `json:"ratings"`
			FromTrusted      bool    `json:"from_trusted"`
			Release          string  `json:"release"`
			Format           string  `json:"format"`
			Files            []struct {
				FileID   int64  `json:"file_id"`
				FileName string `json:"file_name"`
			} `json:"files"`
			Feature struct {
				Title   string         `json:"title"`
				Year    int            `json:"year"`
				Season  int            `json:"season_number"`
				Episode int            `json:"episode_number"`
				TMDbID  stringOrNumber `json:"tmdb_id"`
				IMDbID  stringOrNumber `json:"imdb_id"`
			} `json:"feature_details"`
		} `json:"attributes"`
	} `json:"data"`
}
type downloadResponse struct {
	Link      string `json:"link"`
	FileName  string `json:"file_name"`
	Remaining *int   `json:"remaining"`
}

func (c *OpenSubtitles) QuotaExceeded() bool { return c.quotaExceeded.Load() }

func (c *OpenSubtitles) Search(ctx context.Context, q SearchQuery) ([]Candidate, error) {
	if !validLanguage(q.Language) {
		return nil, fmt.Errorf("invalid subtitle language %q", q.Language)
	}
	query := url.Values{"languages": {q.Language}, "type": {q.MediaType}}
	if q.MovieHash != "" {
		query.Set("moviehash", q.MovieHash)
		query.Set("moviehash_match", "only")
	}
	if q.Title != "" {
		query.Set("query", q.Title)
	}
	if q.Year > 0 {
		query.Set("year", strconv.Itoa(q.Year))
	}
	if q.TMDbID != "" {
		if q.MediaType == "episode" {
			query.Set("parent_tmdb_id", q.TMDbID)
		} else {
			query.Set("tmdb_id", q.TMDbID)
		}
	}
	if q.IMDbID != "" {
		if n := strings.TrimPrefix(strings.ToLower(q.IMDbID), "tt"); n != "" {
			query.Set("imdb_id", n)
		}
	}
	if q.HasSeason || q.Season > 0 {
		query.Set("season_number", strconv.Itoa(q.Season))
	}
	if q.HasEpisode || q.Episode > 0 {
		query.Set("episode_number", strconv.Itoa(q.Episode))
	}
	var response searchResponse
	if err := c.requestJSON(ctx, http.MethodGet, "/subtitles?"+query.Encode(), nil, &response); err != nil {
		return nil, err
	}
	result := make([]Candidate, 0, len(response.Data))
	for _, row := range response.Data {
		a := row.Attributes
		if len(a.Files) == 0 {
			continue
		}
		file := a.Files[0]
		result = append(result, Candidate{ID: row.ID, FileID: file.FileID, FileName: file.FileName, Release: a.Release, Language: a.Language, HearingImpaired: a.HearingImpaired, Forced: a.ForeignPartsOnly, MovieHashMatch: a.MovieHashMatch, FromTrusted: a.FromTrusted, Title: a.Feature.Title, Year: a.Feature.Year, Season: a.Feature.Season, Episode: a.Feature.Episode, TMDbID: string(a.Feature.TMDbID), IMDbID: string(a.Feature.IMDbID), DownloadCount: a.DownloadCount, Ratings: a.Ratings, Format: strings.ToLower(a.Format)})
	}
	return result, nil
}

func (c *OpenSubtitles) Download(ctx context.Context, fileID int64) ([]byte, string, error) {
	if c.QuotaExceeded() {
		return nil, "", ErrDownloadQuotaExceeded
	}
	if fileID <= 0 {
		return nil, "", fmt.Errorf("invalid OpenSubtitles file ID")
	}
	body, _ := json.Marshal(map[string]int64{"file_id": fileID})
	var result downloadResponse
	if err := c.requestJSON(ctx, http.MethodPost, "/download", body, &result); err != nil {
		return nil, "", err
	}
	if result.Remaining != nil && *result.Remaining <= 0 {
		c.quotaExceeded.Store(true)
	}
	u, err := url.Parse(result.Link)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return nil, "", fmt.Errorf("OpenSubtitles returned an invalid download URL")
	}
	client := *c.client
	originalHost := strings.ToLower(u.Host)
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > 3 {
			return fmt.Errorf("too many subtitle download redirects")
		}
		if req.URL.Scheme != "https" || strings.ToLower(req.URL.Host) != originalHost {
			return fmt.Errorf("subtitle download redirected to an untrusted host")
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := c.do(ctx, req, &client)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("subtitle download returned %s", resp.Status)
	}
	maxBytes := c.maxDownloadBytes
	if maxBytes <= 0 {
		maxBytes = 20 << 20
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(data)) > maxBytes {
		return nil, "", fmt.Errorf("subtitle download exceeds %d bytes", maxBytes)
	}
	return data, result.FileName, nil
}

func (c *OpenSubtitles) requestJSON(ctx context.Context, method, endpoint string, body []byte, target any) error {
	authToken := ""
	c.authMu.Lock()
	if c.authError != nil {
		err := c.authError
		c.authMu.Unlock()
		return fmt.Errorf("%w: %v", ErrAuthentication, err)
	}
	if c.user != "" && c.password != "" && c.token == "" {
		if err := c.login(ctx); err != nil {
			if permanentOpenSubtitlesAuthError(err) {
				c.authError = err
			}
			c.authMu.Unlock()
			if permanentOpenSubtitlesAuthError(err) {
				return fmt.Errorf("%w: %v", ErrAuthentication, err)
			}
			return err
		}
	}
	authToken = c.token
	c.authMu.Unlock()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://api.opensubtitles.com/api/v1"+endpoint, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Api-Key", c.apiKey)
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}
	resp, err := c.do(ctx, req, c.client)
	if err != nil {
		if permanentOpenSubtitlesAuthError(err) {
			c.authMu.Lock()
			if c.authError == nil {
				c.authError = err
			}
			c.authMu.Unlock()
			return fmt.Errorf("%w: %v", ErrAuthentication, err)
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("OpenSubtitles request returned %s", resp.Status)
	}
	const maxResponse = 8 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return err
	}
	if len(data) > maxResponse {
		return fmt.Errorf("OpenSubtitles response exceeds %d bytes", maxResponse)
	}
	if target == nil {
		return nil
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("decode OpenSubtitles response: %w", err)
	}
	return nil
}

func permanentOpenSubtitlesAuthError(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "401") || strings.Contains(message, "403")
}

func (c *OpenSubtitles) do(ctx context.Context, req *http.Request, client *http.Client) (*http.Response, error) {
	var last error
	limit := c.retryLimit
	if limit <= 0 {
		limit = 3
	}
	for attempt := 0; attempt < limit; attempt++ {
		if err := c.limiter.Wait(ctx); err != nil {
			return nil, err
		}
		attemptRequest := req.Clone(ctx)
		if req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			attemptRequest.Body = body
		}
		delay := opensubtitleRetryDelay("", attempt)
		response, err := client.Do(attemptRequest)
		if err != nil {
			last = err
		} else if response.StatusCode >= 200 && response.StatusCode < 300 {
			return response, nil
		} else {
			status := response.StatusCode
			delay = opensubtitleRetryDelay(response.Header.Get("Retry-After"), attempt)
			response.Body.Close()
			if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/download") && status == http.StatusNotAcceptable {
				c.quotaExceeded.Store(true)
				return nil, ErrDownloadQuotaExceeded
			}
			last = fmt.Errorf("OpenSubtitles request returned HTTP %d", status)
			if status < 500 && status != 429 {
				return nil, last
			}
		}
		if attempt < limit-1 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}
	}
	return nil, last
}

func opensubtitleRetryDelay(header string, attempt int) time.Duration {
	var delay time.Duration
	if seconds, err := strconv.Atoi(strings.TrimSpace(header)); err == nil && seconds > 0 {
		delay = time.Duration(seconds) * time.Second
		if delay > 10*time.Second {
			delay = 10 * time.Second
		}
	} else {
		delay = 250 * time.Millisecond * time.Duration(1<<attempt)
		if delay > 2*time.Second {
			delay = 2 * time.Second
		}
	}
	return delay + time.Duration(rand.Int64N(int64(delay/4)+1))
}

func (c *OpenSubtitles) login(ctx context.Context) error {
	body, err := json.Marshal(map[string]string{"username": c.user, "password": c.password})
	if err != nil {
		return err
	}
	var response struct {
		Token string `json:"token"`
	}
	if err := c.requestWithoutLogin(ctx, http.MethodPost, "/login", body, &response); err != nil {
		return err
	}
	if response.Token == "" {
		return fmt.Errorf("OpenSubtitles login returned no token")
	}
	c.token = response.Token
	return nil
}
func (c *OpenSubtitles) requestWithoutLogin(ctx context.Context, method, endpoint string, body []byte, target any) error {
	reader := bytes.NewReader(body)
	req, err := http.NewRequestWithContext(ctx, method, "https://api.opensubtitles.com/api/v1"+endpoint, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Api-Key", c.apiKey)
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.do(ctx, req, c.client)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("OpenSubtitles login returned %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

func validLanguage(value string) bool {
	return ValidLanguage(value)
}

// ValidLanguage accepts a bounded BCP-47-style language tag suitable for the
// API query and Jellyfin-compatible sidecar suffix.
func ValidLanguage(value string) bool {
	if value != strings.TrimSpace(value) {
		return false
	}
	if len(value) < 2 || len(value) > 35 {
		return false
	}
	parts := strings.Split(value, "-")
	if len(parts[0]) < 2 || len(parts[0]) > 3 {
		return false
	}
	for i, part := range parts {
		if len(part) == 0 || len(part) > 8 {
			return false
		}
		for _, r := range part {
			if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (i > 0 && r >= '0' && r <= '9')) {
				return false
			}
		}
	}
	return true
}
