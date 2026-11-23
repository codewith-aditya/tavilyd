package cache

import (
	"strings"
	"testing"
)

func TestSearchKeyNormalizationEquivalence(t *testing.T) {
	a := SearchKeyInput{
		Query: "  How   Does   GO  Work ", SearchDepth: "", MaxResults: 0, Topic: "",
		IncludeDomains: []string{"b.com", "a.com", "A.com", " "},
	}
	b := SearchKeyInput{
		Query: "how does go work", SearchDepth: "basic", MaxResults: 5, Topic: "general",
		IncludeDomains: []string{"a.com", "b.com"},
	}
	if a.Key() != b.Key() {
		t.Fatalf("equivalent requests produced different keys:\n%s\n%s", a.Key(), b.Key())
	}
}

func TestSearchKeyIsSensitiveToResultAffectingFields(t *testing.T) {
	base := SearchKeyInput{Query: "go", SearchDepth: "basic", MaxResults: 5, Topic: "general"}
	variants := []SearchKeyInput{
		{Query: "go", SearchDepth: "advanced", MaxResults: 5, Topic: "general"},
		{Query: "go", SearchDepth: "basic", MaxResults: 6, Topic: "general"},
		{Query: "go", SearchDepth: "basic", MaxResults: 5, Topic: "news"},
		{Query: "go", SearchDepth: "basic", MaxResults: 5, Topic: "general", TimeRange: "week"},
		{Query: "go", SearchDepth: "basic", MaxResults: 5, Topic: "general", IncludeDomains: []string{"go.dev"}},
		{Query: "go", SearchDepth: "basic", MaxResults: 5, Topic: "general", ExcludeDomains: []string{"go.dev"}},
		{Query: "go", SearchDepth: "basic", MaxResults: 5, Topic: "general", IncludeRawContent: true},
	}
	seen := map[string]string{base.Key(): "base"}
	for i, v := range variants {
		k := v.Key()
		if other, dup := seen[k]; dup {
			t.Errorf("variant %d collided with %s", i, other)
		}
		seen[k] = "variant"
	}
}

func TestURLKeyIsCaseInsensitive(t *testing.T) {
	if URLKey("HTTPS://Example.com/A", "basic", "noimg", "2") != URLKey("https://example.com/a", "basic", "noimg", "2") {
		t.Fatal("URL keys should be case-insensitive")
	}
	if URLKey("https://example.com/a", "basic", "noimg", "2") == URLKey("https://example.com/a", "advanced", "noimg", "2") {
		t.Fatal("depth must affect the page cache key")
	}
	if !strings.HasPrefix(URLKey("https://example.com", "basic", "noimg", "2"), "page:") {
		t.Fatal("unexpected key prefix")
	}
	if NegativeKey("https://example.com", "2") == URLKey("https://example.com", "basic", "noimg", "2") {
		t.Fatal("negative and positive keys must differ")
	}
}

func TestLRUEvictionAndTTL(t *testing.T) {
	c := newLRU(2)
	c.set("a", []byte("1"), 50*1e9)
	c.set("b", []byte("2"), 50*1e9)
	if _, ok := c.get("a"); !ok {
		t.Fatal("a missing")
	}
	c.set("c", []byte("3"), 50*1e9)
	if _, ok := c.get("b"); ok {
		t.Fatal("b should be evicted (LRU)")
	}
	if _, ok := c.get("a"); !ok {
		t.Fatal("a should survive")
	}
	if c.len() != 2 {
		t.Fatalf("len=%d", c.len())
	}
}

func TestLRUCopySemantics(t *testing.T) {
	c := newLRU(4)
	orig := []byte("value")
	c.set("k", orig, 1e9)
	orig[0] = 'X'
	got, ok := c.get("k")
	if !ok || string(got) != "value" {
		t.Fatalf("cache must copy on write, got %q", got)
	}
	got[0] = 'Y'
	again, _ := c.get("k")
	if string(again) != "value" {
		t.Fatal("cache must copy on read")
	}
}
