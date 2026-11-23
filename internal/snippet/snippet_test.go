package snippet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/astralabs/tavilyd/internal/extract"
	"github.com/astralabs/tavilyd/internal/rerank"
)

func loadMarkdown(t *testing.T, fixture, pageURL string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "extract", "testdata", fixture))
	if err != nil {
		t.Skipf("fixture %s: %v", fixture, err)
	}
	art, err := extract.ParseHTML(body, pageURL, "text/html", extract.ParseOptions{
		Format: "markdown", IncludeImages: false, Cleaner: extract.NewCleaner(extract.CleanConfig{
			Enabled: true, MaxTokenChars: 2000, LinkDensity: 0.5, MinAlpha: 0.4,
		}),
	})
	if err != nil {
		t.Fatalf("parse %s: %v", fixture, err)
	}
	return art.Markdown
}

func chunksOf(md string) []rerank.Chunk {
	return rerank.NewChunker(rerank.ChunkerConfig{Tokens: 350, Overlap: 50, MaxPerPage: 24}).Split(md)
}

func TestAssembleNoDuplicateParagraphs(t *testing.T) {
	cases := []struct{ fixture, url string }{
		{"spacelift.html", "https://spacelift.io/blog/kubernetes-readiness-probe"},
		{"k8s-control.html", "https://kubernetes.io/docs/concepts/workloads/pods/probes"},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			md := loadMarkdown(t, tc.fixture, tc.url)
			chunks := chunksOf(md)
			if len(chunks) < 2 {
				t.Skip("not enough chunks")
			}
			spans := make([]Span, 0, 3)
			for i := 0; i < len(chunks) && i < 3; i++ {
				spans = append(spans, Span{Start: chunks[i].Start, End: chunks[i].End})
			}
			out := Assemble(md, spans, 3000)
			if out == "" {
				t.Fatal("empty snippet")
			}
			seen := map[string]int{}
			for _, para := range strings.Split(out, "\n\n") {
				p := strings.TrimSpace(para)
				if len(p) < 80 || p == "[...]" {
					continue
				}
				seen[p]++
				if seen[p] > 1 {
					t.Fatalf("duplicated paragraph in snippet: %q", p[:80])
				}
			}
			if strings.Count(out, "```")%2 != 0 {
				t.Fatalf("unbalanced code fences:\n%s", out)
			}
			if len(out) > 3000 {
				t.Fatalf("snippet exceeds cap: %d", len(out))
			}
			// The snippet must start at a word boundary in the source.
			idx := strings.Index(md, firstLine(out))
			if idx > 0 {
				prev := md[idx-1]
				if prev != ' ' && prev != '\n' && prev != '\t' {
					t.Fatalf("snippet starts mid-word (prev byte %q)", prev)
				}
			}
		})
	}
}

func firstLine(s string) string {
	line := s
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		line = s[:i]
	}
	if len(line) > 60 {
		line = line[:60]
	}
	return line
}

func TestAssembleJoinsNonAdjacentSpans(t *testing.T) {
	src := "First paragraph about probes.\n\nSecond unrelated paragraph.\n\nThird paragraph about kubelet checks."
	a := Span{Start: 0, End: len("First paragraph about probes.")}
	b := Span{Start: strings.Index(src, "Third"), End: len(src)}
	out := Assemble(src, []Span{a, b}, 1000)
	if !strings.Contains(out, "[...]") {
		t.Fatalf("expected joiner marker: %q", out)
	}
	if strings.Count(out, "paragraph about") != 2 {
		t.Fatalf("unexpected content: %q", out)
	}
}
