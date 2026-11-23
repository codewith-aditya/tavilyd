package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

func newRequestWithHeader(k, v string) *http.Request {
	r, _ := http.NewRequest(http.MethodPost, "/search", nil)
	r.Header.Set(k, v)
	return r
}

func TestSearchRequestValidateDefaults(t *testing.T) {
	var req SearchRequest
	if err := json.Unmarshal([]byte(`{"query":"  golang context  "}`), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	v, err := req.Validate()
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if v.Query != "golang context" || v.SearchDepth != "basic" || v.MaxResults != 10 || v.Topic != "general" {
		t.Fatalf("unexpected defaults: %+v", v)
	}
}

func TestSearchRequestIncludeAnswerFlexible(t *testing.T) {
	cases := map[string]bool{
		`true`: true, `"basic"`: true, `"advanced"`: true, `false`: false, `"false"`: false, `null`: false, `1`: true,
	}
	for raw, want := range cases {
		var v FlexibleBool
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if bool(v) != want {
			t.Errorf("%s => %v, want %v", raw, v, want)
		}
	}
}

func TestSearchRequestValidateRejects(t *testing.T) {
	max := func(n int) *int { return &n }
	cases := map[string]SearchRequest{
		"empty query":      {Query: "   "},
		"bad depth":        {Query: "q", SearchDepth: "deep"},
		"max_results high": {Query: "q", MaxResults: max(21)},
		"bad topic":        {Query: "q", Topic: "images"},
		"bad time range":   {Query: "q", TimeRange: "decade"},
		"bad domain":       {Query: "q", IncludeDomains: []string{"not a domain"}},
	}
	for name, req := range cases {
		if _, err := req.Validate(); err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
}

func TestExtractRequestAcceptsStringOrList(t *testing.T) {
	var one ExtractRequest
	if err := json.Unmarshal([]byte(`{"urls":"https://example.com/a"}`), &one); err != nil {
		t.Fatalf("single: %v", err)
	}
	if len(one.URLs) != 1 {
		t.Fatalf("single url parse: %v", one.URLs)
	}
	var many ExtractRequest
	if err := json.Unmarshal([]byte(`{"urls":["https://a.com","https://b.com"],"format":"text","extract_depth":"advanced"}`), &many); err != nil {
		t.Fatalf("list: %v", err)
	}
	v, err := many.Validate()
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(v.URLs) != 2 || v.Format != "text" || v.ExtractDepth != "advanced" {
		t.Fatalf("unexpected: %+v", v)
	}
}

func TestExtractRequestValidateRejects(t *testing.T) {
	var req ExtractRequest
	if err := json.Unmarshal([]byte(`{"urls":[]}`), &req); err != nil {
		t.Fatal(err)
	}
	if _, err := req.Validate(); err == nil {
		t.Fatal("expected empty urls to fail")
	}
	var bad ExtractRequest
	if err := json.Unmarshal([]byte(`{"urls":["ftp://example.com/x"]}`), &bad); err != nil {
		t.Fatal(err)
	}
	if _, err := bad.Validate(); err == nil {
		t.Fatal("expected non-http scheme to fail")
	}
	var tooMany ExtractRequest
	urls := make([]string, 21)
	for i := range urls {
		urls[i] = "https://example.com/" + string(rune('a'+i))
	}
	payload, _ := json.Marshal(map[string]any{"urls": urls})
	if err := json.Unmarshal(payload, &tooMany); err != nil {
		t.Fatal(err)
	}
	if _, err := tooMany.Validate(); err == nil {
		t.Fatal("expected 21 urls to fail")
	}
}

func TestBearerTokenExtraction(t *testing.T) {
	r := newRequestWithHeader("Authorization", "Bearer secret")
	if got := bearerToken(r); got != "secret" {
		t.Fatalf("got %q", got)
	}
	r2 := newRequestWithHeader("Authorization", "Basic abc")
	if got := bearerToken(r2); got != "" {
		t.Fatalf("got %q", got)
	}
	r3 := newRequestWithHeader("X-API-Key", "k")
	if got := bearerToken(r3); got != "k" {
		t.Fatalf("got %q", got)
	}
}
