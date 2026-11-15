// CollectionRepo provides access to content collections such as movies, shows, etc.
// It is responsible for managing collections, adding new ones, and updating them
// with the latest content from the filesystem.
package collection

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/adhikariprivate/rcfin-server/internal/collection/search"
	"github.com/adhikariprivate/rcfin-server/internal/database"
	"github.com/adhikariprivate/rcfin-server/internal/database/model"
	"github.com/adhikariprivate/rcfin-server/internal/idhash"
	"github.com/adhikariprivate/rcfin-server/internal/rclonestore"
)

// CollectionRepo is a repository holding content collections.
type CollectionRepo struct {
	collections    Collections
	repo           database.Repository
	bleveIndex     *search.Search
	rescanInterval time.Duration
	refreshMu      sync.Mutex
}

type Options struct {
	Collections    []Collection
	Repo           database.Repository
	RescanInterval time.Duration
}

// New creates a new CollectionRepo with the provided options.
func New(options *Options) *CollectionRepo {
	c := &CollectionRepo{
		collections:    options.Collections,
		repo:           options.Repo,
		rescanInterval: options.RescanInterval,
	}
	return c
}

// Close releases the VFS resources owned by each configured library.
func (cr *CollectionRepo) Close() {
	for i := range cr.collections {
		cr.collections[i].Storage.Close()
	}
}

// AddCollection adds a new content collection to the repository.
func (cr *CollectionRepo) AddCollection(name string, ID string,
	collectiontype string, remote string, storage *rclonestore.Library) {
	cr.AddCollectionWithInbox(name, ID, collectiontype, remote, storage, ".Inbox")
}

// AddCollectionWithInbox configures a collection and its explicit intake path.
// An empty inbox disables inbox exclusion.
func (cr *CollectionRepo) AddCollectionWithInbox(name string, ID string,
	collectiontype string, remote string, storage *rclonestore.Library, inbox string) {

	var ct CollectionType
	switch collectiontype {
	case "movies":
		ct = CollectionTypeMovies
	case "shows":
		ct = CollectionTypeShows
	default:
		log.Fatalf("Unknown collection type %s, skipping", collectiontype)
		return
	}

	c := Collection{
		Name:      name,
		ID:        ID,
		Type:      ct,
		Remote:    remote,
		Storage:   storage,
		InboxPath: inbox,
	}
	// If no collection ID is provided, generate one based upon the name.
	if c.ID == "" {
		c.ID = idhash.IdHash(c.Name)
	}

	log.Printf("Adding collection %s, id: %s, type: %s, remote: %s\n", c.Name, c.ID, c.Type, c.Remote)

	cr.collections = append(cr.collections, c)
}

// Init starts scanning the repository for contents for the first time.
func (cr *CollectionRepo) Init() {
	_ = cr.InitContext(context.Background())
}

// InitContext performs the initial scan and search-index build and can be
// cancelled while traversing collection directories.
func (cr *CollectionRepo) InitContext(ctx context.Context) error {
	cr.refreshMu.Lock()
	defer cr.refreshMu.Unlock()
	log.Printf("Initializing collections..")
	// scan all collections without delay
	cr.updateCollections(ctx, 0)
	if err := ctx.Err(); err != nil {
		return err
	}
	// Build search index
	return cr.BuildSearchIndex(ctx)
}

// Background keeps scanning the repository for content changes continously.
func (cr *CollectionRepo) Background(ctx context.Context) {
	interval := cr.rescanInterval
	if interval <= 0 {
		interval = 30 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		log.Printf("Background collection update..")
		cr.Refresh(ctx)
	}
}

// Refresh rescans every collection immediately and rebuilds the search
// index. Directory caches are flushed first so the scan observes current
// remote state regardless of the configured dir cache time.
func (cr *CollectionRepo) Refresh(ctx context.Context) {
	cr.refreshMu.Lock()
	defer cr.refreshMu.Unlock()
	log.Printf("Manual collection refresh..")
	cr.updateCollections(ctx, 0)
	if ctx.Err() != nil {
		return
	}
	cr.BuildSearchIndex(ctx)
}

// RefreshCollection rescans one collection and rebuilds the shared search
// index so completed media moves appear at their final paths immediately.
func (cr *CollectionRepo) RefreshCollection(ctx context.Context, collectionID string) error {
	cr.refreshMu.Lock()
	defer cr.refreshMu.Unlock()
	c := cr.GetCollection(collectionID)
	if c == nil {
		return fmt.Errorf("collection %q not found", collectionID)
	}
	c.stableItemIDs = cr.loadStableItemIDs(ctx, c.ID)
	if c.Storage != nil {
		c.Storage.FlushDirCache()
	}
	switch c.Type {
	case CollectionTypeMovies:
		cr.buildMovies(ctx, c, 0)
	case CollectionTypeShows:
		cr.buildShows(ctx, c, 0)
	default:
		return fmt.Errorf("unsupported collection type %q", c.Type)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return cr.BuildSearchIndex(ctx)
}

// RefreshCollectionWithInventory updates Jellyfin's in-memory library model
// from an already completed unified library/Inbox inventory. Scheduled media
// work uses this to avoid a second remote traversal just to rebuild the index.
func (cr *CollectionRepo) RefreshCollectionWithInventory(ctx context.Context, collectionID string, inventory []InventoryEntry) error {
	cr.refreshMu.Lock()
	defer cr.refreshMu.Unlock()
	c := cr.GetCollection(collectionID)
	if c == nil {
		return fmt.Errorf("collection %q not found", collectionID)
	}
	c.stableItemIDs = cr.loadStableItemIDs(ctx, c.ID)
	var snapshot *model.LibraryInventorySnapshot
	if cr.repo != nil {
		loaded, err := cr.repo.GetLibraryInventory(ctx, c.ID)
		if err != nil {
			return fmt.Errorf("load collection inventory identity: %w", err)
		}
		snapshot = loaded
		if snapshot != nil {
			if c.stableItemIDs == nil {
				c.stableItemIDs = make(map[string]stableItemIdentity)
			}
			for _, entry := range snapshot.Entries {
				if entry.JellyfinID == "" || entry.Scope != InventoryScopeLibrary {
					continue
				}
				c.stableItemIDs[entry.Path] = stableItemIdentity{ID: entry.JellyfinID, ProviderID: entry.JellyfinProviderID}
			}
		}
	}
	playable := InventoryScopeEntries(inventory, false)
	switch c.Type {
	case CollectionTypeMovies:
		cr.buildMoviesFromInventory(ctx, c, 0, playable)
	case CollectionTypeShows:
		cr.buildShowsFromInventory(ctx, c, 0, playable)
	default:
		return fmt.Errorf("unsupported collection type %q", c.Type)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if snapshot != nil {
		if err := cr.persistCatalogModelState(ctx, snapshot, c.Items, inventory); err != nil {
			return fmt.Errorf("persist stable Jellyfin item IDs: %w", err)
		}
	}
	return cr.BuildSearchIndex(ctx)
}

func (cr *CollectionRepo) persistCatalogModelState(ctx context.Context, snapshot *model.LibraryInventorySnapshot, items []Item, inventory []InventoryEntry) error {
	if cr.repo == nil || snapshot == nil {
		return nil
	}
	itemIDs := make(map[string]stableItemIdentity, len(items))
	for _, item := range items {
		if item == nil || item.Path() == "" || item.ID() == "" {
			continue
		}
		itemIDs[item.Path()] = stableItemIdentity{ID: item.ID(), ProviderID: itemProviderID(item)}
	}
	changed := false
	entriesByPath := make(map[string]int, len(snapshot.Entries))
	oldTreeFingerprints := make(map[string]string, len(snapshot.Entries))
	for i := range snapshot.Entries {
		entriesByPath[snapshot.Entries[i].Path] = i
		oldTreeFingerprints[snapshot.Entries[i].Path] = snapshot.Entries[i].TreeFingerprint
	}
	for _, observed := range inventory {
		entry := persistedInventoryEntry(observed)
		if index, exists := entriesByPath[entry.Path]; exists {
			prior := snapshot.Entries[index]
			if prior.Classification == string(InventoryMoved) && prior.Identity != "" && !strings.Contains(entry.Identity, "=") {
				entry.Identity = prior.Identity
			}
			entry.FirstSeenGeneration = prior.FirstSeenGeneration
			entry.LastSeenGeneration = prior.LastSeenGeneration
			entry.Classification = prior.Classification
			entry.PreviousPath = prior.PreviousPath
			entry.JellyfinID = prior.JellyfinID
			entry.JellyfinProviderID = prior.JellyfinProviderID
			entry.MissingSince = prior.MissingSince
			entry.MoveRelinkPending = prior.MoveRelinkPending
			if entry.FirstSeenGeneration == 0 {
				entry.FirstSeenGeneration = snapshot.Generation
			}
			if entry.LastSeenGeneration == 0 {
				entry.LastSeenGeneration = snapshot.Generation
			}
			if !reflect.DeepEqual(prior, entry) {
				changed = true
			}
			snapshot.Entries[index] = entry
		} else {
			entry.FirstSeenGeneration = snapshot.Generation
			entry.LastSeenGeneration = snapshot.Generation
			entry.Classification = string(InventoryBaseline)
			snapshot.Entries = append(snapshot.Entries, entry)
			entriesByPath[entry.Path] = len(snapshot.Entries) - 1
			changed = true
		}
	}
	for itemPath, identity := range itemIDs {
		index, exists := entriesByPath[itemPath]
		if !exists || snapshot.Entries[index].Scope != InventoryScopeLibrary || !snapshot.Entries[index].IsDir {
			continue
		}
		entry := &snapshot.Entries[index]
		if entry.JellyfinID != identity.ID {
			entry.JellyfinID = identity.ID
			changed = true
		}
		if identity.ProviderID != "" && entry.JellyfinProviderID != identity.ProviderID {
			entry.JellyfinProviderID = identity.ProviderID
			changed = true
		}
	}
	populateTreeFingerprints(snapshot.Entries)
	for _, entry := range snapshot.Entries {
		if oldTreeFingerprints[entry.Path] != entry.TreeFingerprint {
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return cr.repo.UpdateLibraryInventorySnapshot(ctx, snapshot)
}

func itemProviderID(item Item) string {
	var providerIDs map[string]string
	switch typed := item.(type) {
	case *Movie:
		providerIDs = typed.Metadata.ProviderIDs()
	case *Show:
		providerIDs = typed.Metadata.ProviderIDs()
	}
	for key, value := range providerIDs {
		if value != "" && (strings.EqualFold(key, "tmdb") || strings.EqualFold(key, "tmdbid") || strings.EqualFold(key, "themoviedb")) {
			return value
		}
	}
	return ""
}

// updateCollections updates the collections with the latest content from remote storage.
// - ScanInterval can be set as wait time between loading details of individual items.
// This can be useful to avoid overloading the remote with too many requests.
func (cr *CollectionRepo) updateCollections(ctx context.Context, scanInterval time.Duration) {
	for i := range cr.collections {
		if ctx.Err() != nil {
			return
		}
		c := &(cr.collections[i])
		c.stableItemIDs = cr.loadStableItemIDs(ctx, c.ID)
		// Drop cached listings so each rescan observes current remote state.
		if c.Storage != nil {
			c.Storage.FlushDirCache()
		}
		switch c.Type {
		case CollectionTypeMovies:
			cr.buildMovies(ctx, c, scanInterval)
		case CollectionTypeShows:
			cr.buildShows(ctx, c, scanInterval)
		default:
			log.Printf("Unknown collection type %s, skipping", c.Type)
		}
	}
}

func (cr *CollectionRepo) loadStableItemIDs(ctx context.Context, collectionID string) map[string]stableItemIdentity {
	if cr.repo == nil {
		return nil
	}
	operations, err := database.ListAllMediaOperations(ctx, cr.repo, collectionID)
	if err != nil {
		log.Printf("Unable to load stable item IDs for collection %q: %v", collectionID, err)
		return nil
	}
	stableIDs := make(map[string]stableItemIdentity)
	for _, operation := range operations {
		if operation.Status != "complete" && operation.Status != "completed" {
			continue
		}
		if operation.Action != "stage_existing" && operation.Action != "move_directory" && operation.Action != "move_directory:root" {
			continue
		}
		if operation.DestinationPath == "" || stableIDs[operation.DestinationPath].ID != "" {
			continue
		}
		var evidence struct {
			StableItemID string `json:"stable_item_id"`
		}
		if json.Unmarshal([]byte(operation.Evidence), &evidence) == nil && evidence.StableItemID != "" {
			stableIDs[operation.DestinationPath] = stableItemIdentity{ID: evidence.StableItemID, ProviderID: operation.ProviderID}
		}
	}
	return stableIDs
}

// GetCollections returns all collections in the repository.
func (cr *CollectionRepo) GetCollections() Collections {
	return cr.collections
}

// Inventory returns a deterministic read-only inventory for a configured
// collection. Set inboxOnly to inspect only the explicitly configured intake
// subtree; normal inventory always excludes it.
func (cr *CollectionRepo) Inventory(ctx context.Context, collectionID string, inboxOnly bool) ([]InventoryEntry, error) {
	c := cr.GetCollection(collectionID)
	if c == nil {
		return nil, fmt.Errorf("collection %q not found", collectionID)
	}
	return InventoryCollection(ctx, c, c.InboxPath, inboxOnly)
}

// GetCollection returns a collection by its ID.
func (cr *CollectionRepo) GetCollection(collectionID string) (c *Collection) {
	for n := range cr.collections {
		if cr.collections[n].ID == collectionID {
			c = &(cr.collections[n])
			return
		}
	}
	return
}

// GetItem returns an item in a collection by its ID or name.
func (cr *CollectionRepo) GetItem(collectionID string, itemName string) (i Item) {
	c := cr.GetCollection(collectionID)
	if c == nil {
		return
	}
	for _, n := range c.Items {
		if n.Name() == itemName || n.ID() == itemName {
			return n
		}
		// If item is a show, also search in seasons and episodes
		switch v := n.(type) {
		case *Show:
			for _, s := range v.Seasons {
				if s.ID() == itemName {
					return &s
				}
				for _, e := range s.Episodes {
					if e.ID() == itemName {
						return &e
					}
				}
			}
		}
	}
	return
}

// GetItemByID returns an item in a collection by its ID.
func (cr *CollectionRepo) GetItemByID(itemID string) (*Collection, Item) {
	for _, c := range cr.collections {
		if i := cr.GetItem(c.ID, itemID); i != nil {
			return &c, i
		}
	}
	return nil, nil
}

// GetShowByID returns a show in a collection by its ID.
func (cr *CollectionRepo) GetShowByID(showID string) (*Collection, *Show) {
	for _, c := range cr.collections {
		for _, i := range c.Items {
			switch v := i.(type) {
			case *Show:
				if v.id == showID {
					return &c, v
				}
			}
		}
	}
	return nil, nil
}

// GetSeasonByID returns a season in a collection by its ID.
func (cr *CollectionRepo) GetSeasonByID(saesonID string) (*Collection, *Show, *Season) {
	// fixme: wooho O(n^^3) "just temporarily.."
	for _, c := range cr.collections {
		for _, i := range c.Items {
			switch v := i.(type) {
			case *Show:
				for _, s := range v.Seasons {
					if s.id == saesonID {
						return &c, v, &s
					}
				}
			}
		}
	}
	return nil, nil, nil
}

// GetEpisodeByID returns an episode in a collection by its ID.
func (cr *CollectionRepo) GetEpisodeByID(episodeID string) (*Collection, *Show, *Season, *Episode) {
	// fixme: wooho O(n^^4) "just temporarily.."
	for _, c := range cr.collections {
		for _, i := range c.Items {
			switch v := i.(type) {
			case *Show:
				for _, s := range v.Seasons {
					for _, e := range s.Episodes {
						if e.id == episodeID {
							return &c, v, &s, &e
						}
					}
				}
			}
		}
	}
	return nil, nil, nil, nil
}

// NextUpInSeries returns the nextup episode in a series based upon list of watched episodes and seriesID.
func (cr *CollectionRepo) NextUpInSeries(watchedEpisodeIDs []string, seriesID string) (nextUpEpisodeIDs []string, e error) {
	c, show := cr.GetShowByID(seriesID)
	if c == nil || show == nil {
		return []string{}, nil
	}

	// Check if any watched episodes belong to this show
	hasWatchedEpisodes := false
	for _, season := range show.Seasons {
		for _, episode := range season.Episodes {
			for _, watchedID := range watchedEpisodeIDs {
				if episode.id == watchedID {
					hasWatchedEpisodes = true
				}
			}
		}
	}
	// If no episodes from this series have been watched, return first episode we can find
	if !hasWatchedEpisodes {
		if len(show.Seasons) > 0 && len(show.Seasons[0].Episodes) > 0 {
			// log.Printf("NextUp: returning first episode of unwatched series %s(%s)\n", show.name, show.id)
			return []string{show.Seasons[0].Episodes[0].id}, nil
		}
	}
	return []string{}, nil
}

// NextUpInCollection returns the nextup episodes in the collection based upon list of watched episodes
func (cr *CollectionRepo) NextUpInCollection(watchedEpisodeIDs []string, seriesID string) (nextUpEpisodeIDs []string, e error) {
	type ShowEntry struct {
		show          *Show
		seasonNumber  int
		episodeNumber int
		seasonIdx     int
		epIdx         int
	}

	// todo: look at seriesID and filter watchedEpisodeIDs to only include episodes from that series
	showMap := make(map[string]ShowEntry)
	for _, episodeID := range watchedEpisodeIDs {
		c, show, season, episode := cr.GetEpisodeByID(episodeID)
		if c == nil || show == nil || season == nil || episode == nil {
			continue
		}
		// NextUp skips everything apart from shows
		if c.Type != CollectionTypeShows {
			continue
		}

		// log.Printf("NextUp: %s(%s) %s, %d-%d\n", show.name, show.id, episode.id, episode.SeasonNo, episode.EpisodeNo)

		// Find season and episode index
		seasonIdx, epIdx := -1, -1
		// seasonIdx = season.SeasonNo - 1
		// epIdx = episode.EpisodeNo - 1
		for si, s := range show.Seasons {
			if s.id == season.id {
				seasonIdx = si
				for ei, e := range s.Episodes {
					if e.id == episode.id {
						epIdx = ei
						break
					}
				}
				break
			}
		}
		if seasonIdx == -1 || epIdx == -1 {
			continue
		}

		entry, exists := showMap[show.id]
		// No entries for this show, add it
		if !exists ||
			// watched item is in next season
			season.seasonno > entry.seasonNumber ||
			// watched item is in same season but next episode
			(season.seasonno == entry.seasonNumber && episode.EpisodeNo > entry.episodeNumber) {
			showMap[show.id] = ShowEntry{
				show:          show,
				seasonNumber:  season.seasonno,
				episodeNumber: episode.EpisodeNo,
				seasonIdx:     seasonIdx,
				epIdx:         epIdx,
			}
		}
	}

	// log.Printf("NextUp: showMap: %+v\n", showMap)

	nextUpEpisodeIDs = make([]string, 0)
	for _, entry := range showMap {
		item := entry.show
		seasonIdx := entry.seasonIdx
		epIdx := entry.epIdx

		if seasonIdx < len(item.Seasons) {
			season := &item.Seasons[seasonIdx]
			if epIdx+1 < len(season.Episodes) {
				// log.Printf("Adding: in same season %s(%s) %s, %d-%d\n", item.name, item.id, season.Episodes[epIdx+1].id, seasonIdx, epIdx+1)
				// Try next episode in same season
				nextUpEpisodeIDs = append(nextUpEpisodeIDs, season.Episodes[epIdx+1].id)
				continue
			}
			// Try first episode in next season
			if seasonIdx+1 < len(item.Seasons) && len(item.Seasons[seasonIdx+1].Episodes) > 0 {
				// log.Printf("Adding: in next season %s(%s) %s, %d-%d\n", item.name, item.id, item.Seasons[seasonIdx+1].Episodes[0].id, seasonIdx+1, 0)
				nextUpEpisodeIDs = append(nextUpEpisodeIDs, item.Seasons[seasonIdx+1].Episodes[0].id)
			}
		}
	}

	return nextUpEpisodeIDs, nil
}

// Statistics contains aggregate stats about the collection.
type Statistics struct {
	// Number of movies.
	MovieCount int
	// Number of shows.
	ShowCount int
	// Number of episodes.
	EpisodeCount int
}

// Statistics returns collection details such as genres, tags, ratings, etc.
func (c *CollectionRepo) GetStatistics() Statistics {
	var movieCount, showCount, episodeCount int
	for _, col := range c.GetCollections() {
		for _, i := range col.Items {
			switch v := i.(type) {
			case *Movie:
				movieCount++
			case *Show:
				showCount++
				for _, season := range v.Seasons {
					episodeCount += len(season.Episodes)
				}
			}
		}
	}

	details := Statistics{
		MovieCount:   movieCount,
		ShowCount:    showCount,
		EpisodeCount: episodeCount,
	}
	return details
}

// GetAllPersonNames returns a list of all person names across all collections
func (c *CollectionRepo) GetAllPersonNames(ctx context.Context) ([]string, error) {
	personNames := make(map[string]struct{})
	for _, col := range c.GetCollections() {
		for _, i := range col.Items {
			for _, p := range i.Actors() {
				personNames[p] = struct{}{}
			}
			for _, p := range i.Directors() {
				personNames[p] = struct{}{}
			}
			for _, p := range i.Writers() {
				personNames[p] = struct{}{}
			}
		}
	}
	names := make([]string, 0, len(personNames))
	for name := range personNames {
		names = append(names, name)
	}
	return names, nil
}

// GenreItemCount returns number of items per genre.
func (c *CollectionRepo) GenreItemCount() map[string]int {
	genreCount := make(map[string]int)
	for _, collection := range c.collections {
		for _, i := range collection.Items {
			for _, g := range i.Genres() {
				if g == "" {
					continue
				}
				if _, found := genreCount[g]; !found {
					genreCount[g] = 1
				} else {
					genreCount[g] += 1
				}
			}
		}
	}
	return genreCount
}
