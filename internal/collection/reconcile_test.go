package collection

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/adhikariprivate/rcfin-server/internal/database/model"
)

type memoryInventoryRepo struct {
	snapshot *model.LibraryInventorySnapshot
	err      error
}

func (r *memoryInventoryRepo) GetLibraryInventory(_ context.Context, _ string) (*model.LibraryInventorySnapshot, error) {
	return r.snapshot, nil
}

func (r *memoryInventoryRepo) SaveLibraryInventory(_ context.Context, snapshot *model.LibraryInventorySnapshot) error {
	if r.err != nil {
		return r.err
	}
	if r.snapshot != nil {
		snapshot.Generation = r.snapshot.Generation + 1
	} else {
		snapshot.Generation = 1
	}
	r.snapshot = snapshot
	return nil
}

func TestReconcileLibraryInventoryBootstrapsExistingLibraryWithoutEnrichment(t *testing.T) {
	repo := &memoryInventoryRepo{}
	result, err := ReconcileLibraryInventory(context.Background(), repo, "movies", []InventoryEntry{
		{Path: "Film/movie.mkv", Scope: InventoryScopeLibrary, Role: InventoryVideo, Size: 100, ModTime: 20},
		{Path: ".Inbox/New/movie.mkv", Scope: InventoryScopeInbox, Role: InventoryVideo, Size: 100, ModTime: 20},
	}, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if result.Generation != 1 || len(result.Changes) != 2 {
		t.Fatalf("first reconciliation = %#v", result)
	}
	baseline, inboxNew := 0, 0
	for _, change := range result.Changes {
		if change.Kind == InventoryBaseline {
			baseline++
		}
		if change.Kind == InventoryNew {
			inboxNew++
		}
	}
	if baseline != 1 || inboxNew != 1 {
		t.Fatalf("first scan classifications = %#v, want library baseline and Inbox new", result.Changes)
	}
}

func TestReconcileLibraryInventoryRecognizesUniqueRenameAndDuplicateAmbiguity(t *testing.T) {
	repo := &memoryInventoryRepo{snapshot: &model.LibraryInventorySnapshot{Generation: 1, Entries: []model.LibraryInventoryEntry{
		{Path: "old/movie.mkv", Scope: InventoryScopeLibrary, Role: string(InventoryVideo), Size: 100, ModTime: 20, Fingerprint: "fp"},
	}}}
	result, err := ReconcileLibraryInventory(context.Background(), repo, "movies", []InventoryEntry{
		{Path: "new/movie.mkv", Scope: InventoryScopeLibrary, Role: InventoryVideo, Size: 100, ModTime: 20},
	}, time.Unix(200, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Changes) != 1 || result.Changes[0].Kind != InventoryMoved || result.Changes[0].OldPath != "old/movie.mkv" {
		t.Fatalf("rename reconciliation = %#v", result.Changes)
	}

	repo.snapshot = &model.LibraryInventorySnapshot{Generation: 2, Entries: []model.LibraryInventoryEntry{
		{Path: "a/movie.mkv", Scope: InventoryScopeLibrary, Role: string(InventoryVideo), Size: 100, ModTime: 20, Fingerprint: "one"},
		{Path: "b/movie.mkv", Scope: InventoryScopeLibrary, Role: string(InventoryVideo), Size: 100, ModTime: 20, Fingerprint: "two"},
	}}
	result, err = ReconcileLibraryInventory(context.Background(), repo, "movies", []InventoryEntry{
		{Path: "c/movie.mkv", Scope: InventoryScopeLibrary, Role: InventoryVideo, Size: 100, ModTime: 20},
	}, time.Unix(300, 0))
	if err != nil {
		t.Fatal(err)
	}
	ambiguous := 0
	for _, change := range result.Changes {
		if change.Kind == InventoryAmbiguous {
			ambiguous++
		}
	}
	if ambiguous != 3 {
		t.Fatalf("ambiguous duplicate changes = %#v", result.Changes)
	}
}

func TestReconcileLibraryInventoryDoesNotReplaceSnapshotWhenSaveFails(t *testing.T) {
	previous := &model.LibraryInventorySnapshot{Generation: 4}
	repo := &memoryInventoryRepo{snapshot: previous, err: errors.New("db unavailable")}
	_, err := ReconcileLibraryInventory(context.Background(), repo, "movies", []InventoryEntry{{Path: "Film/movie.mkv", Role: InventoryVideo}}, time.Now())
	if err == nil {
		t.Fatal("expected save error")
	}
	if repo.snapshot != previous {
		t.Fatal("failed save replaced prior complete snapshot")
	}
}
