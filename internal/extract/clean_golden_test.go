package extract

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func readFixture(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join("testdata", name))
}

func parseFixture(t *testing.T, name, pageURL string, cleaner *Cleaner, images bool) *Article {
	t.Helper()
	body, err := readFixture(name)
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	art, err := ParseHTML(body, pageURL, "text/html; charset=utf-8", ParseOptions{
		Format: "markdown", IncludeImages: images, Cleaner: cleaner,
	})
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return art
}

func TestGoldenNoDataURIsOrPromo(t *testing.T) {
	c := testCleaner()
	cases := []struct {
		fixture string
		url     string
		junk    []string
	}{
		{"kodekloud.html", "https://kodekloud.com/blog/kubernetes-readiness-probe", []string{"install in 5 mins", "check out our", "want to get started with kubernetes?"}},
		{"apptio.html", "https://apptio.com/blog/kubernetes-readiness-probe", []string{"install in 5 mins", "data:image"}},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			art := parseFixture(t, tc.fixture, tc.url, c, false)
			md := strings.ToLower(art.Markdown)
			if strings.Contains(md, "data:image") || strings.Contains(md, "](data:") {
				t.Fatal("data URI survived")
			}
			if strings.Contains(art.Markdown, "![") {
				t.Fatal("image syntax survived with images off")
			}
			for _, junk := range tc.junk {
				if strings.Contains(md, junk) {
					t.Fatalf("junk %q survived:\n%s", junk, firstLines(art.Markdown, 40))
				}
			}
			if art.Length < 500 {
				t.Fatalf("content shrank too far: %d bytes", art.Length)
			}
			t.Logf("%s: cleaned markdown %d bytes, removed %d bytes across %d rules",
				tc.fixture, art.Length, totalRemoved(art.Clean), len(art.Clean))
		})
	}
}

func TestGoldenShrinksMaterially(t *testing.T) {
	c := testCleaner()
	for _, tc := range []struct{ fixture, url string }{
		{"kodekloud.html", "https://kodekloud.com/blog/kubernetes-readiness-probe"},
		{"apptio.html", "https://apptio.com/blog/kubernetes-readiness-probe"},
	} {
		raw := parseFixture(t, tc.fixture, tc.url, nil, false)
		clean := parseFixture(t, tc.fixture, tc.url, c, false)
		if clean.Length >= raw.Length {
			t.Fatalf("%s: cleanup did not shrink markdown (%d -> %d)", tc.fixture, raw.Length, clean.Length)
		}
		t.Logf("%s: %d -> %d bytes (%.0f%% removed)", tc.fixture, raw.Length, clean.Length,
			100*(1-float64(clean.Length)/float64(raw.Length)))
	}
}

// Code blocks and headings from the control page must survive byte-identically.
func TestGoldenControlNotOverStripped(t *testing.T) {
	c := testCleaner()
	before := parseFixture(t, "k8s-control.html", "https://kubernetes.io/docs/concepts/workloads/pods/probes", nil, false)
	after := parseFixture(t, "k8s-control.html", "https://kubernetes.io/docs/concepts/workloads/pods/probes", c, false)

	beforeBlocks := headingsAndFences(before.Markdown)
	afterBlocks := headingsAndFences(after.Markdown)
	if len(beforeBlocks) == 0 {
		t.Fatal("control page produced no headings or code blocks")
	}
	if len(afterBlocks) < len(beforeBlocks) {
		t.Fatalf("cleanup removed %d headings/code blocks", len(beforeBlocks)-len(afterBlocks))
	}
	for i := range beforeBlocks {
		if i >= len(afterBlocks) || beforeBlocks[i] != afterBlocks[i] {
			t.Fatalf("heading/code block %d changed:\nbefore: %q\nafter:  %q", i, beforeBlocks[i], safeIndex(afterBlocks, i))
		}
	}
}

var (
	reFenceBlock = regexp.MustCompile("(?s)```.*?```")
	reHeadingLn  = regexp.MustCompile(`(?m)^#{1,6} .*$`)
)

var reEmptyAnchor = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)

func normalizeAnchors(line string) string {
	return reEmptyAnchor.ReplaceAllStringFunc(line, func(m string) string {
		sub := reEmptyAnchor.FindStringSubmatch(m)
		if len(sub) == 2 {
			for _, r := range sub[1] {
				if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
					return m
				}
			}
		}
		return ""
	})
}

func headingsAndFences(md string) []string {
	var out []string
	for _, line := range reHeadingLn.FindAllString(md, -1) {
		out = append(out, normalizeAnchors(strings.TrimSpace(line)))
	}
	for _, block := range reFenceBlock.FindAllString(md, -1) {
		out = append(out, strings.TrimSpace(block))
	}
	return out
}

func safeIndex(list []string, i int) string {
	if i < len(list) {
		return list[i]
	}
	return "<missing>"
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
