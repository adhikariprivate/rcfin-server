package collection

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/adhikariprivate/rcfin-server/internal/database"
	"github.com/adhikariprivate/rcfin-server/internal/database/model"
)

type InventoryChangeKind string

const (
	InventoryNew       InventoryChangeKind = "new"
	InventoryChanged   InventoryChangeKind = "changed"
	InventoryUnchanged InventoryChangeKind = "unchanged"
	InventoryBaseline  InventoryChangeKind = "baseline"
	InventoryMoved     InventoryChangeKind = "moved"
	InventoryAmbiguous InventoryChangeKind = "ambiguous"
	InventoryMissing   InventoryChangeKind = "missing_candidate"
	InventoryDeleted   InventoryChangeKind = "missing"
)

const defaultInventoryMissingGrace = 7 * 24 * time.Hour
const inventoryFingerprintMode = "path_role_size_modtime_parsed_identity_for_moves"

type InventoryChange struct {
	Kind          InventoryChangeKind          `json:"kind"`
	Path          string                       `json:"path"`
	OldPath       string                       `json:"old_path,omitempty"`
	Entry         InventoryEntry               `json:"entry,omitempty"`
	PreviousEntry *model.LibraryInventoryEntry `json:"previous_entry,omitempty"`
}

type InventoryReconciliation struct {
	Generation           int64             `json:"generation"`
	Completed            time.Time         `json:"completed_at"`
	ListingMode          string            `json:"listing_mode,omitempty"`
	FingerprintMode      string            `json:"fingerprint_mode,omitempty"`
	BackendIDsObserved   int               `json:"backend_ids_observed,omitempty"`
	Changes              []InventoryChange `json:"changes"`
	AlgorithmChanges     []string          `json:"algorithm_changes,omitempty"`
	ConfigurationChanges []string          `json:"configuration_changes,omitempty"`
}

type InventoryProcessingVersions struct {
	ServerVersion           string
	AlgorithmVersions       map[string]int
	ConfigurationSignatures map[string]string
	MissingGrace            time.Duration
	ListingMode             string
}

// ReconcileLibraryInventory persists a complete inventory only after the
// caller's full remote listing succeeded. It detects unambiguous path-only
// moves using cheap attributes and leaves duplicate matches for review.
func ReconcileLibraryInventory(ctx context.Context, repo database.LibraryInventoryRepo, collectionID string, inventory []InventoryEntry, completed time.Time, versions ...InventoryProcessingVersions) (InventoryReconciliation, error) {
	if repo == nil || collectionID == "" {
		return InventoryReconciliation{}, fmt.Errorf("inventory repository and collection ID are required")
	}
	if completed.IsZero() {
		completed = time.Now().UTC()
	}
	previous, err := repo.GetLibraryInventory(ctx, collectionID)
	if err != nil {
		return InventoryReconciliation{}, fmt.Errorf("load previous inventory: %w", err)
	}
	current := make([]model.LibraryInventoryEntry, 0, len(inventory))
	entriesByPath := make(map[string]InventoryEntry, len(inventory))
	backendIDsObserved := 0
	for _, entry := range inventory {
		if entry.Path == "" {
			return InventoryReconciliation{}, fmt.Errorf("inventory contains an empty path")
		}
		if _, duplicate := entriesByPath[entry.Path]; duplicate {
			return InventoryReconciliation{}, fmt.Errorf("inventory contains duplicate path %q", entry.Path)
		}
		entriesByPath[entry.Path] = entry
		if entry.BackendID != "" {
			backendIDsObserved++
		}
		persisted := persistedInventoryEntry(entry)
		current = append(current, persisted)
	}
	populateTreeFingerprints(current)
	sort.Slice(current, func(i, j int) bool { return current[i].Path < current[j].Path })

	oldByPath := map[string]model.LibraryInventoryEntry{}
	nextGeneration := int64(1)
	if previous != nil {
		nextGeneration = previous.Generation + 1
		// Normalize old catalog fingerprints to the current evidence policy.
		// Earlier versions included backend IDs even though Rclone does not
		// guarantee that every backend returns stable IDs across listings.
		for i := range previous.Entries {
			previous.Entries[i].Fingerprint = persistedInventoryFingerprint(previous.Entries[i])
		}
		populateTreeFingerprints(previous.Entries)
		for _, entry := range previous.Entries {
			oldByPath[entry.Path] = entry
		}
	}
	for i := range current {
		current[i].LastSeenGeneration = nextGeneration
		if prior, ok := oldByPath[current[i].Path]; ok {
			current[i].FirstSeenGeneration = prior.FirstSeenGeneration
			current[i].JellyfinID = prior.JellyfinID
			current[i].JellyfinProviderID = prior.JellyfinProviderID
			if current[i].FirstSeenGeneration == 0 {
				current[i].FirstSeenGeneration = previous.Generation
			}
		} else {
			current[i].FirstSeenGeneration = nextGeneration
		}
	}
	processingVersions := InventoryProcessingVersions{}
	if len(versions) > 0 {
		processingVersions = versions[0]
	}
	missingGrace := processingVersions.MissingGrace
	if missingGrace <= 0 {
		missingGrace = defaultInventoryMissingGrace
	}
	var algorithmChanges []string
	var configurationChanges []string
	if previous != nil && previous.ServerVersion != "" && previous.ServerVersion != processingVersions.ServerVersion {
		for stage, currentVersion := range processingVersions.AlgorithmVersions {
			if currentVersion != 0 && previous.AlgorithmVersions[stage] != 0 && previous.AlgorithmVersions[stage] != currentVersion {
				algorithmChanges = append(algorithmChanges, stage)
			}
		}
	}
	sort.Strings(algorithmChanges)
	if previous != nil {
		for stage, currentSignature := range processingVersions.ConfigurationSignatures {
			if currentSignature == "" {
				continue
			}
			if previousSignature := previous.ConfigurationSignatures[stage]; previousSignature != "" && previousSignature != currentSignature {
				configurationChanges = append(configurationChanges, stage)
			}
		}
	}
	sort.Strings(configurationChanges)
	newByPath := map[string]model.LibraryInventoryEntry{}
	for _, entry := range current {
		newByPath[entry.Path] = entry
	}

	var changes []InventoryChange
	newEntries := make([]model.LibraryInventoryEntry, 0)
	oldMissing := make([]model.LibraryInventoryEntry, 0)
	for _, entry := range current {
		if prior, ok := oldByPath[entry.Path]; ok {
			kind := InventoryUnchanged
			if inventoryEntryWasMissing(prior) || prior.Fingerprint != entry.Fingerprint || prior.Scope != entry.Scope || prior.Role != entry.Role {
				kind = InventoryChanged
			}
			if kind == InventoryUnchanged && prior.Classification == string(InventoryMoved) && prior.PreviousPath != "" && prior.MoveRelinkPending {
				changes = append(changes, InventoryChange{Kind: InventoryMoved, Path: entry.Path, OldPath: prior.PreviousPath, Entry: entriesByPath[entry.Path]})
				continue
			}
			changes = append(changes, InventoryChange{Kind: kind, Path: entry.Path, Entry: entriesByPath[entry.Path]})
			continue
		}
		if previous == nil && entry.Scope != InventoryScopeInbox {
			changes = append(changes, InventoryChange{Kind: InventoryBaseline, Path: entry.Path, Entry: entriesByPath[entry.Path]})
			continue
		}
		newEntries = append(newEntries, entry)
	}
	for _, entry := range oldByPath {
		if _, ok := newByPath[entry.Path]; !ok {
			oldMissing = append(oldMissing, entry)
		}
	}

	matchedOld, matchedNew := map[string]bool{}, map[string]bool{}
	ambiguousOldPaths := map[string]bool{}
	// Directory moves are matched from a path-independent manifest of their
	// descendants. Mark equivalent descendants as moves too, so a folder rename
	// does not look like dozens of unrelated additions and removals.
	oldDirs, newDirs := map[string][]model.LibraryInventoryEntry{}, map[string][]model.LibraryInventoryEntry{}
	for _, entry := range oldMissing {
		if entry.IsDir && entry.TreeFingerprint != "" {
			oldDirs[entry.TreeFingerprint] = append(oldDirs[entry.TreeFingerprint], entry)
		}
	}
	for _, entry := range newEntries {
		if entry.IsDir && entry.TreeFingerprint != "" {
			newDirs[entry.TreeFingerprint] = append(newDirs[entry.TreeFingerprint], entry)
		}
	}
	for fingerprint, oldMatches := range oldDirs {
		newMatches := newDirs[fingerprint]
		if len(oldMatches) == 0 || len(newMatches) == 0 {
			continue
		}
		if len(oldMatches) != 1 || len(newMatches) != 1 {
			for _, oldDir := range oldMatches {
				ambiguousOldPaths[oldDir.Path] = true
				previous := oldDir
				changes = append(changes, InventoryChange{Kind: InventoryAmbiguous, Path: oldDir.Path, OldPath: oldDir.Path, PreviousEntry: &previous})
				for _, child := range oldMissing {
					if child.Path == oldDir.Path || !strings.HasPrefix(child.Path, oldDir.Path+"/") {
						continue
					}
					ambiguousOldPaths[child.Path] = true
					previous := child
					changes = append(changes, InventoryChange{Kind: InventoryAmbiguous, Path: child.Path, OldPath: child.Path, PreviousEntry: &previous})
				}
			}
			for _, newDir := range newMatches {
				matchedNew[newDir.Path] = true
				changes = append(changes, InventoryChange{Kind: InventoryAmbiguous, Path: newDir.Path, Entry: entriesByPath[newDir.Path]})
				for _, child := range newEntries {
					if child.Path == newDir.Path || !strings.HasPrefix(child.Path, newDir.Path+"/") {
						continue
					}
					matchedNew[child.Path] = true
					changes = append(changes, InventoryChange{Kind: InventoryAmbiguous, Path: child.Path, Entry: entriesByPath[child.Path]})
				}
			}
			continue
		}
		oldDir, newDir := oldMatches[0], newMatches[0]
		if oldDir.Scope != newDir.Scope {
			continue
		}
		matchedOld[oldDir.Path], matchedNew[newDir.Path] = true, true
		changes = append(changes, InventoryChange{Kind: InventoryMoved, Path: newDir.Path, OldPath: oldDir.Path, Entry: entriesByPath[newDir.Path]})
		oldChildren, newChildren := map[string]model.LibraryInventoryEntry{}, map[string]model.LibraryInventoryEntry{}
		for _, child := range oldMissing {
			if strings.HasPrefix(child.Path, oldDir.Path+"/") {
				oldChildren[strings.TrimPrefix(child.Path, oldDir.Path+"/")] = child
			}
		}
		for _, child := range newEntries {
			if strings.HasPrefix(child.Path, newDir.Path+"/") {
				newChildren[strings.TrimPrefix(child.Path, newDir.Path+"/")] = child
			}
		}
		for relative, oldChild := range oldChildren {
			newChild, ok := newChildren[relative]
			if !ok || moveSubtreeEntryFingerprint(oldChild) == "" || moveSubtreeEntryFingerprint(oldChild) != moveSubtreeEntryFingerprint(newChild) {
				continue
			}
			matchedOld[oldChild.Path], matchedNew[newChild.Path] = true, true
			changes = append(changes, InventoryChange{Kind: InventoryMoved, Path: newChild.Path, OldPath: oldChild.Path, Entry: entriesByPath[newChild.Path]})
		}
	}
	oldByFingerprint := make(map[string][]model.LibraryInventoryEntry)
	newByFingerprint := make(map[string][]model.LibraryInventoryEntry)
	for _, entry := range oldMissing {
		if !entry.IsDir && !matchedOld[entry.Path] {
			if fingerprint := moveFingerprint(entry); fingerprint != "" {
				oldByFingerprint[fingerprint] = append(oldByFingerprint[fingerprint], entry)
			}
		}
	}
	for _, entry := range newEntries {
		if !entry.IsDir && !matchedNew[entry.Path] {
			if fingerprint := moveFingerprint(entry); fingerprint != "" {
				newByFingerprint[fingerprint] = append(newByFingerprint[fingerprint], entry)
			}
		}
	}
	for fingerprint, oldMatches := range oldByFingerprint {
		newMatches := newByFingerprint[fingerprint]
		if len(oldMatches) == 1 && len(newMatches) == 1 {
			oldEntry, newEntry := oldMatches[0], newMatches[0]
			if oldEntry.IsDir == newEntry.IsDir && oldEntry.Scope == newEntry.Scope {
				changes = append(changes, InventoryChange{Kind: InventoryMoved, Path: newEntry.Path, OldPath: oldEntry.Path, Entry: entriesByPath[newEntry.Path]})
				matchedOld[oldEntry.Path], matchedNew[newEntry.Path] = true, true
			}
		} else if len(oldMatches) > 0 && len(newMatches) > 0 {
			for _, oldEntry := range oldMatches {
				ambiguousOldPaths[oldEntry.Path] = true
				previous := oldEntry
				changes = append(changes, InventoryChange{Kind: InventoryAmbiguous, Path: oldEntry.Path, OldPath: oldEntry.Path, PreviousEntry: &previous})
			}
			for _, newEntry := range newMatches {
				changes = append(changes, InventoryChange{Kind: InventoryAmbiguous, Path: newEntry.Path, Entry: entriesByPath[newEntry.Path]})
				matchedNew[newEntry.Path] = true
			}
		}
	}
	for _, entry := range newEntries {
		if !matchedNew[entry.Path] {
			changes = append(changes, InventoryChange{Kind: InventoryNew, Path: entry.Path, Entry: entriesByPath[entry.Path]})
		}
	}
	for _, entry := range oldMissing {
		if !matchedOld[entry.Path] {
			if ambiguousOldPaths[entry.Path] {
				entry.Classification = string(InventoryAmbiguous)
				if entry.MissingSince == nil {
					missingAt := completed.UTC()
					entry.MissingSince = &missingAt
				}
				if completed.Sub(*entry.MissingSince) < missingGrace {
					current = append(current, entry)
				}
				continue
			}
			if entry.MissingSince == nil {
				missingAt := completed.UTC()
				entry.MissingSince = &missingAt
			}
			previous := entry
			if completed.Sub(*entry.MissingSince) >= missingGrace {
				changes = append(changes, InventoryChange{Kind: InventoryDeleted, Path: entry.Path, PreviousEntry: &previous})
				continue
			}
			changes = append(changes, InventoryChange{Kind: InventoryMissing, Path: entry.Path, PreviousEntry: &previous})
			entry.Classification = string(InventoryMissing)
			current = append(current, entry)
		}
	}
	classificationByPath := make(map[string]InventoryChangeKind, len(changes))
	for _, change := range changes {
		if change.Kind != InventoryMissing {
			classificationByPath[change.Path] = change.Kind
		}
	}
	for i := range current {
		if kind, ok := classificationByPath[current[i].Path]; ok {
			if prior, existed := oldByPath[current[i].Path]; existed && prior.Classification == string(InventoryAmbiguous) && kind == InventoryUnchanged {
				current[i].Classification = string(InventoryAmbiguous)
			} else {
				current[i].Classification = string(kind)
			}
			if kind == InventoryMoved {
				current[i].MoveRelinkPending = true
				for _, change := range changes {
					if change.Path == current[i].Path && change.Kind == InventoryMoved {
						current[i].PreviousPath = change.OldPath
						break
					}
				}
			} else if kind != InventoryUnchanged {
				current[i].PreviousPath = ""
				current[i].MoveRelinkPending = false
			}
		} else if current[i].Classification == "" {
			current[i].Classification = string(InventoryBaseline)
		}
	}
	for _, change := range changes {
		if change.Kind != InventoryMoved {
			continue
		}
		prior, exists := oldByPath[change.OldPath]
		if !exists {
			continue
		}
		for i := range current {
			if current[i].Path != change.Path {
				continue
			}
			current[i].FirstSeenGeneration = prior.FirstSeenGeneration
			if current[i].FirstSeenGeneration == 0 {
				current[i].FirstSeenGeneration = previous.Generation
			}
			if prior.Identity != "" {
				current[i].Identity = prior.Identity
			}
			current[i].JellyfinID = prior.JellyfinID
			current[i].JellyfinProviderID = prior.JellyfinProviderID
			break
		}
	}
	sort.Slice(current, func(i, j int) bool { return current[i].Path < current[j].Path })
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].Path == changes[j].Path {
			if changes[i].Kind == changes[j].Kind {
				return changes[i].OldPath < changes[j].OldPath
			}
			return changes[i].Kind < changes[j].Kind
		}
		return changes[i].Path < changes[j].Path
	})
	uniqueChanges := changes[:0]
	for _, change := range changes {
		if len(uniqueChanges) > 0 {
			previous := uniqueChanges[len(uniqueChanges)-1]
			if previous.Path == change.Path && previous.Kind == change.Kind && previous.OldPath == change.OldPath {
				continue
			}
		}
		uniqueChanges = append(uniqueChanges, change)
	}
	changes = uniqueChanges

	// Keep invalidations pending until the corresponding worker acknowledges
	// success. A scan can complete even when provider work is unavailable or
	// fails; persisting the new versions here would silently lose that work.
	storedAlgorithms := cloneIntMap(processingVersions.AlgorithmVersions)
	storedSignatures := cloneStringMap(processingVersions.ConfigurationSignatures)
	storedServerVersion := processingVersions.ServerVersion
	if previous != nil {
		storedAlgorithms = cloneIntMap(previous.AlgorithmVersions)
		if storedAlgorithms == nil {
			storedAlgorithms = map[string]int{}
		}
		for stage, currentVersion := range processingVersions.AlgorithmVersions {
			if !containsStringValue(algorithmChanges, stage) {
				storedAlgorithms[stage] = currentVersion
			}
		}
		storedSignatures = cloneStringMap(processingVersions.ConfigurationSignatures)
		for stage := range processingVersions.ConfigurationSignatures {
			if containsStringValue(configurationChanges, stage) {
				storedSignatures[stage] = previous.ConfigurationSignatures[stage]
			}
		}
		if len(algorithmChanges) > 0 {
			storedServerVersion = previous.ServerVersion
		}
	}
	snapshot := &model.LibraryInventorySnapshot{CollectionID: collectionID, CompletedAt: completed.UTC(), ServerVersion: storedServerVersion, AlgorithmVersions: storedAlgorithms, ConfigurationSignatures: storedSignatures, ListingMode: processingVersions.ListingMode, FingerprintMode: inventoryFingerprintMode, BackendIDsObserved: backendIDsObserved, Entries: current}
	if err := repo.SaveLibraryInventory(ctx, snapshot); err != nil {
		return InventoryReconciliation{}, fmt.Errorf("save completed inventory: %w", err)
	}
	return InventoryReconciliation{Generation: snapshot.Generation, Completed: snapshot.CompletedAt, ListingMode: snapshot.ListingMode, FingerprintMode: snapshot.FingerprintMode, BackendIDsObserved: snapshot.BackendIDsObserved, Changes: changes, AlgorithmChanges: algorithmChanges, ConfigurationChanges: configurationChanges}, nil
}

func containsStringValue(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func cloneIntMap(source map[string]int) map[string]int {
	if source == nil {
		return nil
	}
	clone := make(map[string]int, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func cloneStringMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	clone := make(map[string]string, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func inventoryEntryWasMissing(entry model.LibraryInventoryEntry) bool {
	return entry.MissingSince != nil && !entry.MissingSince.IsZero()
}

func persistedInventoryEntry(entry InventoryEntry) model.LibraryInventoryEntry {
	identityParts := make([]string, 0, len(entry.ProviderIDs))
	for key, value := range entry.ProviderIDs {
		identityParts = append(identityParts, strings.ToLower(key)+"="+value)
	}
	sort.Strings(identityParts)
	identity := strings.Join(identityParts, ";")
	if identity == "" && entry.Title != "" {
		identity = fmt.Sprintf("%s:%d", strings.ToLower(strings.TrimSpace(entry.Title)), entry.Year)
	}
	// Provider IDs and title/year may change when RCFin writes an NFO. They are
	// retained as rename evidence but excluded from the media fingerprint.
	persisted := model.LibraryInventoryEntry{Path: entry.Path, Scope: entry.Scope, Role: string(entry.Role), BackendID: entry.BackendID, Size: entry.Size, ModTime: entry.ModTime, IsDir: entry.IsDir, Identity: identity}
	persisted.Fingerprint = persistedInventoryFingerprint(persisted)
	return persisted
}

func persistedInventoryFingerprint(entry model.LibraryInventoryEntry) string {
	// Backend IDs remain in the catalog for diagnostics, but are not used as
	// identity evidence unless a backend-specific stability contract exists.
	plain := fmt.Sprintf("%t\x00%s\x00%s\x00%d\x00%d", entry.IsDir, entry.Scope, entry.Role, entry.Size, entry.ModTime)
	hash := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(hash[:])
}

func populateTreeFingerprints(entries []model.LibraryInventoryEntry) {
	ordered := append([]model.LibraryInventoryEntry(nil), entries...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Path < ordered[j].Path })
	paths := make([]string, len(ordered))
	for i := range ordered {
		paths[i] = ordered[i].Path
	}
	for i := range entries {
		if !entries[i].IsDir {
			continue
		}
		var descendants []string
		prefix := entries[i].Path + "/"
		start := sort.SearchStrings(paths, prefix)
		for j := start; j < len(ordered) && strings.HasPrefix(ordered[j].Path, prefix); j++ {
			child := ordered[j]
			relative := strings.TrimPrefix(child.Path, prefix)
			if child.IsDir {
				// Parent directory modification times and backend IDs can change
				// during a rename. Preserve the relative directory layout without
				// treating either field as stable move identity.
				descendants = append(descendants, fmt.Sprintf("D\x00%s\x00%s", relative, child.Scope))
				continue
			}
			descendants = append(descendants, fmt.Sprintf("F\x00%s\x00%s\x00%s\x00%s\x00%d\x00%d", relative, child.Scope, child.Role, child.Identity, child.Size, child.ModTime))
		}
		if len(descendants) == 0 {
			continue
		}
		sort.Strings(descendants)
		hash := sha256.Sum256([]byte(strings.Join(descendants, "\n")))
		entries[i].TreeFingerprint = hex.EncodeToString(hash[:])
	}
}

func moveFingerprint(entry model.LibraryInventoryEntry) string {
	identity := strings.TrimSpace(entry.Identity)
	if identity == "" {
		return ""
	}
	return fmt.Sprintf("%t\x00%s\x00%s\x00%s\x00%d\x00%d", entry.IsDir, entry.Scope, entry.Role, identity, entry.Size, entry.ModTime)
}

func moveSubtreeEntryFingerprint(entry model.LibraryInventoryEntry) string {
	if entry.IsDir {
		return fmt.Sprintf("dir\x00%s\x00%s", entry.Scope, entry.Role)
	}
	return fmt.Sprintf("file\x00%s\x00%s\x00%s\x00%d\x00%d", entry.Scope, entry.Role, entry.Identity, entry.Size, entry.ModTime)
}
