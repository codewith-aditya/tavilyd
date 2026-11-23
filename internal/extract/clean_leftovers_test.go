package extract

import (
	"regexp"
	"strings"
	"testing"
)

func TestEmptyAndSymbolLinksRemoved(t *testing.T) {
	md := strings.Join([]string{
		"## Types of probe [#](#types-of-probe)",
		"",
		"Text with an empty [](#anchor) link and a pilcrow [¶](#cite-1) marker.",
		"",
		"See [the docs](https://example.com/docs) for details.",
	}, "\n")
	out, rep := testCleaner().CleanMarkdown(md, false)
	if strings.Contains(out, "[](#") || strings.Contains(out, "[¶]") || strings.Contains(out, "[#](#") {
		t.Fatalf("symbol/empty links survived:\n%s", out)
	}
	if !strings.Contains(out, "## Types of probe") {
		t.Fatalf("heading text lost: %s", out)
	}
	if !strings.Contains(out, "the docs") {
		t.Fatalf("normal link removed: %s", out)
	}
	if rep["md_empty_link"] == 0 {
		t.Fatal("md_empty_link not counted")
	}
}

func TestCitationMarkersAndRefLinesRemoved(t *testing.T) {
	md := strings.Join([]string{
		"Kubernetes schedules pods onto nodes.[\\[12\\]](#cite_note-12) It also restarts them.[13]",
		"",
		"1. [↑](#cite_ref-1) Smith, J. (2020). Kubernetes in Action.",
		"2. [↑](#cite_ref-2) Doe, J. (2021). Containers.",
		"",
		"Real prose continues here.",
	}, "\n")
	out, rep := testCleaner().CleanMarkdown(md, false)
	if strings.Contains(out, "cite_note") || strings.Contains(out, "[12]") || strings.Contains(out, "[13]") {
		t.Fatalf("citation markers survived:\n%s", out)
	}
	if strings.Contains(out, "Smith, J.") || strings.Contains(out, "Doe, J.") {
		t.Fatalf("numbered reference lines survived:\n%s", out)
	}
	if !strings.Contains(out, "Real prose") || !strings.Contains(out, "Kubernetes schedules") {
		t.Fatalf("prose removed:\n%s", out)
	}
	if rep["md_ref_line"] == 0 || rep["md_citation"] == 0 {
		t.Fatalf("counters missing: %v", rep)
	}
}

func TestReferenceSectionDropped(t *testing.T) {
	md := strings.Join([]string{
		"## Body",
		"",
		"Probes decide readiness.",
		"",
		"## References",
		"",
		"1. [↑](#cite_ref-1) Some source",
		"",
		"## External links",
		"",
		"- [Kubernetes docs](https://kubernetes.io)",
		"",
		"## After",
		"",
		"Content after the references must survive.",
	}, "\n")
	out, rep := testCleaner().CleanMarkdown(md, false)
	if strings.Contains(out, "Some source") || strings.Contains(out, "External links") || strings.Contains(out, "Kubernetes docs") {
		t.Fatalf("reference section survived:\n%s", out)
	}
	if !strings.Contains(out, "Content after the references must survive.") {
		t.Fatalf("content after refs removed:\n%s", out)
	}
	if rep["md_ref_section"] == 0 {
		t.Fatal("md_ref_section not counted")
	}
}

func TestDanglingColonRemoved(t *testing.T) {
	md := strings.Join([]string{
		"Consider these options:",
		"",
		"Install in 5 mins.",
		"",
		"## Next",
		"",
		"Real content.",
	}, "\n")
	out, rep := testCleaner().CleanMarkdown(md, false)
	if strings.Contains(out, "Consider these options:") {
		t.Fatalf("dangling colon line survived:\n%s", out)
	}
	if !strings.Contains(out, "Real content.") {
		t.Fatalf("prose removed:\n%s", out)
	}
	if rep["md_dangling_colon"] == 0 {
		t.Fatal("md_dangling_colon not counted")
	}
}

func TestWikipediaLeftoversGone(t *testing.T) {
	art := parseFixture(t, "wikipedia.html", "https://en.wikipedia.org/wiki/Kubernetes", testCleaner(), false)
	md := art.Markdown
	if strings.Contains(md, "cite_note") || strings.Contains(md, "cite_ref") {
		t.Fatal("wikipedia citation anchors survived")
	}
	if regexp.MustCompile(`(?im)^#{1,6}\s*references\b`).MatchString(md) {
		t.Fatalf("references section survived")
	}
	for _, line := range strings.Split(md, "\n") {
		if len(line) > 3 && line[0] >= '0' && line[0] <= '9' && strings.Contains(line, "[") && strings.Contains(line, "↑") {
			t.Fatalf("numbered reference line survived: %q", line)
		}
	}
	// Idempotency on the real page.
	again, _ := testCleaner().CleanMarkdown(md, false)
	if again != md {
		t.Fatal("wikipedia cleanup is not idempotent")
	}
}
