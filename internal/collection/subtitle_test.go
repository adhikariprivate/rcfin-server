package collection

import "testing"

func TestParseSubtitleSidecar(t *testing.T) {
	tests := []struct {
		name string
		want Subs
	}{
		{"Film.en.default.srt", Subs{Lang: "en", Path: "Film.en.default.srt", IsDefault: true}},
		{"Film.fr.forced.srt", Subs{Lang: "fr", Path: "Film.fr.forced.srt", IsForced: true}},
		{"Film.pt-BR.sdh.vtt", Subs{Lang: "pt-br", Path: "Film.pt-BR.sdh.vtt", IsHearingImpaired: true}},
		{"Film.hi.srt", Subs{Lang: "hi", Path: "Film.hi.srt"}},
		{"Film.en.hi.srt", Subs{Lang: "en", Path: "Film.en.hi.srt", IsHearingImpaired: true}},
		{"Film.srt", Subs{Lang: "und", Path: "Film.srt"}},
		{"Film.en.commentary.srt", Subs{Lang: "en", Path: "Film.en.commentary.srt", Title: "commentary"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _, ok := parseSubtitleSidecar("Film", tt.name)
			if !ok {
				t.Fatal("sidecar was not recognized")
			}
			if got != tt.want {
				t.Fatalf("parseSubtitleSidecar() = %#v, want %#v", got, tt.want)
			}
		})
	}
	if _, _, ok := parseSubtitleSidecar("Film", "Filmography.en.srt"); ok {
		t.Fatal("subtitle with a non-matching video stem was accepted")
	}
}
