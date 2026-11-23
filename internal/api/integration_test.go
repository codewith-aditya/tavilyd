package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/astralabs/tavilyd/internal/cache"
	"github.com/astralabs/tavilyd/internal/config"
	"github.com/astralabs/tavilyd/internal/extract"
	"github.com/astralabs/tavilyd/internal/observability"
	"github.com/astralabs/tavilyd/internal/rerank"
	"github.com/astralabs/tavilyd/internal/search"
	"github.com/astralabs/tavilyd/internal/security"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreTopFunction("internal/poll.runtime_pollWait"),
		goleak.IgnoreTopFunction("net/http.(*persistConn).readLoop"),
		goleak.IgnoreTopFunction("net/http.(*persistConn).writeLoop"),
	)
}

const testKey = "test-key"

// siteServer serves three article-like pages.
func siteServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for i := 0; i < 3; i++ {
		i := i
		mux.HandleFunc(fmt.Sprintf("/post%d", i), func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			body := fmt.Sprintf(`<html><head><title>Post %d</title></head><body>
<article><h1>Post %d</h1>
<p>This article number %d explains how distributed search backends combine discovery, extraction and reranking. It has enough text to survive readability and to produce several chunks for the cross encoder, because real pages are longer than one sentence and we want realistic chunk boundaries here.</p>
<p>Vector retrieval and lexical retrieval fail in different ways. Hybrid pipelines merge candidate sets, deduplicate by canonical URL and then score passages with a cross encoder such as bge-reranker-v2-m3 served by text embeddings inference.</p>
<p>Operational concerns include bounded concurrency, per-page timeouts, cache invalidation and graceful degradation when the GPU sidecar disappears.</p>
</article></body></html>`, i, i, i)
			_, _ = io.WriteString(w, body)
		})
	}
	return httptest.NewServer(mux)
}

// searxServer fakes SearXNG's JSON API.
func searxServer(t *testing.T, results []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		res := make([]map[string]any, 0, len(results))
		for i, u := range results {
			res = append(res, map[string]any{
				"url": u, "title": fmt.Sprintf("Result %d", i), "content": fmt.Sprintf("Snippet %d about distributed search", i),
				"engine": "google", "score": 0.9 - float64(i)*0.05,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"query": r.URL.Query().Get("q"), "results": res})
	}))
}

// teiServer fakes TEI's /rerank: score = term overlap ratio.
func teiServer(t *testing.T) *httptest.Server {
	t.Helper()
	type rerankReq struct {
		Query string   `json:"query"`
		Texts []string `json:"texts"`
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		var req rerankReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		out := make([]map[string]any, 0, len(req.Texts))
		for i, text := range req.Texts {
			score := float64(len(strings.Fields(strings.ToLower(text)))) / 1000.0
			if strings.Contains(strings.ToLower(text), strings.ToLower(req.Query)) {
				score += 0.5
			}
			out = append(out, map[string]any{"index": i, "score": score})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
}

type testEnv struct {
	srv     *Server
	server  *httptest.Server
	site    *httptest.Server
	searx   *httptest.Server
	tei     *httptest.Server
	cache   *cache.Cache
	htmlURL []string
}

func newTestEnv(t *testing.T, overrides map[string]string) *testEnv {
	t.Helper()
	site := siteServer(t)
	urls := []string{site.URL + "/post0", site.URL + "/post1", site.URL + "/post2"}
	searx := searxServer(t, urls)
	tei := teiServer(t)

	env := map[string]string{
		"HTTP_ADDR":           ":0",
		"SEARXNG_URL":         searx.URL,
		"TEI_URL":             tei.URL,
		"REDIS_URL":           "redis://127.0.0.1:1/0",
		"REDIS_TIMEOUT":       "20ms",
		"BROWSER_URL":         "off",
		"API_KEY":             testKey,
		"ALLOW_PRIVATE_IPS":   "false",
		"SSRF_ALLOWLIST":      "127.0.0.1/32,::1/128",
		"LRU_ENTRIES":         "200",
		"RATE_LIMIT_RPS":      "0",
		"LOG_LEVEL":           "error",
		"REQUEST_DEADLINE":    "6s",
		"FETCH_BASIC_TIMEOUT": "2s",
		"MAX_CHUNKS_PER_PAGE": "8",
		"MAX_CHUNKS_TOTAL":    "60",
	}
	for k, v := range overrides {
		env[k] = v
	}
	cfg, err := config.LoadFrom(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	metrics := observability.NewMetrics()
	c, err := cache.New(cfg, log, metrics)
	if err != nil {
		t.Fatalf("cache: %v", err)
	}
	guard := security.NewGuard(cfg.AllowPrivateIPs, 2*time.Second)
	if err := guard.SetAllowlist(cfg.SSRFAllowlist); err != nil {
		t.Fatalf("allowlist: %v", err)
	}
	searcher := search.NewClient(cfg, nil, log, metrics)
	extractor := extract.NewExtractor(cfg, guard, c, context.Background(), log, metrics)
	teiClient := rerank.NewTEIClient(cfg, nil)
	scorer := rerank.NewScorer(teiClient, log, metrics)
	chunker := rerank.NewChunker(rerank.ChunkerConfig{
		Tokens: cfg.ChunkTokens, Overlap: cfg.ChunkOverlap, MaxPerPage: cfg.MaxChunksPerPage,
		CharsPerToken: cfg.CharsPerToken, Margin: cfg.TokenSafetyMargin,
	})
	srv := NewServer(Deps{
		Config: cfg, Log: log, Metrics: metrics, Cache: c, Searcher: searcher, Extractor: extractor,
		Scorer: scorer, Chunker: chunker, Auth: security.NewAuthenticator(cfg.APIKeys),
		Limiter: security.NewRateLimiter(cfg.RateLimitRPS, cfg.RateLimitBurst, time.Minute),
	})
	ts := httptest.NewServer(srv.Router())
	t.Cleanup(func() {
		ts.Close()
		extractor.Close()
		_ = c.Close()
		searx.Close()
		tei.Close()
		site.Close()
	})
	return &testEnv{srv: srv, server: ts, site: site, searx: searx, tei: tei, cache: c, htmlURL: urls}
}

type httpResult struct {
	status  int
	body    []byte
	header  http.Header
	elapsed time.Duration
}

func (e *testEnv) post(t *testing.T, path string, payload any, key string) httpResult {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, e.server.URL+path, bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return httpResult{status: resp.StatusCode, body: body, header: resp.Header, elapsed: time.Since(start)}
}

func TestSearchBasicContract(t *testing.T) {
	env := newTestEnv(t, nil)
	res := env.post(t, "/search", map[string]any{"query": "distributed search", "max_results": 3}, testKey)
	if res.status != http.StatusOK {
		t.Fatalf("status %d: %s", res.status, res.body)
	}
	var out SearchResponse
	if err := json.Unmarshal(res.body, &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, res.body)
	}
	if out.Query != "distributed search" {
		t.Fatalf("query = %q", out.Query)
	}
	if out.Answer != nil {
		t.Fatal("answer must be null")
	}
	if len(out.Images) != 0 {
		t.Fatal("images must be an empty array")
	}
	if len(out.Results) != 3 {
		t.Fatalf("results = %d, want 3", len(out.Results))
	}
	for _, r := range out.Results {
		if r.URL == "" || r.Title == "" || r.Content == "" {
			t.Fatalf("incomplete result: %+v", r)
		}
		if r.Score < 0 || r.Score > 1 {
			t.Fatalf("score out of range: %v", r.Score)
		}
		if r.RawContent != nil {
			t.Fatal("raw_content must be null unless requested")
		}
	}
	if out.RequestID == "" || out.ResponseTime <= 0 {
		t.Fatalf("missing metadata: %+v", out)
	}
	if res.header.Get("X-Timing") == "" {
		t.Fatal("missing X-Timing header")
	}
}

func TestSearchCacheHitUnder30ms(t *testing.T) {
	env := newTestEnv(t, nil)
	payload := map[string]any{"query": "cache me", "max_results": 3}
	first := env.post(t, "/search", payload, testKey)
	if first.status != http.StatusOK {
		t.Fatalf("first: %d %s", first.status, first.body)
	}
	second := env.post(t, "/search", payload, testKey)
	if second.status != http.StatusOK {
		t.Fatalf("second: %d %s", second.status, second.body)
	}
	if second.elapsed > 30*time.Millisecond {
		t.Fatalf("cache hit took %v, want < 30ms (header %s)", second.elapsed, second.header.Get("X-Timing"))
	}
	if !strings.Contains(second.header.Get("X-Timing"), "cache=") {
		t.Fatalf("timing header missing cache stage: %s", second.header.Get("X-Timing"))
	}
	var a, b SearchResponse
	_ = json.Unmarshal(first.body, &a)
	_ = json.Unmarshal(second.body, &b)
	if len(a.Results) != len(b.Results) {
		t.Fatalf("cached response differs: %d vs %d", len(a.Results), len(b.Results))
	}
	if a.RequestID == b.RequestID {
		t.Fatal("request ids must be per-request, not cached")
	}
}

func TestSearchIncludeRawContent(t *testing.T) {
	env := newTestEnv(t, nil)
	res := env.post(t, "/search", map[string]any{"query": "raw", "include_raw_content": true, "max_results": 1}, testKey)
	if res.status != http.StatusOK {
		t.Fatalf("status %d: %s", res.status, res.body)
	}
	var out SearchResponse
	if err := json.Unmarshal(res.body, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 1 || out.Results[0].RawContent == nil || len(*out.Results[0].RawContent) < 100 {
		t.Fatalf("raw content missing: %+v", out.Results)
	}
}

func TestSearchAdvancedDepth(t *testing.T) {
	env := newTestEnv(t, nil)
	res := env.post(t, "/search", map[string]any{"query": "advanced", "search_depth": "advanced", "max_results": 2}, testKey)
	if res.status != http.StatusOK {
		t.Fatalf("status %d: %s", res.status, res.body)
	}
	var out SearchResponse
	if err := json.Unmarshal(res.body, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Results) == 0 {
		t.Fatal("no results")
	}
}

func TestSearchAuthFailures(t *testing.T) {
	env := newTestEnv(t, nil)
	if res := env.post(t, "/search", map[string]any{"query": "x"}, ""); res.status != http.StatusUnauthorized {
		t.Fatalf("missing key => %d", res.status)
	}
	if res := env.post(t, "/search", map[string]any{"query": "x"}, "wrong"); res.status != http.StatusUnauthorized {
		t.Fatalf("wrong key => %d", res.status)
	}
	// Legacy body api_key is accepted.
	if res := env.post(t, "/search", map[string]any{"query": "distributed", "api_key": testKey}, ""); res.status != http.StatusOK {
		t.Fatalf("body api_key => %d %s", res.status, res.body)
	}
	var body struct {
		Detail struct {
			Error string `json:"error"`
		} `json:"detail"`
	}
	res := env.post(t, "/search", map[string]any{"query": "x"}, "wrong")
	if err := json.Unmarshal(res.body, &body); err != nil || body.Detail.Error == "" {
		t.Fatalf("error envelope wrong: %s", res.body)
	}
}

func TestSearchValidationErrors(t *testing.T) {
	env := newTestEnv(t, nil)
	if res := env.post(t, "/search", map[string]any{"query": ""}, testKey); res.status != http.StatusBadRequest {
		t.Fatalf("empty query => %d", res.status)
	}
	if res := env.post(t, "/search", map[string]any{"query": "q", "max_results": 50}, testKey); res.status != http.StatusBadRequest {
		t.Fatalf("max_results => %d", res.status)
	}
	if res := env.post(t, "/search", map[string]any{"query": "q", "search_depth": "turbo"}, testKey); res.status != http.StatusBadRequest {
		t.Fatalf("depth => %d", res.status)
	}
	if res := env.post(t, "/search", map[string]any{"query": "q", "unknown_field": 1}, testKey); res.status != http.StatusOK {
		t.Fatalf("unknown fields must be ignored => %d %s", res.status, res.body)
	}
}

func TestExtractEndpoint(t *testing.T) {
	env := newTestEnv(t, nil)
	res := env.post(t, "/extract", map[string]any{"urls": env.htmlURL[0], "format": "markdown"}, testKey)
	if res.status != http.StatusOK {
		t.Fatalf("status %d: %s", res.status, res.body)
	}
	var out ExtractResponse
	if err := json.Unmarshal(res.body, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 1 || !strings.Contains(out.Results[0].RawContent, "distributed") {
		t.Fatalf("unexpected results: %+v", out)
	}
	if out.FailedResults == nil {
		t.Fatal("failed_results must be an array")
	}

	// List form + a URL that the SSRF guard must refuse.
	blocked := env.post(t, "/extract", map[string]any{
		"urls": []string{env.htmlURL[1], "http://169.254.169.254/latest/meta-data/"},
	}, testKey)
	if blocked.status != http.StatusOK {
		t.Fatalf("status %d: %s", blocked.status, blocked.body)
	}
	var multi ExtractResponse
	if err := json.Unmarshal(blocked.body, &multi); err != nil {
		t.Fatal(err)
	}
	if len(multi.Results) != 1 {
		t.Fatalf("results = %d, want 1", len(multi.Results))
	}
	if len(multi.FailedResults) != 1 || !strings.Contains(multi.FailedResults[0].Error, "security") {
		t.Fatalf("expected blocked url in failed_results: %+v", multi.FailedResults)
	}
}

func TestHealthReportsDependencies(t *testing.T) {
	env := newTestEnv(t, nil)
	resp, err := http.Get(env.server.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var out healthResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	for _, dep := range []string{"redis", "searxng", "reranker", "browser"} {
		st, ok := out.Dependencies[dep]
		if !ok {
			t.Fatalf("missing dependency %q", dep)
		}
		if st.Status != "ok" && st.Status != "degraded" && st.Status != "down" {
			t.Fatalf("bad status for %s: %q", dep, st.Status)
		}
	}
	if out.Dependencies["searxng"].Status != "ok" {
		t.Fatalf("searxng should be ok: %+v", out.Dependencies["searxng"])
	}
	if out.Dependencies["redis"].Status != "down" {
		t.Fatalf("redis should be down in tests: %+v", out.Dependencies["redis"])
	}
	if out.Version == "" {
		t.Fatal("missing version")
	}
}

func TestLiveAndReady(t *testing.T) {
	env := newTestEnv(t, nil)
	for _, path := range []string{"/livez", "/readyz"} {
		resp, err := http.Get(env.server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s => %d", path, resp.StatusCode)
		}
	}
}

func TestRateLimitReturns429(t *testing.T) {
	env := newTestEnv(t, map[string]string{"RATE_LIMIT_RPS": "0.001", "RATE_LIMIT_BURST": "1"})
	payload := map[string]any{"query": "distributed", "max_results": 1}
	if res := env.post(t, "/search", payload, testKey); res.status != http.StatusOK {
		t.Fatalf("first => %d %s", res.status, res.body)
	}
	second := env.post(t, "/search", payload, testKey)
	if second.status != http.StatusTooManyRequests {
		t.Fatalf("second => %d, want 429", second.status)
	}
	if ra := second.header.Get("Retry-After"); ra == "" || ra == "0" {
		t.Fatalf("Retry-After missing: %q", ra)
	}
}

func TestUnknownRouteReturnsJSON404(t *testing.T) {
	env := newTestEnv(t, nil)
	resp, err := http.Get(env.server.URL + "/nope")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var body errorBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.Detail.Error == "" {
		t.Fatalf("expected JSON error envelope, got %v", err)
	}
}
