package collection

import (
	"fmt"
	"path"
	"strings"

	"github.com/adhikariprivate/rcfin-server/internal/database/model"
)

// AdoptInventoryMove accepts an administrator's explicit resolution for one
// ambiguous media-file or verified directory-subtree move. It changes catalog
// identity only; it never moves remote files. The caller must have just
// reconciled a complete inventory.
func AdoptInventoryMove(snapshot *model.LibraryInventorySnapshot, fromPath, toPath string) error {
	if snapshot == nil || snapshot.Generation <= 0 {
		return fmt.Errorf("a completed inventory snapshot is required")
	}
	if err := validateInventoryRelativePath(fromPath); err != nil {
		return fmt.Errorf("invalid source path: %w", err)
	}
	if err := validateInventoryRelativePath(toPath); err != nil {
		return fmt.Errorf("invalid destination path: %w", err)
	}
	if fromPath == toPath {
		return fmt.Errorf("source and destination paths are identical")
	}
	if strings.HasPrefix(fromPath, toPath+"/") || strings.HasPrefix(toPath, fromPath+"/") {
		return fmt.Errorf("source and destination paths cannot contain one another")
	}
	fromIndex, toIndex := -1, -1
	for i := range snapshot.Entries {
		if snapshot.Entries[i].Path == fromPath {
			fromIndex = i
		}
		if snapshot.Entries[i].Path == toPath {
			toIndex = i
		}
	}
	if fromIndex < 0 || toIndex < 0 {
		return fmt.Errorf("source and destination must both exist in the current catalog")
	}
	oldEntry := snapshot.Entries[fromIndex]
	newEntry := snapshot.Entries[toIndex]
	if oldEntry.IsDir != newEntry.IsDir || oldEntry.Scope != newEntry.Scope {
		return fmt.Errorf("source and destination must have the same entry type and scope")
	}
	if oldEntry.IsDir {
		return adoptInventoryDirectoryMove(snapshot, fromPath, toPath, oldEntry, newEntry)
	}
	if oldEntry.Role != string(InventoryVideo) || newEntry.Role != string(InventoryVideo) {
		return fmt.Errorf("move adoption requires a video file or a verified directory subtree")
	}
	if !inventoryEntryWasMissing(oldEntry) {
		return fmt.Errorf("source %q is not a missing candidate", fromPath)
	}
	if oldEntry.Fingerprint == "" || oldEntry.Fingerprint != newEntry.Fingerprint {
		return fmt.Errorf("source and destination do not have matching inventory fingerprints")
	}
	carryInventoryIdentity(&newEntry, oldEntry, snapshot.Generation, fromPath)
	snapshot.Entries[toIndex] = newEntry
	snapshot.Entries = removeInventorySubtree(snapshot.Entries, fromPath)
	return nil
}

func adoptInventoryDirectoryMove(snapshot *model.LibraryInventorySnapshot, fromPath, toPath string, oldRoot, newRoot model.LibraryInventoryEntry) error {
	if !inventoryEntryWasMissing(oldRoot) {
		return fmt.Errorf("source directory %q is not a missing candidate", fromPath)
	}
	if oldRoot.TreeFingerprint == "" || oldRoot.TreeFingerprint != newRoot.TreeFingerprint {
		return fmt.Errorf("source and destination directories do not have matching subtree fingerprints")
	}
	oldByRelative := make(map[string]model.LibraryInventoryEntry)
	newByRelative := make(map[string]model.LibraryInventoryEntry)
	for _, entry := range snapshot.Entries {
		if strings.HasPrefix(entry.Path, fromPath+"/") {
			oldByRelative[strings.TrimPrefix(entry.Path, fromPath+"/")] = entry
		}
		if strings.HasPrefix(entry.Path, toPath+"/") {
			newByRelative[strings.TrimPrefix(entry.Path, toPath+"/")] = entry
		}
	}
	if len(oldByRelative) == 0 || len(oldByRelative) != len(newByRelative) {
		return fmt.Errorf("directory move does not have a complete one-to-one descendant match")
	}
	for relative, oldEntry := range oldByRelative {
		newEntry, exists := newByRelative[relative]
		if !exists || oldEntry.IsDir != newEntry.IsDir || oldEntry.Scope != newEntry.Scope || oldEntry.Role != newEntry.Role || oldEntry.Fingerprint == "" || oldEntry.Fingerprint != newEntry.Fingerprint {
			return fmt.Errorf("directory descendant %q has no matching destination entry", relative)
		}
	}
	for i := range snapshot.Entries {
		entry := snapshot.Entries[i]
		if entry.Path != toPath && !strings.HasPrefix(entry.Path, toPath+"/") {
			continue
		}
		oldPath := fromPath + strings.TrimPrefix(entry.Path, toPath)
		oldEntry, exists := func() (model.LibraryInventoryEntry, bool) {
			if entry.Path == toPath {
				return oldRoot, true
			}
			matched, ok := oldByRelative[strings.TrimPrefix(entry.Path, toPath+"/")]
			return matched, ok
		}()
		if !exists {
			return fmt.Errorf("directory descendant %q has no source catalog entry", entry.Path)
		}
		carryInventoryIdentity(&entry, oldEntry, snapshot.Generation, oldPath)
		snapshot.Entries[i] = entry
	}
	snapshot.Entries = removeInventorySubtree(snapshot.Entries, fromPath)
	return nil
}

func carryInventoryIdentity(destination *model.LibraryInventoryEntry, source model.LibraryInventoryEntry, generation int64, sourcePath string) {
	destination.FirstSeenGeneration = source.FirstSeenGeneration
	if destination.FirstSeenGeneration == 0 {
		destination.FirstSeenGeneration = generation
	}
	if source.Identity != "" {
		destination.Identity = source.Identity
	}
	destination.Classification = string(InventoryMoved)
	destination.PreviousPath = sourcePath
	destination.MoveRelinkPending = true
	destination.MissingSince = nil
}

func removeInventorySubtree(entries []model.LibraryInventoryEntry, root string) []model.LibraryInventoryEntry {
	kept := entries[:0]
	for _, entry := range entries {
		if entry.Path != root && !strings.HasPrefix(entry.Path, root+"/") {
			kept = append(kept, entry)
		}
	}
	return kept
}

func validateInventoryRelativePath(value string) error {
	if strings.TrimSpace(value) != value || value == "" || path.IsAbs(value) || path.Clean(value) != value || value == "." || value == ".." || strings.HasPrefix(value, "../") || strings.Contains(value, "\\") {
		return fmt.Errorf("must be a clean relative path beneath the library")
	}
	return nil
}
