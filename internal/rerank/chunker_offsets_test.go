package rerank

import (
	"strings"
	"testing"
)

func TestChunkOffsetsCoverSource(t *testing.T) {
	md := strings.Join([]string{
		"## Alpha",
		"",
		"First paragraph about readiness probes and how the kubelet uses them.",
		"",
		"## Beta",
		"",
		"Second paragraph about liveness probes, restarts and backoff behaviour.",
		"",
		"```go",
		"func main() {",
		"",
		"fmt.Println(\"blank line inside fence\")",
		"}",
		"```",
	}, "\n")
	c := NewChunker(ChunkerConfig{Tokens: 80, Overlap: 10, MaxPerPage: 20})
	chunks := c.Split(md)
	if len(chunks) == 0 {
		t.Fatal("no chunks")
	}
	prevEnd := -1
	for i, ch := range chunks {
		if ch.Start < 0 || ch.End > len(md) || ch.Start >= ch.End {
			t.Fatalf("chunk %d has invalid offsets %d..%d (len %d)", i, ch.Start, ch.End, len(md))
		}
		if ch.Start < prevEnd {
			t.Fatalf("chunk %d starts before previous end (%d < %d)", i, ch.Start, prevEnd)
		}
		prevEnd = ch.End
		slice := md[ch.Start:ch.End]
		if !strings.Contains(slice, firstChunkWord(ch.Text)) {
			t.Fatalf("chunk %d offsets do not cover its text: %q vs %q", i, slice, ch.Text)
		}
	}
}

func firstChunkWord(text string) string {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

func TestFencedCodeStaysWhole(t *testing.T) {
	md := "Intro paragraph.\n\n```go\nline one\n\nline two\n```\n\nOutro paragraph."
	c := NewChunker(ChunkerConfig{Tokens: 30, Overlap: 5, MaxPerPage: 10})
	for _, ch := range c.Split(md) {
		if strings.Count(ch.Text, "```") == 1 {
			t.Fatalf("chunk split a code fence: %q", ch.Text)
		}
	}
}
