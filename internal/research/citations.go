package research

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

var reCitation = regexp.MustCompile(`\[S(\d+)\]`)

// VerifyCitations keeps only markers that resolve to a real source, removes
// invalid ones and returns the cited sources in first-use order.
func VerifyCitations(content string, sources []Source) (string, []Source) {
	byID := make(map[int]Source, len(sources))
	for _, s := range sources {
		byID[s.ID] = s
	}
	seen := map[int]bool{}
	var order []int
	cleaned := reCitation.ReplaceAllStringFunc(content, func(m string) string {
		var id int
		_, _ = fmt.Sscanf(m, "[S%d]", &id)
		if _, ok := byID[id]; !ok {
			return ""
		}
		if !seen[id] {
			seen[id] = true
			order = append(order, id)
		}
		return m
	})
	cleaned = strings.ReplaceAll(cleaned, "  ", " ")
	cited := make([]Source, 0, len(order))
	for _, id := range order {
		cited = append(cited, byID[id])
	}
	return cleaned, cited
}

// RenderCitations appends a deterministic reference list in the requested
// format. Metadata beyond title/URL/access date is not fetched, so MLA/APA/
// Chicago entries are best-effort and say so structurally (no invented authors).
func RenderCitations(content string, cited []Source, format string, accessed time.Time) string {
	if len(cited) == 0 {
		return content
	}
	order := make(map[int]int, len(cited))
	for i, s := range cited {
		order[s.ID] = i + 1
	}
	// Renumber citations to 1..N in first-use order.
	content = reCitation.ReplaceAllStringFunc(content, func(m string) string {
		var id int
		_, _ = fmt.Sscanf(m, "[S%d]", &id)
		if n, ok := order[id]; ok {
			return fmt.Sprintf("[%d]", n)
		}
		return m
	})
	date := accessed.Format("2 January 2006")
	var b strings.Builder
	b.WriteString(strings.TrimRight(content, "\n"))
	b.WriteString("\n\n## Sources\n\n")
	for i, s := range cited {
		title := strings.TrimSpace(s.Title)
		if title == "" {
			title = s.URL
		}
		switch format {
		case "mla":
			fmt.Fprintf(&b, "%d. \"%s.\" Web. %s. <%s>\n", i+1, title, date, s.URL)
		case "apa":
			fmt.Fprintf(&b, "%d. %s. (%s). Retrieved from %s\n", i+1, title, accessed.Format("2006, January 2"), s.URL)
		case "chicago":
			fmt.Fprintf(&b, "%d. \"%s.\" Accessed %s. %s.\n", i+1, title, date, s.URL)
		default: // numbered
			fmt.Fprintf(&b, "%d. [%s](%s)\n", i+1, title, s.URL)
		}
	}
	return b.String()
}

// SortSources orders by score desc then ID.
func SortSources(sources []Source) {
	sort.SliceStable(sources, func(i, j int) bool {
		if sources[i].Score == sources[j].Score {
			return sources[i].ID < sources[j].ID
		}
		return sources[i].Score > sources[j].Score
	})
}

// trimToTokens bounds prompt text by the documented chars/4 approximation with
// a 10% safety margin.
func trimToTokens(s string, maxTokens int) string {
	if maxTokens <= 0 {
		return s
	}
	limit := maxTokens * 4 * 9 / 10
	if len(s) <= limit {
		return s
	}
	cut := limit
	if idx := strings.LastIndex(s[:cut], "\n\n"); idx > cut/2 {
		cut = idx
	}
	return s[:cut]
}
