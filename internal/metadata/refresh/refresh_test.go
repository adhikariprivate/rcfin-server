package refresh

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/adhikariprivate/rcfin-server/internal/collection"
	"github.com/adhikariprivate/rcfin-server/internal/database/model"
)

func TestMetadataOutputsPresentRequiresPreviouslyWrittenSidecars(t *testing.T) {
	item := ItemReport{Files: []FileAction{
		{Destination: "Musk (2026)/poster.jpg", State: "written"},
		{Destination: "Musk (2026)/backdrop.jpg", State: "skipped", Reason: "local artwork already exists"},
		{Destination: "Musk (2026)/movie.nfo", State: "written"},
	}}
	detail, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := json.Marshal(metadataJournalEvidence{Detail: detail})
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{FetchNFO: true, FetchArtwork: true}
	inventory := []collection.InventoryEntry{
		{Path: "Musk (2026)/poster.jpg", Role: collection.InventoryArtwork},
		{Path: "Musk (2026)/backdrop.jpg", Role: collection.InventoryArtwork},
	}
	if metadataOutputsPresent(string(evidence), inventory, "Musk (2026)/movie.nfo", opts) {
		t.Fatal("missing NFO was treated as a complete refresh")
	}
	inventory = append(inventory, collection.InventoryEntry{Path: "Musk (2026)/movie.nfo", Role: collection.InventoryNFO})
	completeInventory := append([]collection.InventoryEntry(nil), inventory...)
	inventory = []collection.InventoryEntry{
		{Path: "Musk (2026)/poster.jpg", Role: collection.InventoryArtwork},
		{Path: "Musk (2026)/movie.nfo", Role: collection.InventoryNFO},
	}
	if metadataOutputsPresent(string(evidence), inventory, "Musk (2026)/movie.nfo", opts) {
		t.Fatal("missing artwork was treated as a complete refresh")
	}
	inventory = completeInventory
	if !metadataOutputsPresent(string(evidence), inventory, "Musk (2026)/movie.nfo", opts) {
		t.Fatal("existing outputs were treated as incomplete")
	}
	if !metadataOutputsPresent(string(evidence), inventory, "Musk (2026)/movie.nfo", Options{FetchNFO: true}) {
		t.Fatal("NFO-only refresh was treated as incomplete because artwork is disabled")
	}
}

func TestRelinkOperationEvidenceKeepsMetadataDecisionCurrent(t *testing.T) {
	oldEntry := collection.InventoryEntry{Path: "Old/Movie.mkv", Size: 10, ModTime: 20}
	newEntry := collection.InventoryEntry{Path: "Renamed/Movie.mkv", Size: 10, ModTime: 20}
	opts := Options{FetchNFO: true, FetchArtwork: true, Language: "en-US", AlgorithmVersion: AlgorithmVersion, RefreshExistingAfter: 3 * time.Hour}
	detail, _ := json.Marshal(ItemReport{Source: oldEntry.Path, Destination: "Old/movie.nfo", Files: []FileAction{{Destination: "Old/poster.jpg", State: "written"}}})
	evidence, _ := json.Marshal(metadataJournalEvidence{Signature: metadataJobSignature(oldEntry, opts), ConfigVersion: metadataJobConfigVersion, Configuration: metadataJobConfiguration(opts), Detail: detail})
	operation := &model.MediaOperation{Action: "metadata_refresh", SourcePath: oldEntry.Path, Evidence: string(evidence)}
	if err := RelinkOperationEvidence(operation, newEntry, "Old", "Renamed"); err != nil {
		t.Fatal(err)
	}
	var updated metadataJournalEvidence
	if err := json.Unmarshal([]byte(operation.Evidence), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Signature != metadataJobSignature(newEntry, opts) {
		t.Fatal("metadata signature was not recalculated for the new path")
	}
	var got ItemReport
	if err := json.Unmarshal(updated.Detail, &got); err != nil {
		t.Fatal(err)
	}
	if got.Source != newEntry.Path || got.Destination != "Renamed/movie.nfo" || got.Files[0].Destination != "Renamed/poster.jpg" {
		t.Fatalf("metadata report paths were not rebased: %#v", got)
	}
}

func TestMetadataOutputsExistRequiresConfiguredSidecars(t *testing.T) {
	inventory := []collection.InventoryEntry{
		{Path: "Musk (2026)/movie.nfo", Role: collection.InventoryNFO},
		{Path: "Musk (2026)/poster.jpg", Role: collection.InventoryArtwork},
		{Path: "Musk (2026)/backdrop.jpg", Role: collection.InventoryArtwork},
	}
	if !metadataOutputsExist(inventory, "Musk (2026)", "Musk (2026)/movie.nfo", Options{FetchNFO: true, FetchArtwork: true}) {
		t.Fatal("complete metadata outputs were not recognized")
	}
	if metadataOutputsExist(inventory[:2], "Musk (2026)", "Musk (2026)/movie.nfo", Options{FetchNFO: true, FetchArtwork: true}) {
		t.Fatal("missing backdrop was treated as a complete output set")
	}
	if metadataOutputsExist(inventory, "Musk (2026)", "Musk (2026)/movie.nfo", Options{}) {
		t.Fatal("no configured outputs were treated as complete")
	}
}
