package refresh

import (
	"encoding/json"
	"testing"

	"github.com/adhikariprivate/rcfin-server/internal/collection"
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
