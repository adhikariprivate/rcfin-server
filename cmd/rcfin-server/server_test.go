package main

import (
	"testing"

	"github.com/spf13/viper"
)

func TestValidateCollectionsRequiresUniqueStableIDs(t *testing.T) {
	valid := []collectionConfig{{ID: "movies", Name: "Movies", Type: "movies", Remote: "media:Movies"}}
	if err := validateCollections(valid); err != nil {
		t.Fatalf("valid collection config rejected: %v", err)
	}
	for name, collections := range map[string][]collectionConfig{
		"missing ID": {{Name: "Movies", Type: "movies", Remote: "media:Movies"}},
		"invalid ID": {{ID: "movies/library", Name: "Movies", Type: "movies", Remote: "media:Movies"}},
		"duplicate ID": {
			{ID: "movies", Name: "Movies", Type: "movies", Remote: "media:Movies"},
			{ID: "movies", Name: "Films", Type: "movies", Remote: "media:Films"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateCollections(collections); err == nil {
				t.Fatal("expected collection validation error")
			}
		})
	}
}

func TestApplyLegacyJellyfinConfigSupportsCompactSpellings(t *testing.T) {
	v := viper.New()
	v.Set("jellyfin.servername", "Legacy Name")
	v.Set("jellyfin.imagequalityposter", 42)

	var config configFile
	if err := v.Unmarshal(&config); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	applyLegacyJellyfinConfig(v, &config)

	if config.Jellyfin.ServerName != "Legacy Name" {
		t.Errorf("server name = %q, want legacy value", config.Jellyfin.ServerName)
	}
	if config.Jellyfin.ImageQualityPoster != 42 {
		t.Errorf("poster quality = %d, want legacy value 42", config.Jellyfin.ImageQualityPoster)
	}
}
