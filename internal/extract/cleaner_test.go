package extract

import (
	"strings"
	"testing"
	"time"

	"github.com/PuerkitoBio/goquery"
)

func testCleaner() *Cleaner {
	return NewCleaner(CleanConfig{Enabled: true, MaxTokenChars: 2000, LinkDensity: 0.5, MinAlpha: 0.4})
}

func TestCleanMarkdownImages(t *testing.T) {
	md := strings.Join([]string{
		"# Title",
		"",
		"Before ![alt](https://example.com/a.png) after.",
		"",
		"[![badge](https://example.com/b.svg)](https://example.com)",
		"",
		"![inline](data:image/svg+xml;base64,AAAA)",
		"",
		"[click](data:text/html;base64,AAAA)",
		"",
		"Text stays.",
	}, "\n")

	off, _ := testCleaner().CleanMarkdown(md, false)
	if strings.Contains(off, "![") {
		t.Fatalf("image syntax survived when images are off:\n%s", off)
	}
	if strings.Contains(off, "data:") {
		t.Fatalf("data URI survived when images are off:\n%s", off)
	}
	if !strings.Contains(off, "Text stays.") {
		t.Fatal("prose was removed")
	}

	on, _ := testCleaner().CleanMarkdown(md, true)
	if !strings.Contains(on, "https://example.com/a.png") {
		t.Fatalf("plain image should survive when images are on:\n%s", on)
	}
	if strings.Contains(on, "data:image") {
		t.Fatalf("data URI must never survive:\n%s", on)
	}
}

func TestTrackingParamsStripped(t *testing.T) {
	md := "See [the docs](https://example.com/page?utm_source=x&utm_medium=y&keep=1&fbclid=z#frag)."
	got, _ := testCleaner().CleanMarkdown(md, true)
	if strings.Contains(got, "utm_") || strings.Contains(got, "fbclid") {
		t.Fatalf("tracking params survived: %s", got)
	}
	if !strings.Contains(got, "keep=1") || !strings.Contains(got, "#frag") {
		t.Fatalf("functional params or fragment were dropped: %s", got)
	}
}

func TestCTARules(t *testing.T) {
	c := testCleaner()
	short := "Install in 5 mins or less."
	long := strings.Repeat("This is a long explanatory paragraph about probes. ", 6) + "Install in production carefully."
	out, rep := c.CleanMarkdown(short+"\n\n"+long, true)
	if strings.Contains(out, "Install in 5 mins") {
		t.Fatalf("short CTA line survived: %s", out)
	}
	if !strings.Contains(out, "long explanatory paragraph") {
		t.Fatalf("long paragraph was wrongly dropped: %s", out)
	}
	if rep["md_cta"] == 0 {
		t.Fatal("CTA removal was not counted")
	}
}

func TestTrailingCTATrimmedFromParagraph(t *testing.T) {
	para := "A readiness probe decides when a pod receives traffic and is distinct from a liveness probe. You can check out our getting started guide."
	out, rep := testCleaner().CleanMarkdown(para, true)
	if strings.Contains(strings.ToLower(out), "check out our") {
		t.Fatalf("trailing CTA survived: %q", out)
	}
	if !strings.Contains(out, "readiness probe decides") {
		t.Fatalf("body prose was removed: %q", out)
	}
	if rep["md_cta_tail"] == 0 {
		t.Fatal("md_cta_tail not counted")
	}
}

func TestLongTokenOutsideFence(t *testing.T) {
	longToken := strings.Repeat("A", 2500)
	md := "junk " + longToken + "\n\n```\n" + longToken + "\n```\n\nkept prose"
	out, rep := testCleaner().CleanMarkdown(md, true)
	if strings.Contains(out, "junk "+longToken) {
		t.Fatal("long-token line outside a fence survived")
	}
	if !strings.Contains(out, longToken) {
		t.Fatal("long token inside a fence was removed")
	}
	if !strings.Contains(out, "kept prose") {
		t.Fatal("prose removed")
	}
	if rep["md_long_token"] == 0 {
		t.Fatal("long-token removal not counted")
	}
}

func TestAlphaRatioDropsSymbolJunk(t *testing.T) {
	md := "|||| ||| ||| --- ||\n\nReal sentences with words."
	out, _ := testCleaner().CleanMarkdown(md, true)
	if strings.Contains(out, "||||") {
		t.Fatalf("symbol junk survived: %q", out)
	}
	if !strings.Contains(out, "Real sentences") {
		t.Fatal("prose removed")
	}
}

func TestLinkDensityAndLinkLists(t *testing.T) {
	nav := "[Home](https://x.com/) [Docs](https://x.com/docs) [Blog](https://x.com/blog) [Pricing](https://x.com/pricing)"
	lists := "More on Kubernetes:\n\n- [Readiness probes](https://x.com/a)\n- [Liveness probes](https://x.com/b)\n- [Startup probes](https://x.com/c)"
	prose := "A readiness probe tells the kubelet when a container is ready to receive traffic, and it is distinct from a liveness probe."

	out, _ := testCleaner().CleanMarkdown(nav+"\n\n"+lists+"\n\n"+prose, true)
	if strings.Contains(out, "Pricing") {
		t.Fatalf("link-dense paragraph survived: %s", out)
	}
	if strings.Contains(out, "More on Kubernetes") {
		t.Fatalf("list of links survived: %s", out)
	}
	if !strings.Contains(out, "readiness probe tells") {
		t.Fatal("prose removed")
	}
}

func TestDedupeRepeatedParagraphs(t *testing.T) {
	p := "Kubernetes restarts containers that fail their probes repeatedly."
	md := p + "\n\n" + p + "\n\n" + strings.Repeat(p+"  ", 1)
	out, rep := testCleaner().CleanMarkdown(md, true)
	if strings.Count(out, "Kubernetes restarts containers") != 1 {
		t.Fatalf("duplicate paragraphs survived:\n%s", out)
	}
	if rep["md_dedupe"] == 0 {
		t.Fatal("dedupe not counted")
	}
}

func TestIdempotent(t *testing.T) {
	inputs := []string{
		"# Head\n\nInstall in 5 mins.\n\n[Home](https://x.com/?utm_source=a) [Docs](https://x.com/d)\n\nKeep this prose paragraph about readiness probes and traffic.",
		"```\ncode stays\n```\n\n![img](data:image/png;base64,XX)\n\nRepeated paragraph.\n\nRepeated paragraph.",
		"",
	}
	c := testCleaner()
	for i, in := range inputs {
		once, _ := c.CleanMarkdown(in, false)
		twice, _ := c.CleanMarkdown(once, false)
		if once != twice {
			t.Fatalf("case %d not idempotent:\n--- once ---\n%s\n--- twice ---\n%s", i, once, twice)
		}
	}
}

func TestKeepChunk(t *testing.T) {
	c := testCleaner()
	if c.KeepChunk("More on X:\n\n- [a](https://x.com/a)\n- [b](https://x.com/b)\n- [c](https://x.com/c)") {
		t.Fatal("link-list chunk should be dropped")
	}
	if !c.KeepChunk("Probes are checked periodically by the kubelet and failures trigger restarts.") {
		t.Fatal("prose chunk should be kept")
	}
	if !c.KeepChunk("## Heading with [link](https://x.com)") {
		t.Fatal("heading chunk should be kept")
	}
	if !c.KeepChunk("```\nvar x = 1\n```") {
		t.Fatal("code chunk should be kept")
	}
}

func TestCleanHTMLStructure(t *testing.T) {
	html := `<html><body>
<nav>menu</nav>
<div class="promo-banner">Promo outside article</div>
<div class="header-title">Brand</div>
<article>
  <h1 class="header-title">Real title</h1>
  <div class="cookie-banner">Cookie consent inside article</div>
  <div class="sidebar">Sidebar inside article</div>
  <p>Body text about probes.</p>
  <img src="data:image/png;base64,AAAA">
  <img src="https://example.com/ok.png">
</article>
<footer>footer</footer>
</body></html>`

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		t.Fatal(err)
	}
	rep := testCleaner().CleanHTML(doc)
	body, _ := goquery.OuterHtml(doc.Selection)

	if strings.Contains(body, "Promo outside article") {
		t.Fatal("promo-banner outside article survived")
	}
	if !strings.Contains(body, "Real title") || !strings.Contains(body, "Body text") {
		t.Fatal("article content was over-stripped")
	}
	if !strings.Contains(body, "header-title") {
		t.Fatal("header-title class was wrongly removed")
	}
	if strings.Contains(body, "Cookie consent inside article") {
		t.Fatal("strong-signal cookie banner inside article survived")
	}
	if !strings.Contains(body, "Sidebar inside article") {
		t.Fatal("weak sidebar token inside article was wrongly removed")
	}
	if strings.Contains(body, "data:image") {
		t.Fatal("data URI image survived HTML pre-clean")
	}
	if !strings.Contains(body, "https://example.com/ok.png") {
		t.Fatal("normal image removed")
	}
	if rep["html_tag"] == 0 || rep["html_token"] == 0 || rep["html_data_uri"] == 0 {
		t.Fatalf("removal counters not populated: %v", rep)
	}
}

func BenchmarkCleanMarkdown(b *testing.B) {
	body, err := readFixture("kodekloud.html")
	if err != nil {
		b.Skip("fixture missing")
	}
	c := testCleaner()
	var md string
	for i := 0; i < 20; i++ {
		md += "# Section\n\nInstall in 5 mins. [Home](https://x.com/?utm_source=a)\n\nReadiness probes decide when a pod receives traffic.\n\n"
	}
	_ = body
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, _ := c.CleanMarkdown(md, false)
		if out == "" {
			b.Fatal("empty output")
		}
	}
}

// nothing here should take longer than 10 ms for a realistic page
func TestCleanLatency(t *testing.T) {
	body, err := readFixture("kodekloud.html")
	if err != nil {
		t.Skip("fixture missing")
	}
	art, err := ParseHTML(body, "https://kodekloud.com/blog/kubernetes-readiness-probe", "text/html", ParseOptions{
		Format: "markdown", IncludeImages: false, Cleaner: testCleaner(),
	})
	if err != nil {
		t.Fatal(err)
	}
	limit := 10 * time.Millisecond
	if raceDetector {
		limit = 150 * time.Millisecond
	}
	if art.CleanDuration > limit {
		t.Fatalf("cleaning took %v, want < %v", art.CleanDuration, limit)
	}
	t.Logf("cleaned kodekloud in %v, removed %d bytes across %d rules", art.CleanDuration, totalRemoved(art.Clean), len(art.Clean))
}

func totalRemoved(r CleanReport) int64 {
	var n int64
	for _, v := range r {
		n += v
	}
	return n
}
