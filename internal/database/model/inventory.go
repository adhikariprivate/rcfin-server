package model

import "time"

// LibraryInventoryEntry is the durable, provider-independent portion of a
// remote object observed during a complete library scan.
type LibraryInventoryEntry struct {
	Path                string     `json:"path"`
	PreviousPath        string     `json:"previous_path,omitempty"`
	FirstSeenGeneration int64      `json:"first_seen_generation,omitempty"`
	LastSeenGeneration  int64      `json:"last_seen_generation,omitempty"`
	Scope               string     `json:"scope"`
	Role                string     `json:"role"`
	BackendID           string     `json:"backend_id,omitempty"`
	Classification      string     `json:"classification,omitempty"`
	MoveRelinkPending   bool       `json:"move_relink_pending,omitempty"`
	Size                int64      `json:"size"`
	ModTime             int64      `json:"mod_time"`
	IsDir               bool       `json:"is_dir"`
	Fingerprint         string     `json:"fingerprint"`
	TreeFingerprint     string     `json:"tree_fingerprint,omitempty"`
	Identity            string     `json:"identity,omitempty"`
	JellyfinID          string     `json:"jellyfin_id,omitempty"`
	JellyfinProviderID  string     `json:"jellyfin_provider_id,omitempty"`
	MissingSince        *time.Time `json:"missing_since,omitempty"`
}

// LibraryInventorySnapshot is the last fully completed inventory generation.
// The repository increments Generation atomically when it stores a snapshot.
type LibraryInventorySnapshot struct {
	CollectionID            string                  `json:"collection_id"`
	Generation              int64                   `json:"generation"`
	CompletedAt             time.Time               `json:"completed_at"`
	ServerVersion           string                  `json:"server_version,omitempty"`
	AlgorithmVersions       map[string]int          `json:"algorithm_versions,omitempty"`
	ConfigurationSignatures map[string]string       `json:"configuration_signatures,omitempty"`
	ListingMode             string                  `json:"listing_mode,omitempty"`
	FingerprintMode         string                  `json:"fingerprint_mode,omitempty"`
	BackendIDsObserved      int                     `json:"backend_ids_observed,omitempty"`
	Entries                 []LibraryInventoryEntry `json:"entries"`
}
