// Package snippet assembles result snippets from source spans. Working from
// byte offsets (instead of concatenating chunk text) avoids duplicated
// paragraphs at chunk overlaps and lets every cut land on a word boundary.
package snippet

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Span is a half-open byte range in the source Markdown.
type Span struct {
	Start int
	End   int
}

const (
	lookback = 240
	lookahead = 240
	joiner   = "\n\n[...]\n\n"
)

// Assemble merges overlapping/adjacent spans, expands them to sentence
// boundaries (never inside fenced code), joins non-adjacent spans with a marker
// and caps the result at maxChars (cut on a paragraph boundary when possible).
func Assemble(source string, spans []Span, maxChars int) string {
	if source == "" || len(spans) == 0 {
		return ""
	}
	if maxChars <= 0 {
		maxChars = 1500
	}
	clean := make([]Span, 0, len(spans))
	for _, s := range spans {
		if s.Start < 0 {
			s.Start = 0
		}
		if s.End > len(source) {
			s.End = len(source)
		}
		if s.Start >= s.End {
			continue
		}
		clean = append(clean, s)
	}
	if len(clean) == 0 {
		return ""
	}
	sort.Slice(clean, func(i, j int) bool { return clean[i].Start < clean[j].Start })

	merged := []Span{clean[0]}
	for _, s := range clean[1:] {
		last := &merged[len(merged)-1]
		if s.Start <= last.End+2 {
			if s.End > last.End {
				last.End = s.End
			}
			continue
		}
		merged = append(merged, s)
	}

	fences := fenceRanges(source)
	parts := make([]string, 0, len(merged))
	for _, s := range merged {
		start, end := expand(source, s.Start, s.End)
		start, end = snapToFences(fences, start, end)
		if start >= end {
			continue
		}
		parts = append(parts, strings.TrimSpace(source[start:end]))
	}
	out := strings.Join(parts, joiner)
	return truncate(out, maxChars)
}

func expand(source string, start, end int) (int, int) {
	if start > 0 {
		lo := start - lookback
		if lo < 0 {
			lo = 0
		}
		window := source[lo:start]
		cut := closestBoundaryBackward(window)
		if cut >= 0 {
			start = lo + cut
		}
		for start < len(source) && (source[start] == ' ' || source[start] == '\t' || source[start] == '\n') {
			start++
		}
		// Never start in the middle of a word.
		for start > 0 && start < len(source) && !isBoundaryChar(source[start-1]) && !isBoundaryChar(source[start]) {
			start--
		}
	}

	if end < len(source) {
		hi := end + lookahead
		if hi > len(source) {
			hi = len(source)
		}
		window := source[end:hi]
		if cut := closestBoundaryForward(window); cut >= 0 {
			end = end + cut
		}
		for end > 0 && end < len(source) && !isBoundaryChar(source[end-1]) && !isBoundaryChar(source[end]) {
			end++
		}
	}
	return start, end
}

// closestBoundaryBackward returns the index just after the nearest paragraph or
// sentence boundary in window, or -1.
func closestBoundaryBackward(window string) int {
	best := -1
	if idx := strings.LastIndex(window, "\n\n"); idx >= 0 {
		best = idx + 2
	}
	for i := len(window) - 1; i >= 0; i-- {
		if window[i] == '.' || window[i] == '!' || window[i] == '?' {
			if i+1 >= len(window) || unicode.IsSpace(rune(window[i+1])) {
				if i+2 > best {
					best = i + 2
				}
				break
			}
		}
	}
	return best
}

// closestBoundaryForward returns the index just after the nearest boundary.
func closestBoundaryForward(window string) int {
	best := -1
	if idx := strings.Index(window, "\n\n"); idx >= 0 {
		best = idx + 2
	}
	for i := 0; i < len(window); i++ {
		if window[i] == '.' || window[i] == '!' || window[i] == '?' {
			if i+1 >= len(window) || unicode.IsSpace(rune(window[i+1])) {
				if best == -1 || i+1 < best {
					best = i + 1
				}
				break
			}
		}
	}
	return best
}

func isBoundaryChar(b byte) bool {
	if b == ' ' || b == '\t' || b == '\n' || b == '\r' {
		return true
	}
	r, _ := utf8.DecodeRune([]byte{b})
	return unicode.IsPunct(r) && r != '_'
}

type fence struct{ start, end int }

func fenceRanges(source string) []fence {
	var out []fence
	offset := 0
	var openStart = -1
	for _, line := range strings.Split(source, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			if openStart < 0 {
				openStart = offset
			} else {
				out = append(out, fence{start: openStart, end: offset + len(line)})
				openStart = -1
			}
		}
		offset += len(line) + 1
	}
	if openStart >= 0 {
		out = append(out, fence{start: openStart, end: len(source)})
	}
	return out
}

func snapToFences(fences []fence, start, end int) (int, int) {
	for _, f := range fences {
		if start > f.start && start < f.end {
			start = f.start
		}
		if end > f.start && end < f.end {
			end = f.end
		}
	}
	if end > start {
		// Do not include a fence opening without its body.
		for _, f := range fences {
			if start < f.start && end > f.start && end < f.end {
				end = f.start
			}
		}
	}
	if end < start {
		end = start
	}
	return start, end
}

func truncate(s string, maxChars int) string {
	if len(s) <= maxChars {
		return s
	}
	cut := maxChars
	if idx := strings.LastIndex(s[:cut], "\n\n"); idx > maxChars/3 {
		cut = idx
	} else if idx := strings.LastIndexAny(s[:cut], " \n"); idx > maxChars/2 {
		cut = idx
	}
	// Never leave a broken UTF-8 rune.
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimSpace(s[:cut])
}
