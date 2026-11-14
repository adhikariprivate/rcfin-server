package main

import (
	"net/url"
	"strings"
	"testing"
)

func TestRedactSensitiveQuery(t *testing.T) {
	parsed, err := url.Parse("/Videos/item/Subtitles/2/Stream.vtt?api_key=secret&keep=value&access_token=also-secret")
	if err != nil {
		t.Fatal(err)
	}
	got := redactSensitiveQuery(parsed)
	if strings.Contains(got, "secret") || !strings.Contains(got, "api_key=%5Bredacted%5D") || !strings.Contains(got, "keep=value") {
		t.Fatalf("redacted URL = %q", got)
	}
}
