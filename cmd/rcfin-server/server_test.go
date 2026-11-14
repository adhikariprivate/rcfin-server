package main

import (
	"testing"

	"github.com/spf13/viper"
)

func TestApplyLegacyJellyfinConfigSupportsAutoregisterSpelling(t *testing.T) {
	v := viper.New()
	v.Set("jellyfin.autoregister", true)
	v.Set("jellyfin.servername", "Legacy Name")
	v.Set("jellyfin.imagequalityposter", 42)

	var config configFile
	if err := v.Unmarshal(&config); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	applyLegacyJellyfinConfig(v, &config)

	if !config.Jellyfin.AutoRegister {
		t.Error("legacy autoregister key was ignored")
	}
	if config.Jellyfin.ServerName != "Legacy Name" {
		t.Errorf("server name = %q, want legacy value", config.Jellyfin.ServerName)
	}
	if config.Jellyfin.ImageQualityPoster != 42 {
		t.Errorf("poster quality = %d, want legacy value 42", config.Jellyfin.ImageQualityPoster)
	}
}
