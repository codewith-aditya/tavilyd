// Package rerank scores extracted chunks against the query. Primary scoring is
// a GPU cross-encoder (TEI /rerank); a BM25 implementation is the documented
// lexical fallback when the reranker is unavailable.
package rerank

import (
	"math"
	"regexp"
	"strings"
	"unicode/utf8"
)

// ChunkerConfig mirrors the env-tunable chunking knobs.
type ChunkerConfig struct {
	Tokens        int
	Overlap       int
	MaxPerPage    int
	CharsPerToken float64
	Margin        float64
}

// Chunk is a retrieval unit with its estimated token count and the byte range
// it covers in the source Markdown (Start inclusive, End exclusive). Offsets let
// callers assemble snippets from the document instead of concatenating chunk
// text, which is what caused duplicated paragraphs.
type Chunk struct {
	Text   string
	Index  int
	Tokens int
	Start  int
	End    int
}

// block is a Markdown block with its byte offsets in the source.
type block struct {
	text  string
	start int
	end   int
}

// Chunker splits Markdown into overlapping, heading-aware chunks sized in
// estimated tokens.
type Chunker struct {
	cfg ChunkerConfig
}

// NewChunker validates and builds a chunker.
func NewChunker(cfg ChunkerConfig) *Chunker {
	if cfg.Tokens <= 0 {
		cfg.Tokens = 350
	}
	if cfg.Overlap < 0 || cfg.Overlap >= cfg.Tokens {
		cfg.Overlap = Min(50, cfg.Tokens/4)
	}
	if cfg.MaxPerPage <= 0 {
		cfg.MaxPerPage = 24
	}
	if cfg.CharsPerToken <= 0 {
		cfg.CharsPerToken = 4
	}
	if cfg.Margin < 1 {
		cfg.Margin = 1.15
	}
	return &Chunker{cfg: cfg}
}

var (
	reSentenceEnd = regexp.MustCompile(`(?m)([.!?])\s+`)
)

// EstimateTokens approximates the tokenizer count. We deliberately over-estimate
// (safety margin) because BPE tokens per character vary by language and code.
// The approximation is documented in the README; there is no cgo tokenizer.
func (c *Chunker) EstimateTokens(s string) int {
	runes := utf8.RuneCountInString(s)
	if runes == 0 {
		return 0
	}
	est := float64(runes) / c.cfg.CharsPerToken * c.cfg.Margin
	if est < 1 {
		return 1
	}
	return int(math.Ceil(est))
}

// Split produces chunks of roughly cfg.Tokens estimated tokens with a
// cfg.Overlap token tail carried into the next chunk.
func (c *Chunker) Split(markdown string) []Chunk {
	text := strings.ReplaceAll(markdown, "\r\n", "\n")
	blocks := c.blocks(text)
	if len(blocks) == 0 {
		return nil
	}

	var (
		chunks []Chunk
		cur    []block
		curTok int
	)
	flush := func() bool {
		if len(cur) == 0 {
			return true
		}
		body := joinBlockText(cur)
		if body != "" {
			chunks = append(chunks, Chunk{
				Text: body, Index: len(chunks), Tokens: c.EstimateTokens(body),
				Start: cur[0].start, End: cur[len(cur)-1].end,
			})
		}
		if len(chunks) >= c.cfg.MaxPerPage {
			cur, curTok = nil, 0
			return false
		}
		// Carry an overlap tail of whole blocks, or a trailing slice of the last
		// block when it alone exceeds the overlap budget (long paragraphs).
		tail := c.tailBlocks(cur, c.cfg.Overlap)
		cur = tail
		curTok = 0
		for _, b := range tail {
			curTok += c.EstimateTokens(b.text)
		}
		return true
	}

	for _, b := range blocks {
		bt := c.EstimateTokens(b.text)
		if bt > c.cfg.Tokens {
			// Oversized single block: hard-split into token-sized pieces.
			if !flush() {
				return chunks
			}
			for _, piece := range c.splitLargeBlock(b) {
				if len(chunks) >= c.cfg.MaxPerPage {
					return chunks
				}
				if piece.text == "" {
					continue
				}
				chunks = append(chunks, Chunk{
					Text: piece.text, Index: len(chunks), Tokens: c.EstimateTokens(piece.text),
					Start: piece.start, End: piece.end,
				})
			}
			continue
		}
		if curTok+bt > c.cfg.Tokens && len(cur) > 0 {
			if !flush() {
				return chunks
			}
		}
		cur = append(cur, b)
		curTok += bt
	}
	if body := joinBlockText(cur); body != "" && len(chunks) < c.cfg.MaxPerPage {
		chunks = append(chunks, Chunk{
			Text: body, Index: len(chunks), Tokens: c.EstimateTokens(body),
			Start: cur[0].start, End: cur[len(cur)-1].end,
		})
	}
	return chunks
}

func joinBlockText(blocks []block) string {
	parts := make([]string, len(blocks))
	for i, b := range blocks {
		parts[i] = b.text
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

// blocks splits on blank lines, keeping fenced code blocks intact and headings
// attached to the paragraph that follows them, while tracking byte offsets.
func (c *Chunker) blocks(text string) []block {
	type lineRef struct {
		text  string
		start int
		end   int
	}
	lines := make([]lineRef, 0, 128)
	off := 0
	for _, line := range strings.Split(text, "\n") {
		lines = append(lines, lineRef{text: line, start: off, end: off + len(line)})
		off += len(line) + 1
	}

	raw := make([]block, 0, len(lines)/2)
	cur := make([]lineRef, 0, 8)
	inFence := false
	flush := func() {
		if len(cur) == 0 {
			return
		}
		start, end := cur[0].start, cur[len(cur)-1].end
		if start < 0 || end > len(text) || start >= end {
			cur = cur[:0]
			return
		}
		slice := text[start:end]
		trimmed := strings.TrimSpace(slice)
		if trimmed != "" {
			delta := strings.Index(slice, trimmed)
			if delta < 0 {
				delta = 0
			}
			raw = append(raw, block{text: trimmed, start: start + delta, end: start + delta + len(trimmed)})
		}
		cur = cur[:0]
	}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line.text)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
		}
		if trimmed == "" && !inFence {
			flush()
			continue
		}
		cur = append(cur, line)
	}
	flush()

	out := make([]block, 0, len(raw))
	var pending *block
	for i := range raw {
		b := raw[i]
		if isHeading(b.text) {
			if pending != nil {
				out = append(out, *pending)
			}
			pending = &b
			continue
		}
		if pending != nil {
			b.text = pending.text + "\n\n" + b.text
			b.start = pending.start
			pending = nil
		}
		out = append(out, b)
	}
	if pending != nil {
		out = append(out, *pending)
	}
	return out
}

func isHeading(block string) bool {
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		return strings.HasPrefix(line, "#")
	}
	return false
}

// splitLargeBlock breaks an oversized block on sentence boundaries and maps each
// piece back to its byte offset inside the block.
func (c *Chunker) splitLargeBlock(b block) []block {
	pieces := c.splitLarge(b.text)
	out := make([]block, 0, len(pieces))
	pos := 0
	for _, piece := range pieces {
		idx := strings.Index(b.text[pos:], piece)
		if idx < 0 {
			idx = 0
		}
		start := b.start + pos + idx
		out = append(out, block{text: piece, start: start, end: start + len(piece)})
		pos += idx + len(piece)
	}
	return out
}

// splitLarge breaks an oversized block on sentence boundaries first, then on a
// hard character budget as a last resort.
func (c *Chunker) splitLarge(block string) []string {
	budget := int(float64(c.cfg.Tokens) * c.cfg.CharsPerToken / c.cfg.Margin)
	if budget < 200 {
		budget = 200
	}
	sentences := reSentenceEnd.Split(block, -1)
	var (
		out []string
		cur strings.Builder
	)
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			out = append(out, s)
		}
		cur.Reset()
	}
	for _, s := range sentences {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if utf8.RuneCountInString(s) > budget {
			flush()
			for len(s) > 0 {
				runes := []rune(s)
				n := budget
				if n > len(runes) {
					n = len(runes)
				}
				out = append(out, string(runes[:n]))
				s = string(runes[n:])
			}
			continue
		}
		if utf8.RuneCountInString(cur.String())+utf8.RuneCountInString(s)+1 > budget {
			flush()
		}
		if cur.Len() > 0 {
			cur.WriteString(" ")
		}
		cur.WriteString(s)
	}
	flush()
	return out
}

func (c *Chunker) tailBlocks(blocks []block, overlapTokens int) []block {
	if overlapTokens <= 0 || len(blocks) == 0 {
		return nil
	}
	total := 0
	start := len(blocks)
	for i := len(blocks) - 1; i >= 0; i-- {
		t := c.EstimateTokens(blocks[i].text)
		if total+t > overlapTokens {
			break
		}
		total += t
		start = i
	}
	if start < len(blocks) {
		out := make([]block, len(blocks)-start)
		copy(out, blocks[start:])
		return out
	}
	// Nothing fit whole: carry the last sentences of the last block.
	last := blocks[len(blocks)-1]
	budgetChars := int(float64(overlapTokens) * c.cfg.CharsPerToken / c.cfg.Margin)
	if budgetChars < 40 {
		budgetChars = 40
	}
	runes := []rune(last.text)
	if len(runes) <= budgetChars {
		return []block{last}
	}
	tailRunes := runes[len(runes)-budgetChars:]
	tail := string(tailRunes)
	if idx := strings.IndexAny(tail, ".!?\n"); idx >= 0 && idx+1 < len(tail) {
		tail = tail[idx+1:]
	}
	tail = strings.TrimSpace(tail)
	if tail == "" {
		return nil
	}
	startOff := last.end - len(tail)
	if startOff < last.start {
		startOff = last.start
	}
	return []block{{text: tail, start: startOff, end: last.end}}
}

// Min returns the smaller of two ints.
func Min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
