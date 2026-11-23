package extract

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/astralabs/tavilyd/internal/cache"
	"github.com/astralabs/tavilyd/internal/config"
	"github.com/astralabs/tavilyd/internal/observability"
	"github.com/astralabs/tavilyd/internal/security"
)

func testConfig() *config.Config {
	return &config.Config{
		RedisURL: "redis://127.0.0.1:1/0", RedisTimeout: 20 * time.Millisecond, LRUEntries: 64,
		FetchBasicTimeout: 2 * time.Second, FetchAdvancedTimeout: 3 * time.Second,
		FetchMaxBytes: 1 << 20, FetchConcurrency: 4, FetchUserAgent: "test-agent", MaxRedirects: 3,
		BrowserURL: "off", BrowserMaxTabs: 2,
		ExtractCacheTTL: time.Hour, NegativeCacheTTL: time.Minute,
		CleanEnabled: true, CleanMaxTokenChars: 2000, CleanLinkDensity: 0.5, CleanMinAlpha: 0.4,
		CacheVersion: "test",
		ChunkTokens:  350, ChunkOverlap: 50, MaxChunksPerPage: 24, MaxChunksTotal: 200,
	}
}

func newTestExtractor(t *testing.T, srv *httptest.Server) (*Extractor, *cache.Cache) {
	t.Helper()
	cfg := testConfig()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	metrics := observability.NewMetrics()
	c, err := cache.New(cfg, log, metrics)
	if err != nil {
		t.Fatalf("cache: %v", err)
	}
	guard := security.NewGuard(true, 2*time.Second)
	e := NewExtractor(cfg, guard, c, context.Background(), log, metrics)
	t.Cleanup(func() {
		e.Close()
		_ = c.Close()
	})
	return e, c
}

func TestFetchPageCachesResult(t *testing.T) {
	srv := httptest.NewServer(newServerMux())
	defer srv.Close()
	e, _ := newTestExtractor(t, srv)

	art, tier, err := e.FetchPage(context.Background(), srv.URL+"/article", "basic", false)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if tier != 1 {
		t.Fatalf("tier = %d, want 1", tier)
	}
	if !strings.Contains(art.Markdown, "Distributed systems") {
		t.Fatalf("markdown missing content: %q", art.Markdown)
	}

	art2, tier2, err := e.FetchPage(context.Background(), srv.URL+"/article", "basic", false)
	if err != nil {
		t.Fatalf("cached fetch: %v", err)
	}
	if tier2 != 0 {
		t.Fatalf("second fetch tier = %d, want 0 (cache)", tier2)
	}
	if art2.Markdown != art.Markdown {
		t.Fatal("cached markdown differs")
	}
}

func TestFetchPageNegativeCache(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL + "/anything"
	srv.Close() // connection refused from now on

	e, _ := newTestExtractor(t, srv)
	if _, _, err := e.FetchPage(context.Background(), url, "basic", false); err == nil {
		t.Fatal("expected failure")
	}
	_, _, err := e.FetchPage(context.Background(), url, "basic", false)
	if err == nil || !strings.Contains(err.Error(), "negative cache") {
		t.Fatalf("expected negative cache hit, got %v", err)
	}
}

func TestExtractManyStreamsUntilWanted(t *testing.T) {
	mux := http.NewServeMux()
	for _, p := range []string{"/a", "/b", "/c", "/d", "/e"} {
		p := p
		mux.HandleFunc(p, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(strings.Replace(articleHTML, "Distributed Systems Primer", "Article "+p, 1)))
		})
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()
	e, _ := newTestExtractor(t, srv)

	cands := make([]Candidate, 0, 5)
	for _, p := range []string{"/a", "/b", "/c", "/d", "/e"} {
		cands = append(cands, Candidate{URL: srv.URL + p, Title: p, Snippet: "snippet"})
	}
	timings := observability.NewTimings()
	docs := e.ExtractMany(context.Background(), cands, "basic", 3, timings, false)
	if len(docs) != 3 {
		t.Fatalf("expected exactly 3 docs, got %d", len(docs))
	}
	for _, d := range docs {
		if strings.TrimSpace(d.Markdown) == "" {
			t.Fatalf("empty markdown for %s", d.URL)
		}
		if d.Tier != 1 {
			t.Fatalf("tier = %d", d.Tier)
		}
	}
	snap := timings.Snapshot()
	if snap[observability.StageTier1] <= 0 {
		t.Fatal("tier1 timing not recorded")
	}
}

func TestExtractManyRespectsCancelledContext(t *testing.T) {
	srv := httptest.NewServer(newServerMux())
	defer srv.Close()
	e, _ := newTestExtractor(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	docs := e.ExtractMany(ctx, []Candidate{{URL: srv.URL + "/article"}}, "basic", 3, nil, false)
	if len(docs) != 0 {
		t.Fatalf("expected no docs, got %d", len(docs))
	}
}
