package extract

import (
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/astralabs/tavilyd/internal/security"
)

const articleHTML = `<!doctype html><html><head><title>Fallback Title</title>
<meta property="og:title" content="Distributed Systems Primer">
</head><body>
<nav><a href="/">Home</a></nav>
<div class="cookie-banner">We use cookies. Cookie policy.</div>
<article>
<h1>Distributed Systems Primer</h1>
<p>Distributed systems are collections of independent computers that appear to users as a single coherent system. They introduce partial failure, unreliable networks and clock skew.</p>
<p>Consensus algorithms such as Raft and Paxos let replicas agree on a sequence of values even when a minority of nodes fail. This paragraph exists to give readability enough text to keep the article.</p>
<p>Caching layers, consistent hashing and retries with jitter are the practical tools that make these systems operable at scale.</p>
<img src="/img/diagram.png" alt="diagram">
</article>
<footer>Copyright 2026</footer>
</body></html>`

func TestParseHTMLExtractsMainContent(t *testing.T) {
	art, err := ParseHTML([]byte(articleHTML), "https://example.com/post", "text/html; charset=utf-8", ParseOptions{Format: "markdown", IncludeImages: true})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if art.Title != "Distributed Systems Primer" {
		t.Fatalf("title = %q", art.Title)
	}
	if !strings.Contains(art.Markdown, "Consensus algorithms") {
		t.Fatalf("main content missing: %q", art.Markdown)
	}
	if strings.Contains(art.Markdown, "Cookie policy") || strings.Contains(art.Markdown, "Copyright 2026") {
		t.Fatalf("boilerplate leaked into markdown: %q", art.Markdown)
	}
	if len(art.Images) != 1 || art.Images[0] != "https://example.com/img/diagram.png" {
		t.Fatalf("images = %v", art.Images)
	}
	if art.Length < 200 {
		t.Fatalf("length = %d", art.Length)
	}
}

func TestParseHTMLTextFormat(t *testing.T) {
	art, err := ParseHTML([]byte(articleHTML), "https://example.com/post", "text/html", ParseOptions{Format: "text"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if strings.Contains(art.Markdown, "#") || strings.Contains(art.Markdown, "**") {
		t.Fatalf("text format still contains markdown: %q", art.Markdown)
	}
	if !strings.Contains(art.Markdown, "Distributed systems") {
		t.Fatalf("text output missing content: %q", art.Markdown)
	}
}

func TestParseHTMLStripsImagesWhenDisabled(t *testing.T) {
	art, err := ParseHTML([]byte(articleHTML), "https://example.com/post", "text/html", ParseOptions{IncludeImages: false})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(art.Markdown, "![") {
		t.Fatalf("images should be removed: %q", art.Markdown)
	}
}

func TestIsChallenge(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{200, "<html><title>Just a moment...</title></html>", true},
		{200, "<html>Checking your browser before accessing</html>", true},
		{200, "<html><body>Enable JavaScript and cookies to continue</body></html>", true},
		{403, "<html>Forbidden</html>", true},
		{200, "<html><article>Real content about databases</article></html>", false},
		{200, "", false},
	}
	for _, c := range cases {
		if got := IsChallenge(c.status, []byte(c.body)); got != c.want {
			t.Errorf("IsChallenge(%d, %q) = %v, want %v", c.status, c.body, got, c.want)
		}
	}
}

func newServerMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/article", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(articleHTML))
	})
	mux.HandleFunc("/gzip", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(w)
		_, _ = zw.Write([]byte(articleHTML))
		_ = zw.Close()
	})
	mux.HandleFunc("/huge", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body>" + strings.Repeat("x", 200<<10) + "</body></html>"))
	})
	mux.HandleFunc("/binary", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write([]byte("%PDF-1.7"))
	})
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/article", http.StatusFound)
	})
	mux.HandleFunc("/loop", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/loop", http.StatusFound)
	})
	return mux
}

func testFetcher(t *testing.T, allowPrivate bool, maxBytes int64) *HTTPFetcher {
	t.Helper()
	g := security.NewGuard(allowPrivate, 2*time.Second)
	return NewHTTPFetcher(g, maxBytes, "test-agent", 3*time.Second, 3, false)
}

func TestHTTPFetcherFetch(t *testing.T) {
	srv := httptest.NewServer(newServerMux())
	defer srv.Close()

	f := testFetcher(t, true, 1<<20)
	target, err := security.NewGuard(true, time.Second).ValidateURL(context.Background(), srv.URL+"/article")
	if err != nil {
		t.Fatal(err)
	}
	page, err := f.Fetch(context.Background(), target)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if page.Status != 200 || !strings.Contains(string(page.Body), "Distributed Systems") {
		t.Fatalf("unexpected page: status=%d body=%d bytes", page.Status, len(page.Body))
	}
	if page.Tier != 1 {
		t.Fatalf("tier = %d", page.Tier)
	}
}

func TestHTTPFetcherGzip(t *testing.T) {
	srv := httptest.NewServer(newServerMux())
	defer srv.Close()
	f := testFetcher(t, true, 1<<20)
	g := security.NewGuard(true, time.Second)
	target, _ := g.ValidateURL(context.Background(), srv.URL+"/gzip")
	page, err := f.Fetch(context.Background(), target)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !strings.Contains(string(page.Body), "Distributed Systems") {
		t.Fatal("gzip body not decoded")
	}
}

func TestHTTPFetcherSizeCap(t *testing.T) {
	srv := httptest.NewServer(newServerMux())
	defer srv.Close()
	f := testFetcher(t, true, 64<<10)
	g := security.NewGuard(true, time.Second)
	target, _ := g.ValidateURL(context.Background(), srv.URL+"/huge")
	if _, err := f.Fetch(context.Background(), target); err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("expected size-limit error, got %v", err)
	}
}

func TestHTTPFetcherRejectsBinary(t *testing.T) {
	srv := httptest.NewServer(newServerMux())
	defer srv.Close()
	f := testFetcher(t, true, 1<<20)
	g := security.NewGuard(true, time.Second)
	target, _ := g.ValidateURL(context.Background(), srv.URL+"/binary")
	if _, err := f.Fetch(context.Background(), target); err == nil {
		t.Fatal("expected unsupported content type error")
	}
}

func TestHTTPFetcherFollowsRedirect(t *testing.T) {
	srv := httptest.NewServer(newServerMux())
	defer srv.Close()
	f := testFetcher(t, true, 1<<20)
	g := security.NewGuard(true, time.Second)
	target, _ := g.ValidateURL(context.Background(), srv.URL+"/redirect")
	page, err := f.Fetch(context.Background(), target)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !strings.Contains(string(page.Body), "Distributed Systems") {
		t.Fatal("redirect target body missing")
	}
	// A previous final URL is not returned because each hop is a fresh request;
	// the important part is that the chain was followed.
	if !strings.Contains(page.URL, "/article") && !strings.Contains(page.URL, "/redirect") {
		t.Fatalf("unexpected final url: %s", page.URL)
	}
}

func TestHTTPFetcherRedirectCap(t *testing.T) {
	srv := httptest.NewServer(newServerMux())
	defer srv.Close()
	f := testFetcher(t, true, 1<<20)
	g := security.NewGuard(true, time.Second)
	target, _ := g.ValidateURL(context.Background(), srv.URL+"/loop")
	if _, err := f.Fetch(context.Background(), target); err == nil {
		t.Fatal("expected redirect loop to fail")
	}
}

func TestBrowserDisabled(t *testing.T) {
	b := &Browser{}
	if b.Name() != "browser" {
		t.Fatal("name")
	}
	if _, err := b.Fetch(context.Background(), nil); err == nil {
		t.Fatal("disabled browser must error")
	}
}
