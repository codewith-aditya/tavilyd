package search

import (
	"net/url"
	"testing"
)

func TestCanonical(t *testing.T) {
	cases := map[string]string{
		"HTTPS://WWW.Example.com:443/Path/?utm_source=x&b=2&a=1#frag": "https://example.com/Path?a=1&b=2",
		"http://example.com:80/":                "http://example.com/",
		"http://example.com/Path/":              "http://example.com/Path",
		"http://example.com/a?fbclid=1&gclid=2": "http://example.com/a",
		"https://example.com/a?ref=hn&keep=1":   "https://example.com/a?keep=1",
	}
	for in, want := range cases {
		got, err := Canonical(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got != want {
			t.Errorf("Canonical(%q)=%q, want %q", in, got, want)
		}
	}
	if _, err := Canonical("not a url"); err == nil {
		t.Fatal("expected error for invalid url")
	}
}

func TestHostAllowed(t *testing.T) {
	if !HostAllowed("blog.example.com", []string{"example.com"}) {
		t.Fatal("subdomain should match")
	}
	if !HostAllowed("www.example.com", []string{"example.com"}) {
		t.Fatal("www should match")
	}
	if HostAllowed("notexample.com", []string{"example.com"}) {
		t.Fatal("suffix confusion must not match")
	}
	if !HostAllowed("anything.dev", nil) {
		t.Fatal("empty list allows all")
	}
}

func TestFilterIncludeExclude(t *testing.T) {
	results := []Result{
		{URL: "https://go.dev/blog/1"},
		{URL: "https://example.com/a"},
		{URL: "https://sub.example.com/b"},
		{URL: "https://evil.com/c"},
	}
	got := Filter(results, []string{"example.com"}, []string{"sub.example.com"})
	if len(got) != 1 || got[0].URL != "https://example.com/a" {
		t.Fatalf("unexpected filter result: %+v", got)
	}
}

func TestBuildQuery(t *testing.T) {
	q := BuildQuery("golang  memory  model", []string{"Go.dev", "go.dev", "https://blog.example.com"})
	if q != "golang  memory  model site:go.dev site:blog.example.com" {
		t.Fatalf("unexpected query: %q", q)
	}
	if BuildQuery("x", nil) != "x" {
		t.Fatal("no domains should not modify the query")
	}
}

func TestDedupeKeepsBestSnippet(t *testing.T) {
	results := []Result{
		{URL: "https://example.com/a?utm_source=news", Title: "A", Content: "short"},
		{URL: "https://www.example.com/a/", Title: "", Content: "a much longer snippet"},
		{URL: "https://other.com/b", Title: "B", Content: "b"},
	}
	got := Dedupe(results)
	if len(got) != 2 {
		t.Fatalf("expected 2 results, got %d: %+v", len(got), got)
	}
	if got[0].Content != "a much longer snippet" {
		t.Fatalf("expected longest snippet to win, got %q", got[0].Content)
	}
	if got[0].Title != "A" {
		t.Fatalf("expected existing title kept, got %q", got[0].Title)
	}
}

func TestResolveReference(t *testing.T) {
	base, _ := url.Parse("https://example.com/a/b")
	ref, _ := url.Parse("../c")
	if got := base.ResolveReference(ref).String(); got != "https://example.com/c" {
		t.Fatalf("got %s", got)
	}
}
