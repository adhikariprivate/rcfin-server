package provider

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

type Resolution struct {
	Metadata    Metadata `json:"metadata"`
	Match       *Match   `json:"match,omitempty"`
	Evidence    []string `json:"evidence"`
	NeedsReview bool     `json:"needs_review"`
}

// Resolve prefers stable identifiers. If an explicit provider ID is present
// but cannot be resolved, it stops for review instead of silently rematching
// the item by title.
func Resolve(ctx context.Context, p Provider, kind MediaType, title string, year int, ids map[string]string, language string, minimum, tieMargin float64) (Resolution, error) {
	if p == nil {
		return Resolution{}, fmt.Errorf("metadata provider is not configured")
	}
	normalizedIDs := make(map[string]string, len(ids))
	for key, id := range ids {
		key = strings.ToLower(strings.TrimSpace(key))
		switch key {
		case "tmdbid":
			key = "tmdb"
		case "tvdbid":
			key = "tvdb"
		case "imdbid":
			key = "imdb"
		}
		if strings.TrimSpace(id) != "" && normalizedIDs[key] == "" {
			normalizedIDs[key] = strings.TrimSpace(id)
		}
	}
	keys := make([]string, 0, len(normalizedIDs))
	for key := range normalizedIDs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var unsupportedID string
	for _, key := range keys {
		id := normalizedIDs[key]
		if id == "" {
			continue
		}
		var meta Metadata
		var err error
		switch key {
		case "tmdb":
			meta, err = p.Get(ctx, kind, id, language)
		case "imdb", "tvdb":
			meta, err = p.FindExternalID(ctx, key, id, kind, language)
		default:
			unsupportedID = key
			continue
		}
		if err != nil {
			return Resolution{Evidence: []string{fmt.Sprintf("explicit %s ID %s could not be resolved", key, id)}, NeedsReview: true}, err
		}
		return Resolution{Metadata: meta, Match: &Match{Candidate: Candidate{ID: meta.ID, Title: meta.Title, Year: meta.Year, Type: meta.Type}, Confidence: 1}, Evidence: []string{fmt.Sprintf("exact %s ID lookup", key)}}, nil
	}
	if unsupportedID != "" {
		return Resolution{Evidence: []string{fmt.Sprintf("explicit %s ID cannot be resolved by the configured provider", unsupportedID)}, NeedsReview: true}, fmt.Errorf("unsupported explicit provider ID %q", unsupportedID)
	}
	if strings.TrimSpace(title) == "" {
		return Resolution{NeedsReview: true, Evidence: []string{"no stable provider ID or usable title"}}, nil
	}
	candidates, err := p.Search(ctx, kind, title, year)
	if err != nil {
		return Resolution{}, err
	}
	ranked := RankCandidates(title, year, candidates, minimum, tieMargin)
	if len(ranked) == 0 {
		return Resolution{NeedsReview: true, Evidence: []string{"provider search returned no candidates"}}, nil
	}
	best := ranked[0]
	if best.NeedsReview {
		return Resolution{Match: &best, Evidence: best.Evidence, NeedsReview: true}, nil
	}
	meta, err := p.Get(ctx, kind, best.Candidate.ID, language)
	if err != nil {
		return Resolution{}, err
	}
	return Resolution{Metadata: meta, Match: &best, Evidence: best.Evidence}, nil
}
