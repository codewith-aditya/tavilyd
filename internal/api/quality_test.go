package api

import (
	"strings"
	"testing"
)

func TestApplyScoreFloor(t *testing.T) {
	results := []SearchResult{
		{URL: "a", Score: 0.99},
		{URL: "b", Score: 0.70},
		{URL: "c", Score: 0.02},
		{URL: "d", Score: 0.02},
	}
	got := applyScoreFloor(results, 0.10, 0.05, 0.02)
	if len(got) != 2 {
		t.Fatalf("expected 2 results, got %d: %+v", len(got), got)
	}
	if got[0].URL != "a" || got[1].URL != "b" {
		t.Fatalf("unexpected results: %+v", got)
	}
}

func TestApplyScoreFloorKeepsTopWhenAllLow(t *testing.T) {
	results := []SearchResult{
		{URL: "a", Score: 0.09},
		{URL: "b", Score: 0.02},
	}
	got := applyScoreFloor(results, 0.10, 0.05, 0.02)
	if len(got) != 1 || got[0].URL != "a" {
		t.Fatalf("expected only the top result, got %+v", got)
	}
}

func TestApplyScoreFloorEmpty(t *testing.T) {
	if got := applyScoreFloor(nil, 0.1, 0.05, 0.02); len(got) != 0 {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestMergeResultsKeepsHigherScore(t *testing.T) {
	a := []SearchResult{{URL: "a", Score: 0.9}, {URL: "b", Score: 0.4}}
	b := []SearchResult{{URL: "b", Score: 0.6}, {URL: "c", Score: 0.5}}
	got := mergeResults(a, b)
	if len(got) != 3 {
		t.Fatalf("expected 3, got %+v", got)
	}
	if got[0].URL != "a" || got[1].URL != "b" || got[1].Score != 0.6 {
		t.Fatalf("unexpected merge: %+v", got)
	}
}

func TestTruncateAtParagraph(t *testing.T) {
	long := strings.Repeat("This is a paragraph with enough words to matter. ", 40)
	got := truncateAtParagraph(long, 500)
	if len(got) > 520 {
		t.Fatalf("not truncated: %d", len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("missing our truncation marker: %q", got[len(got)-20:])
	}
	if strings.Contains(got, "This is a paragraph with enough words to matter. This is a paragraph with enough words to matter. ...") {
		t.Fatal("truncation marker collided with page text")
	}
	small := "short"
	if truncateAtParagraph(small, 500) != small {
		t.Fatal("short content changed")
	}
}
