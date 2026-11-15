package collection

import (
	"context"
	"errors"
	"strings"
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

func TestReconcileLibraryInventoryRecognizesFolderRenameWithoutNewWork(t *testing.T) {
	repo := &memoryInventoryRepo{}
	_, err := ReconcileLibraryInventory(context.Background(), repo, "movies", []InventoryEntry{
		{Path: "Old", Scope: InventoryScopeLibrary, Role: InventoryOther, IsDir: true, ModTime: 20},
		{Path: "Old/Movie.mkv", Scope: InventoryScopeLibrary, Role: InventoryVideo, Size: 100, ModTime: 20},
	}, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	result, err := ReconcileLibraryInventory(context.Background(), repo, "movies", []InventoryEntry{
		{Path: "Renamed", Scope: InventoryScopeLibrary, Role: InventoryOther, IsDir: true, ModTime: 40},
		{Path: "Renamed/Movie.mkv", Scope: InventoryScopeLibrary, Role: InventoryVideo, Size: 100, ModTime: 20},
	}, time.Unix(200, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Changes) != 2 {
		t.Fatalf("folder move should produce exactly two move records, got %#v", result.Changes)
	}
	for _, change := range result.Changes {
		if change.Kind != InventoryMoved || !strings.HasPrefix(change.OldPath, "Old") || !strings.HasPrefix(change.Path, "Renamed") {
			t.Fatalf("folder move produced a false add/remove: %#v", result.Changes)
		}
	}
	for _, entry := range repo.snapshot.Entries {
		if entry.Path == "Renamed/Movie.mkv" && (entry.FirstSeenGeneration != 1 || entry.LastSeenGeneration != 2) {
			t.Fatalf("folder move did not preserve inventory age: %#v", entry)
		}
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

func TestReconcileLibraryInventoryOnlyInvalidatesWhenBuildAndAlgorithmBothChange(t *testing.T) {
	repo := &memoryInventoryRepo{snapshot: &model.LibraryInventorySnapshot{
		Generation: 1, ServerVersion: "1.0+old",
		AlgorithmVersions: map[string]int{"metadata": 2, "subtitles": 4},
		Entries:           []model.LibraryInventoryEntry{{Path: "Film/movie.mkv", Scope: InventoryScopeLibrary, Role: string(InventoryVideo)}},
	}}
	versions := InventoryProcessingVersions{ServerVersion: "1.1+new", AlgorithmVersions: map[string]int{"metadata": 2, "subtitles": 4}}
	result, err := ReconcileLibraryInventory(context.Background(), repo, "movies", []InventoryEntry{{Path: "Film/movie.mkv", Scope: InventoryScopeLibrary, Role: InventoryVideo}}, time.Unix(200, 0), versions)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.AlgorithmChanges) != 0 {
		t.Fatalf("build-only change invalidated algorithms: %#v", result.AlgorithmChanges)
	}
	versions.ServerVersion = "1.2+newer"
	versions.AlgorithmVersions = map[string]int{"metadata": 3, "subtitles": 4}
	result, err = ReconcileLibraryInventory(context.Background(), repo, "movies", []InventoryEntry{{Path: "Film/movie.mkv", Scope: InventoryScopeLibrary, Role: InventoryVideo}}, time.Unix(300, 0), versions)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.AlgorithmChanges) != 1 || result.AlgorithmChanges[0] != "metadata" {
		t.Fatalf("algorithm change invalidation = %#v, want metadata", result.AlgorithmChanges)
	}
}

func TestReconcileLibraryInventoryRetainsMissingTombstoneAndClearsOnReappearance(t *testing.T) {
	repo := &memoryInventoryRepo{snapshot: &model.LibraryInventorySnapshot{Generation: 1, Entries: []model.LibraryInventoryEntry{{
		Path: "Film/movie.mkv", Scope: InventoryScopeLibrary, Role: string(InventoryVideo), Size: 10, ModTime: 20, Fingerprint: "fp",
	}}}}
	firstMissing := time.Unix(1000, 0).UTC()
	result, err := ReconcileLibraryInventory(context.Background(), repo, "movies", nil, firstMissing)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Changes) != 1 || result.Changes[0].Kind != InventoryMissing || len(repo.snapshot.Entries) != 1 || repo.snapshot.Entries[0].MissingSince == nil {
		t.Fatalf("first missing generation = %#v, snapshot=%#v", result.Changes, repo.snapshot.Entries)
	}
	result, err = ReconcileLibraryInventory(context.Background(), repo, "movies", []InventoryEntry{{
		Path: "Film/movie.mkv", Scope: InventoryScopeLibrary, Role: InventoryVideo, Size: 10, ModTime: 20,
	}}, firstMissing.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Changes) != 1 || result.Changes[0].Kind != InventoryChanged || repo.snapshot.Entries[0].MissingSince != nil {
		t.Fatalf("reappeared object was not revived cleanly: %#v %#v", result.Changes, repo.snapshot.Entries)
	}
}

func TestReconcileLibraryInventoryUsesConfiguredMissingGrace(t *testing.T) {
	repo := &memoryInventoryRepo{snapshot: &model.LibraryInventorySnapshot{Generation: 1, Entries: []model.LibraryInventoryEntry{{
		Path: "gone.mkv", Scope: InventoryScopeLibrary, Role: string(InventoryVideo), Fingerprint: "fp",
	}}}}
	missingAt := time.Unix(1000, 0).UTC()
	versions := InventoryProcessingVersions{MissingGrace: time.Hour}
	if _, err := ReconcileLibraryInventory(context.Background(), repo, "movies", nil, missingAt, versions); err != nil {
		t.Fatal(err)
	}
	if _, err := ReconcileLibraryInventory(context.Background(), repo, "movies", nil, missingAt.Add(2*time.Hour), versions); err != nil {
		t.Fatal(err)
	}
	if len(repo.snapshot.Entries) != 0 {
		t.Fatalf("expired tombstone remains in inventory: %#v", repo.snapshot.Entries)
	}
}
