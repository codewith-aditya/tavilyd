package api

import (
	"encoding/json"
	"testing"
)

func TestExaWants(t *testing.T) {
	cases := map[string]bool{
		``:              false,
		`null`:          false,
		`false`:         false,
		`true`:          true,
		`{"query":"x"}`: true,
		`0`:             false,
	}
	for raw, want := range cases {
		if got := wants(json.RawMessage(raw)); got != want {
			t.Fatalf("wants(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestExaSearchValidated(t *testing.T) {
	n := 50
	r := &exaSearchRequest{Query: "  golang context  ", Type: "deep", NumResults: &n,
		Contents: json.RawMessage(`{"text":{"maxCharacters":100},"highlights":true}`)}
	v, opts, err := r.validated()
	if err != nil {
		t.Fatal(err)
	}
	if v.Query != "golang context" || v.SearchDepth != "advanced" {
		t.Fatalf("query/depth = %q/%q", v.Query, v.SearchDepth)
	}
	if v.MaxResults != 20 {
		t.Fatalf("maxResults = %d, want clamp to 20", v.MaxResults)
	}
	if !v.IncludeRawContent.Enabled || !wants(opts.Highlights) {
		t.Fatalf("contents mapping wrong: raw=%v highlights=%v", v.IncludeRawContent.Enabled, wants(opts.Highlights))
	}
	if exaMaxChars(opts.Text) != 100 {
		t.Fatalf("maxChars = %d", exaMaxChars(opts.Text))
	}
}

func TestExaSearchDepth(t *testing.T) {
	for in, want := range map[string]string{"lite": "fast", "auto": "basic", "deep": "advanced", "deep-lite": "advanced", "": "basic"} {
		if got := exaSearchDepth(in); got != want {
			t.Fatalf("exaSearchDepth(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExaSearchValidatedRejectsEmptyQuery(t *testing.T) {
	if _, _, err := (&exaSearchRequest{}).validated(); err == nil {
		t.Fatal("expected empty query error")
	}
}
