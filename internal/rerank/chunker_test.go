package rerank

import (
	"fmt"
	"strings"
	"testing"
)

func makeMarkdown(paragraphs int) string {
	var b strings.Builder
	for i := 0; i < paragraphs; i++ {
		fmt.Fprintf(&b, "## Section %d\n\n", i)
		fmt.Fprintf(&b, "This is paragraph %d about distributed systems, caching layers and search relevance. "+
			"It contains enough words to resemble a real documentation page so the chunker has work to do. "+
			"Every sentence carries a little more signal than the previous one.\n\n", i)
	}
	return b.String()
}

func TestChunkerSplitsWithOverlap(t *testing.T) {
	c := NewChunker(ChunkerConfig{Tokens: 120, Overlap: 25, MaxPerPage: 40, CharsPerToken: 4, Margin: 1.15})
	chunks := c.Split(makeMarkdown(20))
	if len(chunks) < 3 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	for i, ch := range chunks {
		if strings.TrimSpace(ch.Text) == "" {
			t.Fatalf("chunk %d is empty", i)
		}
		if ch.Tokens <= 0 {
			t.Fatalf("chunk %d has no token estimate", i)
		}
		if ch.Tokens > 200 { // tokens + overlap + margin
			t.Fatalf("chunk %d too large: %d tokens", i, ch.Tokens)
		}
		if ch.Index != i {
			t.Fatalf("chunk index %d, want %d", ch.Index, i)
		}
	}
	first := strings.TrimSpace(chunks[0].Text)
	tail := first
	if len(tail) > 60 {
		tail = tail[len(tail)-60:]
	}
	tail = strings.TrimSpace(tail)
	if tail != "" && !strings.Contains(chunks[1].Text, tail[:min(30, len(tail))]) {
		t.Fatalf("expected overlap tail %q in second chunk", tail[:min(30, len(tail))])
	}
}

func TestChunkerKeepsHeadingWithParagraph(t *testing.T) {
	c := NewChunker(ChunkerConfig{Tokens: 350, Overlap: 50, MaxPerPage: 5})
	chunks := c.Split("# The Title\n\nA short paragraph that belongs with the heading.")
	if len(chunks) != 1 {
		t.Fatalf("expected a single chunk, got %d", len(chunks))
	}
	if !strings.HasPrefix(chunks[0].Text, "# The Title") {
		t.Fatalf("heading detached: %q", chunks[0].Text)
	}
}

func TestChunkerCapsChunksPerPage(t *testing.T) {
	c := NewChunker(ChunkerConfig{Tokens: 60, Overlap: 10, MaxPerPage: 3})
	chunks := c.Split(makeMarkdown(50))
	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks (cap), got %d", len(chunks))
	}
}

func TestChunkerSplitsOversizedBlock(t *testing.T) {
	big := strings.Repeat("Sentence about retrieval augmented generation and reranking. ", 400)
	c := NewChunker(ChunkerConfig{Tokens: 100, Overlap: 10, MaxPerPage: 50})
	chunks := c.Split(big)
	if len(chunks) < 2 {
		t.Fatalf("expected the block to be split, got %d chunk(s)", len(chunks))
	}
	for i, ch := range chunks {
		if ch.Tokens > 200 {
			t.Fatalf("chunk %d still too large: %d", i, ch.Tokens)
		}
		if strings.TrimSpace(ch.Text) == "" {
			t.Fatalf("empty chunk %d", i)
		}
	}
}

func TestChunkerEmptyInput(t *testing.T) {
	c := NewChunker(ChunkerConfig{})
	if got := c.Split(""); len(got) != 0 {
		t.Fatalf("expected no chunks, got %d", len(got))
	}
	if got := c.Split("   \n\n  \n"); len(got) != 0 {
		t.Fatalf("expected no chunks for whitespace, got %d", len(got))
	}
}

func TestEstimateTokensSafetyMargin(t *testing.T) {
	c := NewChunker(ChunkerConfig{Tokens: 350, CharsPerToken: 4, Margin: 1.15})
	// 400 ASCII chars -> 100 raw token estimate -> 115 with margin.
	if got := c.EstimateTokens(strings.Repeat("a", 400)); got != 115 {
		t.Fatalf("estimate = %d, want 115", got)
	}
}

func BenchmarkChunkerSplit(b *testing.B) {
	c := NewChunker(ChunkerConfig{Tokens: 350, Overlap: 50, MaxPerPage: 24})
	doc := makeMarkdown(200)
	b.ReportAllocs()
	b.SetBytes(int64(len(doc)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if len(c.Split(doc)) == 0 {
			b.Fatal("no chunks")
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
