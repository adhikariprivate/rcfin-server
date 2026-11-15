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
)

type InventoryChange struct {
	Kind    InventoryChangeKind `json:"kind"`
	Path    string              `json:"path"`
	OldPath string              `json:"old_path,omitempty"`
	Entry   InventoryEntry      `json:"entry,omitempty"`
}

type InventoryReconciliation struct {
	Generation int64             `json:"generation"`
	Completed  time.Time         `json:"completed_at"`
	Changes    []InventoryChange `json:"changes"`
}

// ReconcileLibraryInventory persists a complete inventory only after the
// caller's full remote listing succeeded. It detects unambiguous path-only
// moves using cheap attributes and leaves duplicate matches for review.
func ReconcileLibraryInventory(ctx context.Context, repo database.LibraryInventoryRepo, collectionID string, inventory []InventoryEntry, completed time.Time) (InventoryReconciliation, error) {
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
	for _, entry := range inventory {
		if entry.Path == "" {
			return InventoryReconciliation{}, fmt.Errorf("inventory contains an empty path")
		}
		if _, duplicate := entriesByPath[entry.Path]; duplicate {
			return InventoryReconciliation{}, fmt.Errorf("inventory contains duplicate path %q", entry.Path)
		}
		entriesByPath[entry.Path] = entry
		current = append(current, persistedInventoryEntry(entry))
	}
	sort.Slice(current, func(i, j int) bool { return current[i].Path < current[j].Path })

	oldByPath := map[string]model.LibraryInventoryEntry{}
	if previous != nil {
		for _, entry := range previous.Entries {
			oldByPath[entry.Path] = entry
		}
	}
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
			if prior.Fingerprint != entry.Fingerprint || prior.Scope != entry.Scope || prior.Role != entry.Role {
				kind = InventoryChanged
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

	oldByFingerprint := make(map[string][]model.LibraryInventoryEntry)
	newByFingerprint := make(map[string][]model.LibraryInventoryEntry)
	for _, entry := range oldMissing {
		oldByFingerprint[moveFingerprint(entry)] = append(oldByFingerprint[moveFingerprint(entry)], entry)
	}
	for _, entry := range newEntries {
		newByFingerprint[moveFingerprint(entry)] = append(newByFingerprint[moveFingerprint(entry)], entry)
	}
	matchedOld, matchedNew := map[string]bool{}, map[string]bool{}
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
				changes = append(changes, InventoryChange{Kind: InventoryAmbiguous, Path: oldEntry.Path, OldPath: oldEntry.Path})
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
			changes = append(changes, InventoryChange{Kind: InventoryMissing, Path: entry.Path})
		}
	}
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].Path == changes[j].Path {
			return changes[i].Kind < changes[j].Kind
		}
		return changes[i].Path < changes[j].Path
	})

	snapshot := &model.LibraryInventorySnapshot{CollectionID: collectionID, CompletedAt: completed.UTC(), Entries: current}
	if err := repo.SaveLibraryInventory(ctx, snapshot); err != nil {
		return InventoryReconciliation{}, fmt.Errorf("save completed inventory: %w", err)
	}
	return InventoryReconciliation{Generation: snapshot.Generation, Completed: snapshot.CompletedAt, Changes: changes}, nil
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
	plain := fmt.Sprintf("%t\x00%s\x00%s\x00%d\x00%d", entry.IsDir, entry.Scope, entry.Role, entry.Size, entry.ModTime)
	hash := sha256.Sum256([]byte(plain))
	return model.LibraryInventoryEntry{Path: entry.Path, Scope: entry.Scope, Role: string(entry.Role), Size: entry.Size, ModTime: entry.ModTime, IsDir: entry.IsDir, Fingerprint: hex.EncodeToString(hash[:]), Identity: identity}
}

func moveFingerprint(entry model.LibraryInventoryEntry) string {
	return fmt.Sprintf("%t\x00%s\x00%s\x00%d\x00%d", entry.IsDir, entry.Scope, entry.Role, entry.Size, entry.ModTime)
}
