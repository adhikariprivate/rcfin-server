package collection

import (
	"path"
	"strings"

	"github.com/adhikariprivate/rcfin-server/internal/rclonestore"
)

func (c *Collection) isInboxPath(candidate string) bool {
	inbox := strings.Trim(path.Clean(c.InboxPath), "/")
	if inbox == "" || inbox == "." {
		return false
	}
	candidate = strings.Trim(path.Clean(candidate), "/")
	return candidate == inbox || strings.HasPrefix(candidate, inbox+"/")
}

type Collection struct {
	// Unique identifier for the collection. Hash of the collection name, or taken from configfile.
	ID string
	// Name of the collection, .e.g., "My Favorite Movies"
	Name string
	// Type of the collection, e.g., "movies", "shows"
	Type CollectionType
	// Items in the collection, could be type movies or shows
	Items []Item
	// Remote is the rclone remote path serving this collection,
	// for example "movies:Films".
	Remote string
	// Storage is the library-specific rclone filesystem and VFS.
	Storage *rclonestore.Library
	// InboxPath is an explicit collection-relative intake subtree. Normal
	// library scans exclude it so unfinished arrivals never appear playable.
	InboxPath string
	// stableItemIDs maps completed journaled move destinations to their prior
	// Jellyfin item IDs so path normalization does not break client references.
	stableItemIDs map[string]stableItemIdentity
}

type stableItemIdentity struct {
	ID         string
	ProviderID string
}

// StableItemID returns the item ID restored from a completed organization
// move at the given collection-relative destination path. If both the move
// record and current metadata have provider IDs, they must agree.
func (c *Collection) StableItemID(itemPath string, providerIDs map[string]string) string {
	if c == nil {
		return ""
	}
	identity := c.stableItemIDs[itemPath]
	if identity.ID == "" {
		return ""
	}
	if identity.ProviderID != "" {
		for key, value := range providerIDs {
			if strings.EqualFold(key, "tmdb") || strings.EqualFold(key, "tmdbid") || strings.EqualFold(key, "themoviedb") {
				if value != "" && value != identity.ProviderID {
					return ""
				}
			}
		}
	}
	return identity.ID
}

type CollectionType string

const (
	CollectionTypeMovies CollectionType = "movies"
	CollectionTypeShows  CollectionType = "shows"
)

type Collections []Collection
