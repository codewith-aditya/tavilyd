package extract

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"

	"github.com/astralabs/tavilyd/internal/cache"
	"github.com/astralabs/tavilyd/internal/config"
	"github.com/astralabs/tavilyd/internal/observability"
	"github.com/astralabs/tavilyd/internal/proxy"
	"github.com/astralabs/tavilyd/internal/security"
)

// Candidate is one search hit queued for extraction.
type Candidate struct {
	URL     string
	Title   string
	Snippet string
	Score   float64
}

// Extractor orchestrates the two-tier fetch, parsing, caching and streaming.
type Extractor struct {
	cfg     *config.Config
	http    *HTTPFetcher
	browser *Browser
	cache   *cache.Cache
	guard   *security.Guard
	cleaner *Cleaner
	log     *slog.Logger
	metrics *observability.Metrics
}

type noCacheKey struct{}
type maxAgeKey struct{}

// WithMaxAge controls page-cache freshness for one request: 0 forces a live
// fetch, -1 allows cache only, N accepts cache up to N hours old.
func WithMaxAge(ctx context.Context, hours int) context.Context {
	return context.WithValue(ctx, maxAgeKey{}, hours)
}

func maxAgeFrom(ctx context.Context) (int, bool) {
	v, ok := ctx.Value(maxAgeKey{}).(int)
	return v, ok
}

// WithNoCache bypasses the page cache and negative cache for one request. Used
// by the authenticated ?nocache=1 diagnostics path.
func WithNoCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, noCacheKey{}, true)
}

func noCache(ctx context.Context) bool {
	v, _ := ctx.Value(noCacheKey{}).(bool)
	return v
}

type cachedPage struct {
	Title         string    `json:"title"`
	Byline        string    `json:"byline,omitempty"`
	SiteName      string    `json:"site_name,omitempty"`
	Excerpt       string    `json:"excerpt,omitempty"`
	Markdown      string    `json:"markdown"`
	Text          string    `json:"text,omitempty"`
	Images        []string  `json:"images,omitempty"`
	Tier          int       `json:"tier"`
	PublishedDate string    `json:"published_date,omitempty"`
	FetchedAt     time.Time `json:"fetched_at"`
}

type cachedFailure struct {
	Error string `json:"error"`
}

// NewExtractor wires tier 1, tier 2 and the page cache.
func NewExtractor(cfg *config.Config, guard *security.Guard, c *cache.Cache, parent context.Context, log *slog.Logger, m *observability.Metrics) *Extractor {
	cleaner := NewCleaner(CleanConfig{
		Enabled:       cfg.CleanEnabled,
		MaxTokenChars: cfg.CleanMaxTokenChars,
		LinkDensity:   cfg.CleanLinkDensity,
		MinAlpha:      cfg.CleanMinAlpha,
		CTA:           cfg.CleanCTAPhrases,
	})
	return &Extractor{
		cfg:     cfg,
		http:    NewHTTPFetcher(guard, cfg.FetchMaxBytes, cfg.FetchUserAgent, cfg.FetchBasicTimeout, cfg.MaxRedirects, true),
		browser: NewBrowser(parent, cfg, guard, log, m),
		cache:   c,
		guard:   guard,
		cleaner: cleaner,
		log:     log,
		metrics: m,
	}
}

// FetchPage extracts one URL with tier escalation and page caching. The
// returned tier is 0 for cache hits, 1 for HTTP, 2 for browser. includeImages
// controls whether inline image syntax is preserved in the Markdown (the image
// URL list is always collected separately).
func (e *Extractor) FetchPage(ctx context.Context, rawURL, depth string, includeImages bool) (*Article, int, error) {
	if depth != "advanced" {
		depth = "basic"
	}
	variant := "noimg"
	if includeImages {
		variant = "img"
	}
	bypass := noCache(ctx)
	maxAge, hasMaxAge := maxAgeFrom(ctx)
	if hasMaxAge && maxAge == 0 {
		bypass = true
	}
	key := cache.URLKey(rawURL, depth, variant, e.cfg.CacheVersion)
	if b, ok := e.cache.Get(ctx, key); ok && !bypass {
		var cp cachedPage
		if err := json.Unmarshal(b, &cp); err == nil && cacheFresh(cp, maxAge, hasMaxAge) {
			if e.metrics != nil {
				e.metrics.Extractions.WithLabelValues("cache", "hit").Inc()
			}
			return &Article{
				Title: cp.Title, Byline: cp.Byline, SiteName: cp.SiteName, Excerpt: cp.Excerpt,
				Markdown: cp.Markdown, Text: cp.Text, Images: cp.Images, Length: len(cp.Markdown),
				PublishedDate: cp.PublishedDate,
			}, 0, nil
		}
	}
	if hasMaxAge && maxAge < 0 {
		return nil, 0, fmt.Errorf("cache-only mode: %s not cached", rawURL)
	}
	if b, ok := e.cache.Get(ctx, cache.NegativeKey(rawURL, e.cfg.CacheVersion)); ok && !bypass {
		var cf cachedFailure
		_ = json.Unmarshal(b, &cf)
		if cf.Error == "" {
			cf.Error = "previously failed"
		}
		return nil, 0, fmt.Errorf("negative cache: %s", cf.Error)
	}

	started := time.Now()
	art, tier, err := e.fetchAndParse(ctx, rawURL, depth, includeImages)
	e.logCandidate(rawURL, depth, tier, art, err, time.Since(started))
	if err != nil {
		e.storeNegative(ctx, rawURL, err)
		if e.metrics != nil {
			e.metrics.Extractions.WithLabelValues(tierName(tier), "fail").Inc()
		}
		return nil, tier, err
	}
	if !bypass {
		e.store(ctx, key, art, tier)
	}
	return art, tier, nil
}

func (e *Extractor) fetchAndParse(ctx context.Context, rawURL, depth string, includeImages bool) (*Article, int, error) {
	target, err := e.guard.ValidateURL(ctx, rawURL)
	if err != nil {
		return nil, 0, err
	}

	pageTimeout := e.cfg.FetchBasicTimeout
	if e.cfg.ProxyMode != "off" {
		pageTimeout += e.cfg.ProxyFetchExtra
	}
	tier1Ctx, cancel1 := context.WithTimeout(ctx, pageTimeout)
	page, err1 := e.http.Fetch(tier1Ctx, target)
	cancel1()

	var art *Article
	if err1 == nil {
		art, err = ParseHTML(page.Body, page.FinalURL, page.ContentType, ParseOptions{
			Format: "markdown", IncludeImages: includeImages, MaxChars: 1 << 20, Cleaner: e.cleaner,
		})
		if art != nil {
			art.HTTPStatus, art.RawBytes = page.Status, len(page.Body)
		}
		e.recordClean(art)
		if err == nil && !NeedsBrowser(page, art) {
			return art, 1, nil
		}
	}
	if depth != "advanced" {
		if art != nil {
			return art, 1, nil
		}
		return nil, 1, fmt.Errorf("tier1: %w", err1)
	}

	// Tier 2: browser. Bounded separately by the tab pool and its own timeout.
	advancedTimeout := e.cfg.FetchAdvancedTimeout
	if e.cfg.ProxyMode != "off" {
		advancedTimeout += e.cfg.ProxyFetchExtra
	}
	tier2Ctx, cancel2 := context.WithTimeout(ctx, advancedTimeout)
	bpage, err2 := e.browser.Fetch(tier2Ctx, target)
	cancel2()
	if err2 == nil {
		if a2, perr := ParseHTML(bpage.Body, bpage.FinalURL, bpage.ContentType, ParseOptions{
			Format: "markdown", IncludeImages: includeImages, MaxChars: 1 << 20, Cleaner: e.cleaner,
		}); perr == nil && a2.Length > 0 {
			a2.HTTPStatus, a2.RawBytes = bpage.Status, len(bpage.Body)
			e.recordClean(a2)
			return a2, 2, nil
		} else if perr != nil {
			err2 = perr
		}
	}
	if art != nil && art.Length > 0 {
		return art, 1, nil
	}
	if err1 != nil {
		return nil, 2, fmt.Errorf("tier1: %v; tier2: %w", err1, err2)
	}
	return nil, 2, fmt.Errorf("tier2: %w", err2)
}

func cacheFresh(cp cachedPage, maxAge int, has bool) bool {
	if !has {
		return true
	}
	if maxAge < 0 {
		return true
	}
	if maxAge == 0 {
		return false
	}
	if cp.FetchedAt.IsZero() {
		return false
	}
	return time.Since(cp.FetchedAt) <= time.Duration(maxAge)*time.Hour
}

func (e *Extractor) store(ctx context.Context, key string, art *Article, tier int) {
	cp := cachedPage{
		Title: art.Title, Byline: art.Byline, SiteName: art.SiteName, Excerpt: art.Excerpt,
		Markdown: art.Markdown, Text: art.Text, Images: art.Images, Tier: tier,
		PublishedDate: art.PublishedDate, FetchedAt: time.Now().UTC(),
	}
	b, err := json.Marshal(cp)
	if err != nil {
		return
	}
	e.cache.Set(ctx, key, b, e.cfg.ExtractCacheTTL)
}

// logCandidate emits one debug line per fetched candidate with everything the
// quality team needs to explain why a URL disappeared.
func (e *Extractor) logCandidate(url, depth string, tier int, art *Article, err error, took time.Duration) {
	if e.log == nil {
		return
	}
	outcome := "ok"
	switch {
	case err != nil:
		outcome = classifyOutcome(err)
	case art != nil && art.Length == 0:
		outcome = "dropped_by_cleaner"
	case art != nil && art.Length < 40:
		outcome = "too_short"
	case tier == 0:
		outcome = "cache"
	}
	args := []any{
		"url", url, "depth", depth, "tier", tier,
		"fetch_ms", took.Milliseconds(), "outcome", outcome,
	}
	if art != nil {
		args = append(args, "http_status", art.HTTPStatus, "raw_chars", art.RawBytes,
			"cleaned_chars", art.Length, "removed", art.Clean)
	}
	if err != nil {
		args = append(args, "error", err.Error())
	}
	e.log.Debug("candidate", args...)
}

func classifyOutcome(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, security.ErrBlockedAddress):
		return "blocked"
	case errors.Is(err, ErrUnsupportedType), errors.Is(err, ErrNotHTML):
		return "non_html"
	case errors.Is(err, ErrTooLarge):
		return "too_large"
	case strings.Contains(err.Error(), "negative cache"):
		return "cached_failure"
	default:
		return "error"
	}
}

func (e *Extractor) storeNegative(ctx context.Context, rawURL string, cause error) {
	msg := cause.Error()
	if len(msg) > 200 {
		msg = msg[:200]
	}
	b, err := json.Marshal(cachedFailure{Error: msg})
	if err != nil {
		return
	}
	e.cache.Set(ctx, cache.NegativeKey(rawURL, e.cfg.CacheVersion), b, e.cfg.NegativeCacheTTL)
}

// ExtractMany extracts candidates and returns up to want documents.
func (e *Extractor) ExtractMany(ctx context.Context, cands []Candidate, depth string, want int, timings *observability.Timings, includeImages bool) []Document {
	docs, _ := e.ExtractManyWithStats(ctx, cands, depth, want, timings, includeImages)
	return docs
}

// ExtractManyWithStats is ExtractMany plus a stage funnel. Failed or timed-out
// pages fall back to their SearXNG snippet (SnippetOnly) so discovery results
// are never lost.
func (e *Extractor) ExtractManyWithStats(ctx context.Context, cands []Candidate, depth string, want int, timings *observability.Timings, includeImages bool) ([]Document, ExtractStats) {
	stats := ExtractStats{Candidates: len(cands)}
	var statsMu sync.Mutex
	bump := func(field *int) { statsMu.Lock(); *field++; statsMu.Unlock() }
	if want <= 0 || len(cands) == 0 {
		return nil, stats
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	sem := semaphore.NewWeighted(int64(max(1, e.cfg.FetchConcurrency)))
	type item struct {
		doc  Document
		good bool
	}
	results := make(chan item, len(cands))
	g, gctx := errgroup.WithContext(runCtx)
	for _, c := range cands {
		c := c
		g.Go(func() error {
			if err := sem.Acquire(gctx, 1); err != nil {
				return nil
			}
			defer sem.Release(1)
			start := time.Now()
			art, tier, err := e.FetchPage(gctx, c.URL, depth, includeImages)
			if timings != nil {
				if tier == 2 {
					timings.Add(observability.StageTier2, time.Since(start))
				} else {
					timings.Add(observability.StageTier1, time.Since(start))
				}
			}
			if err != nil {
				e.log.Debug("extraction failed", "url", c.URL, "error", err)
				if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
					bump(&stats.Timeout)
				} else {
					bump(&stats.Failed)
				}
				if snippet := strings.TrimSpace(c.Snippet); snippet != "" && e.cfg.SnippetFallback {
					select {
					case results <- item{doc: Document{URL: c.URL, Title: c.Title, Markdown: snippet, Snippet: snippet, Tier: -1, SnippetOnly: true, Took: time.Since(start)}}:
					case <-gctx.Done():
					}
				}
				return nil
			}
			md := strings.TrimSpace(art.Markdown)
			if len(md) < 40 {
				bump(&stats.TooShort)
				if snippet := strings.TrimSpace(c.Snippet); snippet != "" && e.cfg.SnippetFallback {
					select {
					case results <- item{doc: Document{URL: c.URL, Title: firstNonEmpty(art.Title, c.Title), Markdown: snippet, Snippet: snippet, Tier: -1, SnippetOnly: true, Took: time.Since(start)}}:
					case <-gctx.Done():
					}
				}
				return nil
			}
			select {
			case results <- item{good: true, doc: Document{
				URL: c.URL, Title: firstNonEmpty(art.Title, c.Title), Markdown: md, Text: art.Text, Images: art.Images,
				PublishedDate: art.PublishedDate,
				Snippet:       c.Snippet, Tier: tier, Cached: tier == 0, Took: time.Since(start),
			}}:
			case <-gctx.Done():
			}
			return nil
		})
	}
	go func() {
		_ = g.Wait()
		close(results)
	}()

	good := make([]Document, 0, want)
	fallback := make([]Document, 0, len(cands))
	for it := range results {
		if it.good {
			good = append(good, it.doc)
		} else {
			fallback = append(fallback, it.doc)
		}
		// Hedged: return once `want` documents (good or snippet) are in; late
		// pages are allowed only until this point, then stragglers are cancelled.
		if len(good)+len(fallback) >= want {
			cancel()
			for range results {
			}
			break
		}
	}
	// Fill to `want` with snippet-only documents (never drop a discovery hit).
	docs := make([]Document, 0, want)
	docs = append(docs, good...)
	if len(docs) > want {
		docs = docs[:want]
	}
	for _, d := range fallback {
		if len(docs) >= want {
			break
		}
		if strings.TrimSpace(d.Markdown) == "" {
			continue
		}
		docs = append(docs, d)
	}
	stats.Good = len(good)
	stats.SnippetOnly = len(docs) - min(len(good), want)
	return docs, stats
}

// FetchHTML fetches a page as raw HTML for the map/crawl frontier (guarded and
// proxied exactly like extraction).
func (e *Extractor) FetchHTML(ctx context.Context, rawURL string) (string, []byte, string, error) {
	target, err := e.guard.ValidateURL(ctx, rawURL)
	if err != nil {
		return "", nil, "", err
	}
	page, err := e.http.Fetch(ctx, target)
	if err != nil {
		return "", nil, "", err
	}
	return page.FinalURL, page.Body, page.ContentType, nil
}

// SetProxyPolicy wires the outbound proxy policy into tier 1.
func (e *Extractor) SetProxyPolicy(engine *proxy.Engine, retries int, extra ...time.Duration) {
	if e.http != nil {
		e.http.SetProxyPolicy(engine, retries, extra...)
	}
}

// KeepChunk exposes the chunk-level guard so handlers can drop promo chunks
// before they reach the reranker.
func (e *Extractor) KeepChunk(text string) bool { return e.cleaner.KeepChunk(text) }

// recordClean emits the per-rule byte counters and the cleaning latency.
func (e *Extractor) recordClean(art *Article) {
	if art == nil || e.metrics == nil {
		return
	}
	for rule, n := range art.Clean {
		e.metrics.ObserveClean(rule, n)
	}
	e.metrics.ObserveCleanDuration(art.CleanDuration)
}

// Health reports browser pool status.
func (e *Extractor) Health(ctx context.Context) (string, error) {
	return e.browser.Health(ctx)
}

// Close releases the browser allocator.
func (e *Extractor) Close() {
	if e.browser != nil {
		e.browser.Close()
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func tierName(tier int) string {
	switch tier {
	case 0:
		return "cache"
	case 2:
		return "browser"
	default:
		return "http"
	}
}
