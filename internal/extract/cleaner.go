package extract

import (
	"hash/fnv"
	"net/url"
	"regexp"
	"strings"
	"unicode"

	"github.com/PuerkitoBio/goquery"
)

// CleanReport maps a rule name to the number of bytes it removed.
type CleanReport map[string]int64

// CleanConfig are the tunables for the cleanup layer.
type CleanConfig struct {
	Enabled       bool
	MaxTokenChars int
	LinkDensity   float64
	MinAlpha      float64
	CTA           []string
}

// Cleaner strips junk from HTML and Markdown. It is safe for concurrent use:
// all state is built once in NewCleaner.
type Cleaner struct {
	cfg       CleanConfig
	rules     CleanRules
	cta       []*regexp.Regexp
	exact     map[string]struct{}
	linkRe    *regexp.Regexp
	imgRes    []*regexp.Regexp
	dataRe    []*regexp.Regexp
	weakSet   map[string]struct{}
	strongSet map[string]struct{}
}

var (
	reLinkedImage = regexp.MustCompile(`\[!\[[^\]]*\]\([^)]*\)\]\([^)]*\)`)
	reImage       = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)
	reHTMLImage   = regexp.MustCompile(`(?is)<img\b[^>]*>`)
	reHTMLSource  = regexp.MustCompile(`(?is)<source\b[^>]*>`)
	reDataImage   = regexp.MustCompile(`!\[[^\]]*\]\(\s*<?data:[^)]*\)`)
	reDataLink    = regexp.MustCompile(`\[([^\]]*)\]\(\s*<?data:[^)]*\)`)
	reLink        = regexp.MustCompile(`\[([^\]]*)\]\(([^)\s]+)\)`)
	reAutoLink    = regexp.MustCompile(`<((?:https?)://[^>\s]+)>`)
	reLongToken   = regexp.MustCompile(`\S+`)
	reFence       = regexp.MustCompile("^\\s*(```|~~~)")
	reListMarker  = regexp.MustCompile(`^\s*(?:[-*+]|\d+[.)])\s+`)
	reBareLink    = regexp.MustCompile(`^\[[^\]]+\]\([^)]+\)$`)
	reTableRow    = regexp.MustCompile(`^\s*\|`)
	reHeading     = regexp.MustCompile(`^\s{0,3}#{1,6}\s`)
	reCookieLine  = regexp.MustCompile(`(?i)^(we use cookies|this (site|website) uses cookies|accept (all )?cookies|cookie (policy|settings|notice))`)
	reAnyLink     = regexp.MustCompile(`\[([^\]]*)\]\(([^)]*)\)`)
	reCiteLink    = regexp.MustCompile(`\[\\?\[\d{1,4}\\?\]\]\(#cite[^)]*\)`)
	reCiteHref    = regexp.MustCompile(`\[.*?\]\(#[^)]*cite_(?:note|ref)[^)]*\)`)
	reCiteNum     = regexp.MustCompile(`\[\d{1,4}\]`)
	reRefLine     = regexp.MustCompile(`^\d{1,4}\.\s+\[`)
	reRefHeading  = regexp.MustCompile(`(?i)^#{1,6}\s*(references|notes|citations|external links|further reading)\b`)
	trackingKeys  = []string{"utm_", "mc_"}
	trackingExact = map[string]struct{}{"ref": {}, "fbclid": {}, "gclid": {}, "msclkid": {}, "yclid": {}, "igshid": {}, "twclid": {}}
)

// NewCleaner builds a cleaner; cfg.CTA replaces the default CTA regex list.
func NewCleaner(cfg CleanConfig) *Cleaner {
	if cfg.MaxTokenChars <= 0 {
		cfg.MaxTokenChars = 2000
	}
	if cfg.LinkDensity <= 0 || cfg.LinkDensity > 1 {
		cfg.LinkDensity = 0.5
	}
	if cfg.MinAlpha <= 0 || cfg.MinAlpha > 1 {
		cfg.MinAlpha = 0.4
	}
	rules := DefaultCleanRules
	phrases := cfg.CTA
	if len(phrases) == 0 {
		phrases = rules.CTA
	}
	c := &Cleaner{
		cfg:       cfg,
		rules:     rules,
		exact:     make(map[string]struct{}, len(rules.ExactJunk)),
		linkRe:    reLink,
		imgRes:    []*regexp.Regexp{reLinkedImage, reImage, reHTMLImage, reHTMLSource},
		dataRe:    []*regexp.Regexp{reDataImage, reDataLink},
		weakSet:   make(map[string]struct{}, len(rules.WeakTokens)),
		strongSet: make(map[string]struct{}, len(rules.StrongTokens)),
	}
	for _, t := range rules.WeakTokens {
		c.weakSet[t] = struct{}{}
	}
	for _, t := range rules.StrongTokens {
		c.strongSet[t] = struct{}{}
	}
	for _, phrase := range phrases {
		if re, err := regexp.Compile("(?i)" + phrase); err == nil {
			c.cta = append(c.cta, re)
		}
	}
	for _, j := range rules.ExactJunk {
		c.exact[strings.ToLower(j)] = struct{}{}
	}
	return c
}

// Enabled reports whether the cleaner does anything.
func (c *Cleaner) Enabled() bool { return c != nil && c.cfg.Enabled }

// CleanHTML removes structural and promotional junk before readability runs.
func (c *Cleaner) CleanHTML(doc *goquery.Document) CleanReport {
	rep := CleanReport{}
	if !c.Enabled() || doc == nil {
		return rep
	}
	for _, sel := range c.rules.RemoveTags {
		removeAll(doc.Find(sel), "html_tag", rep)
	}
	for _, role := range c.rules.RemoveRoles {
		removeAll(doc.Find(`[role="`+role+`"]`), "html_role", rep)
	}
	// Data-URI images are never useful and bloat the Markdown.
	removeAll(doc.Find(`img[src^="data:"], img[srcset*="data:"], source[src^="data:"], source[srcset*="data:"]`), "html_data_uri", rep)

	doc.Find("[class],[id]").Each(func(_ int, s *goquery.Selection) {
		switch goquery.NodeName(s) {
		case "html", "head", "body", "main", "article":
			return
		}
		weak, strong := c.matchTokens(s)
		if !weak && !strong {
			return
		}
		if weak && !strong && insideArticle(s) {
			return
		}
		html, err := goquery.OuterHtml(s)
		if err == nil {
			rep["html_token"] += int64(len(html))
		}
		s.Remove()
	})
	return rep
}

func removeAll(sel *goquery.Selection, rule string, rep CleanReport) {
	sel.Each(func(_ int, s *goquery.Selection) {
		if html, err := goquery.OuterHtml(s); err == nil {
			rep[rule] += int64(len(html))
		}
		s.Remove()
	})
}

func (c *Cleaner) matchTokens(s *goquery.Selection) (weak, strong bool) {
	for _, attr := range []string{"class", "id"} {
		val, ok := s.Attr(attr)
		if !ok {
			continue
		}
		for _, class := range strings.Fields(strings.ToLower(val)) {
			candidates := []string{class}
			candidates = append(candidates, strings.FieldsFunc(class, func(r rune) bool { return r == '-' || r == '_' })...)
			for _, tok := range candidates {
				if _, hit := c.strongSet[tok]; hit {
					strong = true
				}
				if _, hit := c.weakSet[tok]; hit {
					weak = true
				}
			}
			// "ad-" prefix classes (ad-slot, ad-container) are always ads.
			if strings.HasPrefix(class, "ad-") || strings.HasPrefix(class, "advert-") {
				strong = true
			}
		}
	}
	return weak, strong
}

func insideArticle(s *goquery.Selection) bool {
	return s.ParentsFiltered("article, main").Length() > 0
}

// CleanMarkdown applies the image, line, block and dedupe rules.
func (c *Cleaner) CleanMarkdown(md string, includeImages bool) (string, CleanReport) {
	rep := CleanReport{}
	if !c.Enabled() {
		return md, rep
	}
	s := strings.ReplaceAll(md, "\r\n", "\n")

	// Images: drop all when the caller does not want them, otherwise only the
	// data: ones. A data: URI must never survive in any mode.
	if !includeImages {
		for _, re := range c.imgRes {
			s = removeMatches(s, re, "md_image", rep)
		}
	} else {
		s = removeMatches(s, reDataImage, "md_image", rep)
		s = removeMatches(s, reImageDataSrc, "md_data_uri", rep)
	}
	s = removeMatches(s, reDataLink, "md_data_uri", rep)

	lines := strings.Split(s, "\n")
	kept := make([]string, 0, len(lines))
	inFence := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if reFence.MatchString(line) {
			inFence = !inFence
			kept = append(kept, line)
			continue
		}
		if inFence {
			kept = append(kept, line)
			continue
		}
		if reRefLine.MatchString(trimmed) {
			rep["md_ref_line"] += int64(len(line) + 1)
			continue
		}
		// Link cleanup runs outside inline code spans; code content is untouched.
		line = mapOutsideCode(line, func(seg string) string { return cleanInlineLinks(seg, rep) })
		trimmed = strings.TrimSpace(line)
		// Lines that contain inline code are never dropped whole.
		if strings.Contains(line, "`") {
			kept = append(kept, strings.TrimRight(line, " \t"))
			continue
		}
		if hasLongToken(line, c.cfg.MaxTokenChars) {
			rep["md_long_token"] += int64(len(line) + 1)
			continue
		}
		lower := strings.ToLower(trimmed)
		if _, junk := c.exact[lower]; junk && trimmed != "" {
			rep["md_exact_junk"] += int64(len(line) + 1)
			continue
		}
		if reCookieLine.MatchString(trimmed) {
			rep["md_exact_junk"] += int64(len(line) + 1)
			continue
		}
		// Length is measured on visible text: long URLs should not hide a CTA.
		// A CTA buried at the end of a substantial paragraph is handled by
		// trimTrailingCTA instead of dropping the whole paragraph.
		visible := reLink.ReplaceAllString(trimmed, "$1")
		visibleLen := len([]rune(visible))
		if visibleLen < 160 && c.matchesCTA(lower) {
			idx := c.ctaIndex(strings.ToLower(visible))
			if idx <= 40 || visibleLen < 80 {
				rep["md_cta"] += int64(len(line) + 1)
				continue
			}
		}
		line = mapOutsideCode(line, stripTrackingInLine)
		kept = append(kept, strings.TrimRight(line, " \t"))
	}
	s = strings.Join(kept, "\n")

	blocks := splitMarkdownBlocks(s)
	out := make([]string, 0, len(blocks))
	seen := make(map[uint64]struct{}, len(blocks))
	skipUntil := 0
	for i, block := range blocks {
		trimmed := strings.TrimSpace(block)
		if trimmed == "" {
			continue
		}
		if c.introForLinkList(trimmed, blocks, i) {
			rep["md_link_list"] += int64(len(block) + 2)
			continue
		}
		if lvl := headingLevel(trimmed); skipUntil > 0 {
			if lvl > 0 && lvl <= skipUntil {
				skipUntil = 0
			} else {
				rep["md_ref_section"] += int64(len(block) + 2)
				continue
			}
		}
		if lvl := headingLevel(trimmed); lvl > 0 && reRefHeading.MatchString(trimmed) {
			skipUntil = lvl
			rep["md_ref_section"] += int64(len(block) + 2)
			continue
		}
		if c.protectedBlock(trimmed) {
			out = append(out, trimmed)
			continue
		}
		if hasInlineCode(trimmed) {
			out = append(out, trimmed)
			continue
		}
		if c.dropListOfLinks(trimmed) {
			rep["md_link_list"] += int64(len(block) + 2)
			continue
		}
		if density, links := c.linkDensity(trimmed); links >= 2 && density > c.cfg.LinkDensity && len([]rune(trimmed)) < 300 {
			rep["md_link_density"] += int64(len(block) + 2)
			continue
		}
		if c.alphaRatio(trimmed) < c.cfg.MinAlpha {
			rep["md_alpha"] += int64(len(block) + 2)
			continue
		}
		var removed int
		trimmed, removed = c.trimTrailingCTA(trimmed)
		if removed > 0 {
			rep["md_cta_tail"] += int64(removed)
		}
		if trimmed == "" {
			continue
		}
		key := hashNorm(trimmed)
		if _, dup := seen[key]; dup {
			rep["md_dedupe"] += int64(len(block) + 2)
			continue
		}
		seen[key] = struct{}{}
		out = append(out, trimmed)
	}

	out = dropDanglingColons(out, rep)
	s = strings.Join(out, "\n\n")
	s = collapseBlankLines(s)
	return strings.TrimSpace(s), rep
}

var reImageDataSrc = regexp.MustCompile(`(?is)<(?:img|source)\b[^>]*(?:src|srcset)\s*=\s*["']?data:[^>]*>`)

// KeepChunk reports whether a chunk is fit to become a result snippet.
func (c *Cleaner) KeepChunk(text string) bool {
	if c == nil || !c.cfg.Enabled {
		return true
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return false
	}
	if c.protectedBlock(trimmed) || hasInlineCode(trimmed) {
		return true
	}
	if c.dropListOfLinks(trimmed) {
		return false
	}
	if density, links := c.linkDensity(trimmed); links >= 2 && density > c.cfg.LinkDensity && len([]rune(trimmed)) < 300 {
		return false
	}
	return c.alphaRatio(trimmed) >= c.cfg.MinAlpha
}

// ctaIndex returns the earliest byte offset of a CTA match, or -1.
func (c *Cleaner) ctaIndex(lowerLine string) int {
	best := -1
	for _, re := range c.cta {
		if loc := re.FindStringIndex(lowerLine); loc != nil {
			if best == -1 || loc[0] < best {
				best = loc[0]
			}
		}
	}
	return best
}

func (c *Cleaner) matchesCTA(lowerLine string) bool {
	for _, re := range c.cta {
		if re.MatchString(lowerLine) {
			return true
		}
	}
	return false
}

// trimTrailingCTA removes a single trailing promotional sentence from an
// otherwise useful paragraph (e.g. "... You can check out our guide."). It only
// fires on the last sentence, so body prose is preserved.
// mapOutsideCode applies fn to the parts of a line outside `code spans` and
// leaves the spans themselves byte-identical.
func mapOutsideCode(line string, fn func(string) string) string {
	var out strings.Builder
	var seg strings.Builder
	inCode := false
	flush := func() {
		if inCode {
			out.WriteString("`")
			out.WriteString(seg.String())
			out.WriteString("`")
		} else {
			out.WriteString(fn(seg.String()))
		}
		seg.Reset()
	}
	for _, r := range line {
		if r == '`' {
			flush()
			inCode = !inCode
			continue
		}
		seg.WriteRune(r)
	}
	flush()
	return out.String()
}

// cleanInlineLinks removes empty/symbol anchors and citation links/markers from
// one line of prose, counting bytes per rule.
func cleanInlineLinks(line string, rep CleanReport) string {
	line = removeEmptyLinks(line, rep)
	for _, re := range []*regexp.Regexp{reCiteLink, reCiteHref} {
		line = re.ReplaceAllStringFunc(line, func(m string) string {
			rep["md_citation"] += int64(len(m))
			return ""
		})
	}
	line = reCiteNum.ReplaceAllStringFunc(line, func(m string) string {
		rep["md_citation"] += int64(len(m))
		return ""
	})
	return line
}

// removeEmptyLinks drops links whose text has no letters or digits, e.g.
// [](#types) or [¶](#cite-1), while keeping surrounding heading text.
func removeEmptyLinks(s string, rep CleanReport) string {
	return reAnyLink.ReplaceAllStringFunc(s, func(m string) string {
		sub := reAnyLink.FindStringSubmatch(m)
		if len(sub) != 3 {
			return m
		}
		for _, r := range sub[1] {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				return m
			}
		}
		rep["md_empty_link"] += int64(len(m))
		return ""
	})
}

func headingLevel(block string) int {
	trimmed := strings.TrimSpace(block)
	if !strings.HasPrefix(trimmed, "#") {
		return 0
	}
	n := 0
	for n < len(trimmed) && trimmed[n] == '#' {
		n++
	}
	if n > 6 {
		return 0
	}
	return n
}

// dropDanglingColons removes a lead-in line ending in ":" when whatever it
// introduced was cleaned away (next block is a heading or nothing at all).
func dropDanglingColons(blocks []string, rep CleanReport) []string {
	out := blocks[:0]
	for i, b := range blocks {
		trimmed := strings.TrimSpace(b)
		if strings.HasSuffix(trimmed, ":") && !strings.Contains(trimmed, "](") {
			dangling := i+1 >= len(blocks)
			if !dangling {
				next := strings.TrimSpace(blocks[i+1])
				dangling = strings.HasPrefix(next, "#")
			}
			if dangling {
				rep["md_dangling_colon"] += int64(len(b) + 2)
				continue
			}
		}
		out = append(out, b)
	}
	return out
}

func (c *Cleaner) trimTrailingCTA(block string) (string, int) {
	t := strings.TrimSpace(block)
	if reHeading.MatchString(t) || strings.Contains(t, "```") || reTableRow.MatchString(t) {
		return block, 0
	}
	// Never touch lists: a CTA bullet may carry real content.
	if reListMarker.MatchString(t) && strings.Contains(t, "\n") {
		return block, 0
	}
	idx := strings.LastIndex(t, ". ")
	last := t
	cut := 0
	if idx >= 0 {
		last = t[idx+2:]
		cut = idx + 1
	}
	visible := reLink.ReplaceAllString(last, "$1")
	if len([]rune(visible)) >= 160 || !c.matchesCTA(strings.ToLower(last)) {
		return block, 0
	}
	kept := strings.TrimSpace(t[:cut])
	if kept == "" {
		return block, 0
	}
	return kept, len(block) - len(kept)
}

// introForLinkList recognises "More on X:" lines that only introduce a list of
// bare links, so the heading disappears together with the list.
func (c *Cleaner) introForLinkList(block string, blocks []string, i int) bool {
	if i+1 >= len(blocks) {
		return false
	}
	t := strings.TrimSpace(block)
	if len([]rune(t)) > 120 || !strings.HasSuffix(t, ":") {
		return false
	}
	if strings.Contains(t, "](") || reHeading.MatchString(t) {
		return false
	}
	return c.dropListOfLinks(blocks[i+1])
}

func (c *Cleaner) dropListOfLinks(block string) bool {
	lines := strings.Split(block, "\n")
	items, bare := 0, 0
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if !reListMarker.MatchString(trimmed) {
			continue
		}
		items++
		body := strings.TrimSpace(reListMarker.ReplaceAllString(trimmed, ""))
		body = strings.Trim(body, "*-+ ")
		if reBareLink.MatchString(body) {
			bare++
		}
	}
	if items < 2 {
		return false
	}
	if strings.Contains(strings.ToLower(block), "table of contents") && bare*2 >= items {
		return true
	}
	return bare*10 >= items*6
}

func (c *Cleaner) linkDensity(block string) (float64, int) {
	// Denominator is the visible text only: counting link destinations would
	// make navigation blocks look like prose.
	visible := reLink.ReplaceAllString(block, "$1")
	textChars := len([]rune(strings.Join(strings.Fields(visible), "")))
	matches := reLink.FindAllStringSubmatch(block, -1)
	if textChars == 0 {
		return 1, len(matches)
	}
	linkChars := 0
	for _, m := range matches {
		linkChars += len([]rune(m[1]))
	}
	return float64(linkChars) / float64(textChars), len(matches)
}

func (c *Cleaner) alphaRatio(block string) float64 {
	total, alpha := 0, 0
	for _, r := range block {
		if unicode.IsSpace(r) {
			continue
		}
		total++
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			alpha++
		}
	}
	if total == 0 {
		return 0
	}
	return float64(alpha) / float64(total)
}

func (c *Cleaner) protectedBlock(block string) bool {
	trimmed := strings.TrimSpace(block)
	if trimmed == "" {
		return true
	}
	if reHeading.MatchString(trimmed) || strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
		return true
	}
	// Markdown tables survive; ASCII art made of pipes does not (alpha check).
	if reTableRow.MatchString(trimmed) && c.alphaRatio(trimmed) >= c.cfg.MinAlpha {
		return true
	}
	return false
}

func hasInlineCode(block string) bool { return strings.Contains(block, "`") }

func hasLongToken(line string, limit int) bool {
	for _, tok := range reLongToken.FindAllString(line, -1) {
		if len(tok) > limit {
			return true
		}
	}
	return false
}

func stripTrackingInLine(line string) string {
	line = reLink.ReplaceAllStringFunc(line, func(m string) string {
		sub := reLink.FindStringSubmatch(m)
		if len(sub) != 3 {
			return m
		}
		return "[" + sub[1] + "](" + stripTrackingParams(sub[2]) + ")"
	})
	line = reAutoLink.ReplaceAllStringFunc(line, func(m string) string {
		sub := reAutoLink.FindStringSubmatch(m)
		if len(sub) != 2 {
			return m
		}
		return "<" + stripTrackingParams(sub[1]) + ">"
	})
	return line
}

// stripTrackingParams removes campaign parameters while keeping functional ones.
func stripTrackingParams(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.RawQuery == "" {
		return raw
	}
	q := u.Query()
	changed := false
	for key := range q {
		lower := strings.ToLower(key)
		drop := false
		for _, prefix := range trackingKeys {
			if strings.HasPrefix(lower, prefix) {
				drop = true
				break
			}
		}
		if _, hit := trackingExact[lower]; hit {
			drop = true
		}
		if drop {
			q.Del(key)
			changed = true
		}
	}
	if !changed {
		return raw
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func removeMatches(s string, re *regexp.Regexp, rule string, rep CleanReport) string {
	return re.ReplaceAllStringFunc(s, func(m string) string {
		rep[rule] += int64(len(m))
		return ""
	})
}

func splitMarkdownBlocks(s string) []string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines)/2)
	var cur []string
	inFence := false
	flush := func() {
		if b := strings.TrimSpace(strings.Join(cur, "\n")); b != "" {
			out = append(out, b)
		}
		cur = cur[:0]
	}
	for _, line := range lines {
		if reFence.MatchString(line) {
			inFence = !inFence
			cur = append(cur, line)
			continue
		}
		if strings.TrimSpace(line) == "" && !inFence {
			flush()
			continue
		}
		cur = append(cur, line)
	}
	flush()
	return out
}

var reBlankRun = regexp.MustCompile(`\n{3,}`)

func collapseBlankLines(s string) string {
	return reBlankRun.ReplaceAllString(s, "\n\n")
}

func hashNorm(s string) uint64 {
	h := fnv.New64a()
	for _, r := range strings.Join(strings.Fields(s), " ") {
		_, _ = h.Write([]byte(string(r)))
	}
	return h.Sum64()
}

// applyReport folds one report into another (used by ParseHTML).
func applyReport(dst, src CleanReport) {
	for k, v := range src {
		dst[k] += v
	}
}
