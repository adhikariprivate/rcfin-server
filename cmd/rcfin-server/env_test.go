package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDotEnvPreservesProcessEnvironmentAndReadsQuotedValues(t *testing.T) {
	t.Setenv("RCFIN_DOTENV_EXISTING_TEST", "process")
	filename := filepath.Join(t.TempDir(), ".env")
	contents := "# local provider credentials\nexport RCFIN_DOTENV_EXISTING_TEST=dotenv\nRCFIN_DOTENV_QUOTED_TEST=\"value with spaces\" # quoted note\nRCFIN_DOTENV_COMMENT_TEST=plain # ignored note\n"
	if err := os.WriteFile(filename, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := loadDotEnv(filename); err != nil {
		t.Fatalf("loadDotEnv() error = %v", err)
	}
	if got := os.Getenv("RCFIN_DOTENV_EXISTING_TEST"); got != "process" {
		t.Fatalf("existing process value = %q, want process", got)
	}
	if got := os.Getenv("RCFIN_DOTENV_QUOTED_TEST"); got != "value with spaces" {
		t.Fatalf("quoted dotenv value = %q", got)
	}
	if got := os.Getenv("RCFIN_DOTENV_COMMENT_TEST"); got != "plain" {
		t.Fatalf("dotenv value with comment = %q, want plain", got)
	}
}

func TestApplyProviderCredentialAliases(t *testing.T) {
	for _, key := range []string{
		"RCFIN_TMDB_API_KEY", "TMDB_READ_ACCESS_TOKEN", "TMDB_API_KEY",
		"RCFIN_OPENSUBTITLES_API_KEY", "OPENSUBTITLES_API_KEY",
		"RCFIN_OPENSUBTITLES_USERNAME", "OPENSUBTITLES_USERNAME",
		"RCFIN_OPENSUBTITLES_PASSWORD", "OPENSUBTITLES_PASSWORD",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("TMDB_READ_ACCESS_TOKEN", "read-token")
	t.Setenv("TMDB_API_KEY", "legacy-key")
	t.Setenv("OPENSUBTITLES_API_KEY", "opensub-key")
	t.Setenv("OPENSUBTITLES_USERNAME", "account-name")
	t.Setenv("OPENSUBTITLES_PASSWORD", "account-password")
	if err := applyProviderCredentialAliases(); err != nil {
		t.Fatalf("applyProviderCredentialAliases() error = %v", err)
	}
	for key, want := range map[string]string{
		"RCFIN_TMDB_API_KEY":           "read-token",
		"RCFIN_OPENSUBTITLES_API_KEY":  "opensub-key",
		"RCFIN_OPENSUBTITLES_USERNAME": "account-name",
		"RCFIN_OPENSUBTITLES_PASSWORD": "account-password",
	} {
		if got := os.Getenv(key); got != want {
			t.Errorf("%s = %q, want mapped value", key, got)
		}
	}
}

func TestApplyProviderCredentialAliasesKeepsExplicitValues(t *testing.T) {
	t.Setenv("RCFIN_TMDB_API_KEY", "explicit")
	t.Setenv("TMDB_READ_ACCESS_TOKEN", "dotenv")
	if err := applyProviderCredentialAliases(); err != nil {
		t.Fatalf("applyProviderCredentialAliases() error = %v", err)
	}
	if got := os.Getenv("RCFIN_TMDB_API_KEY"); got != "explicit" {
		t.Fatalf("explicit RCFIN value = %q, want explicit", got)
	}
}
