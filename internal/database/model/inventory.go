package model

import "time"

// LibraryInventoryEntry is the durable, provider-independent portion of a
// remote object observed during a complete library scan.
type LibraryInventoryEntry struct {
	Path            string    `json:"path"`
	Scope           string    `json:"scope"`
	Role            string    `json:"role"`
	Size            int64     `json:"size"`
	ModTime         int64     `json:"mod_time"`
	IsDir           bool      `json:"is_dir"`
	Fingerprint     string    `json:"fingerprint"`
	TreeFingerprint string    `json:"tree_fingerprint,omitempty"`
	Identity        string    `json:"identity,omitempty"`
	MissingSince    time.Time `json:"missing_since,omitempty"`
}

// LibraryInventorySnapshot is the last fully completed inventory generation.
// The repository increments Generation atomically when it stores a snapshot.
type LibraryInventorySnapshot struct {
	CollectionID      string                  `json:"collection_id"`
	Generation        int64                   `json:"generation"`
	CompletedAt       time.Time               `json:"completed_at"`
	ServerVersion     string                  `json:"server_version,omitempty"`
	AlgorithmVersions map[string]int          `json:"algorithm_versions,omitempty"`
	Entries           []LibraryInventoryEntry `json:"entries"`
}
