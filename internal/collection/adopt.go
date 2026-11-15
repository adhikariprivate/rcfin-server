package collection

import (
	"fmt"
	"path"
	"strings"

	"github.com/adhikariprivate/rcfin-server/internal/database/model"
)

// AdoptInventoryMove accepts an administrator's explicit resolution for one
// ambiguous media-file move. It changes catalog identity only; it never moves
// remote files. The caller must have just reconciled a complete inventory.
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
	if oldEntry.IsDir || newEntry.IsDir || oldEntry.Role != string(InventoryVideo) || newEntry.Role != string(InventoryVideo) {
		return fmt.Errorf("move adoption currently requires one video file, not a folder")
	}
	if !inventoryEntryWasMissing(oldEntry) {
		return fmt.Errorf("source %q is not a missing candidate", fromPath)
	}
	if oldEntry.Scope != newEntry.Scope || oldEntry.Fingerprint != newEntry.Fingerprint {
		return fmt.Errorf("source and destination do not have matching scope and inventory fingerprints")
	}
	newEntry.FirstSeenGeneration = oldEntry.FirstSeenGeneration
	if newEntry.FirstSeenGeneration == 0 {
		newEntry.FirstSeenGeneration = snapshot.Generation
	}
	if oldEntry.Identity != "" {
		newEntry.Identity = oldEntry.Identity
	}
	newEntry.Classification = string(InventoryMoved)
	newEntry.PreviousPath = fromPath
	newEntry.MoveRelinkPending = true
	newEntry.MissingSince = nil
	snapshot.Entries[toIndex] = newEntry
	kept := snapshot.Entries[:0]
	for _, entry := range snapshot.Entries {
		if entry.Path != fromPath {
			kept = append(kept, entry)
		}
	}
	snapshot.Entries = kept
	return nil
}

func validateInventoryRelativePath(value string) error {
	if strings.TrimSpace(value) != value || value == "" || path.IsAbs(value) || path.Clean(value) != value || value == "." || value == ".." || strings.HasPrefix(value, "../") || strings.Contains(value, "\\") {
		return fmt.Errorf("must be a clean relative path beneath the library")
	}
	return nil
}
