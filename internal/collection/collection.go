package collection

import "github.com/adhikariprivate/rcfin-server/internal/rclonestore"

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
}

type CollectionType string

const (
	CollectionTypeMovies CollectionType = "movies"
	CollectionTypeShows  CollectionType = "shows"
)

type Collections []Collection
