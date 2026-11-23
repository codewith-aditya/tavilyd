package extract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	htmltomarkdown "github.com/JohannesKaufmann/html-to-markdown/v2"
	"github.com/PuerkitoBio/goquery"
	readability "github.com/go-shiori/go-readability"
	"golang.org/x/net/html/charset"
)

// ParseOptions controls rendering of an extracted page.
type ParseOptions struct {
	Format        string // "markdown" (default) or "text"
	IncludeImages bool
	MaxChars      int
	Cleaner       *Cleaner // nil disables the cleanup layer (tests only)
}

// legacyNoiseSelectors is the pre-cleaner fallback used only when no Cleaner is
// configured. Production always passes a Cleaner.
var legacyNoiseSelectors = []string{
	"script", "style", "noscript", "template", "svg", "iframe", "form", "button",
	"nav", "footer", "aside",
}

var (
	reBlankLines    = regexp.MustCompile(`\n{3,}`)
	reSpaces        = regexp.MustCompile(`[ \t]+\n`)
	reZeroWidth     = regexp.MustCompile("[\u200b\u200c\u200d\ufeff]")
	reMarkdownImage = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)
)

// ParseHTML converts an HTML document into an Article. Readability picks the
// main content; html-to-markdown renders it; the cleanup layer removes junk
// before and after that step.
func ParseHTML(body []byte, pageURL, contentType string, opts ParseOptions) (*Article, error) {
	if opts.MaxChars <= 0 {
		opts.MaxChars = 1 << 20
	}
	u, err := url.Parse(pageURL)
	if err != nil {
		return nil, fmt.Errorf("parse page url: %w", err)
	}

	utf8Body, err := toUTF8(body, contentType)
	if err != nil {
		return nil, err
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(utf8Body))
	if err != nil {
		return nil, fmt.Errorf("parse html: %w", err)
	}

	report := CleanReport{}
	var cleanDuration time.Duration
	if opts.Cleaner != nil && opts.Cleaner.Enabled() {
		started := time.Now()
		applyReport(report, opts.Cleaner.CleanHTML(doc))
		cleanDuration += time.Since(started)
	} else {
		doc.Find(strings.Join(legacyNoiseSelectors, ",")).Remove()
	}

	cleaned, err := goquery.OuterHtml(doc.Selection)
	if err != nil {
		return nil, fmt.Errorf("serialize html: %w", err)
	}

	art := &Article{Title: metaTitle(doc)}
	if parsed, rerr := readability.FromReader(strings.NewReader(cleaned), u); rerr == nil {
		if strings.TrimSpace(parsed.Title) != "" {
			art.Title = strings.TrimSpace(parsed.Title)
		}
		art.Byline = parsed.Byline
		art.SiteName = parsed.SiteName
		art.Excerpt = parsed.Excerpt
		art.Text = parsed.TextContent
		if len(strings.TrimSpace(parsed.Content)) > 0 {
			cleaned = parsed.Content
		}
	}

	md, err := htmltomarkdown.ConvertString(cleaned)
	if err != nil {
		return nil, fmt.Errorf("markdown conversion: %w", err)
	}
	md = postProcessMarkdown(md)
	if opts.Cleaner != nil && opts.Cleaner.Enabled() {
		var mdReport CleanReport
		started := time.Now()
		md, mdReport = opts.Cleaner.CleanMarkdown(md, opts.IncludeImages)
		cleanDuration += time.Since(started)
		applyReport(report, mdReport)
	} else if !opts.IncludeImages {
		md = reMarkdownImage.ReplaceAllString(md, "")
	}
	if len(md) > opts.MaxChars {
		md = md[:opts.MaxChars]
	}
	if art.Text == "" {
		art.Text = stripMarkdown(md)
	}
	art.Markdown = md
	art.Length = len(strings.TrimSpace(md))
	art.Images = collectImages(cleaned, u, 25)
	art.PublishedDate = extractPublishedDate(doc)
	art.Clean = report
	art.CleanDuration = cleanDuration
	if opts.Format == "text" {
		art.Markdown = strings.TrimSpace(art.Text)
	}
	return art, nil
}

// IsChallenge heuristically detects interstitial "prove you are human" pages.
func IsChallenge(status int, body []byte) bool {
	if status == 403 || status == 429 || status == 503 {
		return true
	}
	if len(body) == 0 {
		return false
	}
	lower := strings.ToLower(string(body))
	if len(lower) > 200_000 {
		lower = lower[:200_000]
	}
	markers := []string{
		"just a moment", "checking your browser", "cf-chl", "_cf_chl_opt",
		"attention required! | cloudflare", "enable javascript and cookies to continue",
		"ddos-guard", "px-captcha", "captcha-delivery.com", "access denied",
		"verify you are human", "unusual traffic from your computer network",
		"please enable js and disable any ad blocker", "akamai reference #",
		"you have been blocked", "you are unable to access", "cf-error-details",
	}
	for _, m := range markers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// NeedsBrowser reports whether tier 2 is warranted for a fetched page.
func NeedsBrowser(page *Page, article *Article) bool {
	if page == nil {
		return false
	}
	switch page.Status {
	case 403, 429, 503:
		return true
	}
	if page.Status >= 400 {
		// 404/410/500 pages do not get better in a browser.
		return false
	}
	if article == nil || len(strings.TrimSpace(article.Markdown)) < 200 {
		return true
	}
	return IsChallenge(page.Status, page.Body)
}

func toUTF8(body []byte, contentType string) ([]byte, error) {
	r, err := charset.NewReader(bytes.NewReader(body), contentType)
	if err != nil {
		// Unknown charset: most pages are UTF-8 in practice.
		return body, nil
	}
	out, err := io.ReadAll(io.LimitReader(r, int64(len(body))*4+1024))
	if err != nil || len(out) == 0 {
		return body, nil
	}
	return out, nil
}

// extractPublishedDate checks the common metadata sources in priority order.
func extractPublishedDate(doc *goquery.Document) string {
	selectors := []string{
		`meta[property="article:published_time"]`,
		`meta[name="article:published_time"]`,
		`meta[property="og:published_time"]`,
		`meta[name="date"]`,
		`meta[itemprop="datePublished"]`,
	}
	for _, sel := range selectors {
		if v, ok := doc.Find(sel).First().Attr("content"); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	if v, ok := doc.Find(`time[datetime]`).First().Attr("datetime"); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	var found string
	doc.Find(`script[type="application/ld+json"]`).EachWithBreak(func(_ int, sel *goquery.Selection) bool {
		var v any
		if err := json.Unmarshal([]byte(sel.Text()), &v); err != nil {
			return true
		}
		if d := jsonLDDate(v); d != "" {
			found = d
			return false
		}
		return true
	})
	return found
}

func jsonLDDate(v any) string {
	switch t := v.(type) {
	case map[string]any:
		if d, ok := t["datePublished"].(string); ok && d != "" {
			return d
		}
		for _, child := range t {
			if d := jsonLDDate(child); d != "" {
				return d
			}
		}
	case []any:
		for _, child := range t {
			if d := jsonLDDate(child); d != "" {
				return d
			}
		}
	}
	return ""
}

func metaTitle(doc *goquery.Document) string {
	for _, sel := range []string{`meta[property="og:title"]`, `meta[name="twitter:title"]`} {
		if v, ok := doc.Find(sel).First().Attr("content"); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	if t := strings.TrimSpace(doc.Find("title").First().Text()); t != "" {
		return t
	}
	if h1 := strings.TrimSpace(doc.Find("h1").First().Text()); h1 != "" {
		return h1
	}
	return ""
}

// postProcessMarkdown only normalizes whitespace; content rules live in Cleaner.
func postProcessMarkdown(md string) string {
	md = reZeroWidth.ReplaceAllString(md, "")
	md = reSpaces.ReplaceAllString(md, "\n")
	lines := strings.Split(md, "\n")
	out := lines[:0]
	for _, l := range lines {
		out = append(out, strings.TrimRight(l, " \t"))
	}
	md = strings.Join(out, "\n")
	md = reBlankLines.ReplaceAllString(md, "\n\n")
	return strings.TrimSpace(md)
}

func stripMarkdown(md string) string {
	repl := strings.NewReplacer(
		"**", "", "__", "", "`", "", "#", "", ">", "",
		"[", "", "](", " (", ")", "",
	)
	return strings.TrimSpace(repl.Replace(md))
}

func collectImages(htmlFragment string, base *url.URL, limit int) []string {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(htmlFragment))
	if err != nil {
		return nil
	}
	seen := make(map[string]struct{}, limit)
	out := make([]string, 0, limit)
	doc.Find("img").EachWithBreak(func(_ int, s *goquery.Selection) bool {
		src, ok := s.Attr("src")
		if !ok {
			if lazy, ok2 := s.Attr("data-src"); ok2 {
				src = lazy
			}
		}
		if strings.TrimSpace(src) == "" || strings.HasPrefix(strings.ToLower(strings.TrimSpace(src)), "data:") {
			return true
		}
		ref, err := url.Parse(strings.TrimSpace(src))
		if err != nil {
			return true
		}
		abs := base.ResolveReference(ref).String()
		if _, dup := seen[abs]; dup {
			return true
		}
		seen[abs] = struct{}{}
		out = append(out, abs)
		return len(out) < limit
	})
	return out
}
