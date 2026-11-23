package api

import (
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	urlpkg "net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/sync/errgroup"

	"github.com/astralabs/tavilyd/internal/cache"
	"github.com/astralabs/tavilyd/internal/config"
	"github.com/astralabs/tavilyd/internal/extract"
	"github.com/astralabs/tavilyd/internal/llm"
	"github.com/astralabs/tavilyd/internal/observability"
	"github.com/astralabs/tavilyd/internal/proxy"
	"github.com/astralabs/tavilyd/internal/rerank"
	"github.com/astralabs/tavilyd/internal/research"
	"github.com/astralabs/tavilyd/internal/search"
	"github.com/astralabs/tavilyd/internal/security"
	"github.com/astralabs/tavilyd/internal/snippet"
	"github.com/astralabs/tavilyd/internal/usage"
)

// ResearchManager is the research job surface used by the API (implemented by
// research.Manager and by stubs in contract tests).
type ResearchManager interface {
	Create(ctx context.Context, job *research.Job) (<-chan research.Event, func(), error)
	Get(ctx context.Context, id string) (*research.Job, error)
	Cancel(id string)
}

// Server holds the handler dependencies.
type Server struct {
	cfg             *config.Config
	log             *slog.Logger
	metrics         *observability.Metrics
	cache           *cache.Cache
	searcher        *search.Client
	searcherProxy   *search.Client
	extractor       *extract.Extractor
	scorer          *rerank.Scorer
	chunker         *rerank.Chunker
	auth            *security.Authenticator
	limiter         *security.RateLimiter
	proxy           *proxy.Engine
	research        ResearchManager
	llm             *llm.Client
	usage           *usage.Store
	mu              sync.Mutex
	engineMu        sync.Mutex
	lastEnginesUsed []string
	lastSuspended   map[string]string
	engineRR        int
	engineCooldown  map[string]time.Time
	llmProbe        *llmHealth
	started         time.Time
}

// Deps is the constructor input for Server.
type Deps struct {
	Config        *config.Config
	Log           *slog.Logger
	Metrics       *observability.Metrics
	Cache         *cache.Cache
	Searcher      *search.Client
	SearcherProxy *search.Client
	Extractor     *extract.Extractor
	Scorer        *rerank.Scorer
	Chunker       *rerank.Chunker
	Auth          *security.Authenticator
	Limiter       *security.RateLimiter
	Proxy         *proxy.Engine
	Research      ResearchManager
	LLM           *llm.Client
	Usage         *usage.Store
}

// NewServer builds the HTTP layer.
func NewServer(d Deps) *Server {
	srv := &Server{
		cfg: d.Config, log: d.Log, metrics: d.Metrics, cache: d.Cache,
		searcher: d.Searcher, searcherProxy: d.SearcherProxy, extractor: d.Extractor, scorer: d.Scorer,
		chunker: d.Chunker, auth: d.Auth, limiter: d.Limiter, proxy: d.Proxy,
		research: d.Research, llm: d.LLM, usage: d.Usage, started: time.Now(),
		engineCooldown: map[string]time.Time{},
	}
	if d.LLM != nil && d.Config != nil {
		srv.llmProbe = newLLMHealth(d.LLM.Probe, d.Config.LLMHealthTimeout, 30*time.Second)
		srv.llmProbe.Start(context.Background())
	}
	return srv
}

// searchPayload is what gets cached for /search (request metadata excluded so
// response_time and request_id stay per-request).
type searchPayload struct {
	Query       string             `json:"query"`
	Results     []SearchResult     `json:"results"`
	Images      []any              `json:"images,omitempty"`
	Diagnostics *SearchDiagnostics `json:"diagnostics,omitempty"`
	Partial     bool               `json:"partial"`
}

// HandleSearch implements POST /search.
func (s *Server) HandleSearch(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	timings := observability.NewTimings()

	var req SearchRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.authenticate(r, req.APIKey); err != nil {
		s.fail(w, r, err)
		return
	}
	v, err := req.Validate()
	if err != nil {
		s.fail(w, r, Errorf(http.StatusBadRequest, "%s", err.Error()))
		return
	}
	quotaCost := int64(1)
	if v.SearchDepth == "advanced" {
		quotaCost = 2
	}
	if !s.enforceQuota(r.Context(), w, r, quotaCost) {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestDeadline)
	defer cancel()

	key := cache.SearchKeyInput{
		Query: v.Query, SearchDepth: v.SearchDepth, MaxResults: v.MaxResults, Topic: v.Topic,
		TimeRange: v.TimeRange, IncludeDomains: v.IncludeDomains, ExcludeDomains: v.ExcludeDomains,
		IncludeRawContent: v.IncludeRawContent.Enabled, IncludeImages: v.IncludeImages,
		ChunksPerSource: v.ChunksPerSource, RawText: v.IncludeRawContent.Text,
		IncludeDomainsMode: v.IncludeDomainsMode, ExactMatch: v.ExactMatch,
		StartDate: v.StartDate, EndDate: v.EndDate, MaxAgeHours: v.MaxAgeHours,
		CacheVersion: s.cfg.CacheVersion,
	}.Key()
	if v.MaxAgeHours != nil {
		ctx = extract.WithMaxAge(ctx, *v.MaxAgeHours)
	}

	// ?nocache=1 (authenticated) bypasses both the search cache and the page
	// cache; used to diagnose why a candidate disappeared.
	nocache := r.URL.Query().Get("nocache") == "1"
	if nocache {
		ctx = extract.WithNoCache(ctx)
	}

	var (
		raw       []byte
		fromCache bool
	)
	if nocache {
		p, cacheable, perr := s.runSearch(ctx, v, timings)
		if perr == nil {
			raw, perr = json.Marshal(p)
		}
		_ = cacheable
		err = perr
	} else {
		raw, fromCache, err = s.cache.DoWith(ctx, key, s.cfg.CacheTTL, func(ctx context.Context) ([]byte, bool, error) {
			p, cacheable, perr := s.runSearch(ctx, v, timings)
			if perr != nil {
				return nil, false, perr
			}
			b, merr := json.Marshal(p)
			if merr != nil {
				return nil, false, merr
			}
			return b, cacheable, nil
		})
	}
	if err != nil {
		w.Header().Set("X-Timing", timings.Header())
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			s.fail(w, r, ErrGatewayTimeout)
			return
		}
		s.fail(w, r, err)
		return
	}
	var payload searchPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		s.fail(w, r, Errorf(http.StatusInternalServerError, "Internal server error"))
		return
	}
	if fromCache {
		timings.Add(observability.StageCache, time.Since(start))
	}
	s.finishTimings(timings)
	if payload.Results == nil {
		payload.Results = []SearchResult{}
	}
	w.Header().Set("X-Timing", timings.Header())
	if payload.Partial {
		w.Header().Set("X-Partial-Results", "true")
	}
	images := payload.Images
	if images == nil {
		images = []any{}
	}
	if r.URL.Query().Get("diagnostics") != "1" {
		payload.Diagnostics = nil
	}
	resp := SearchResponse{
		Query: payload.Query, Answer: nil, Images: images, Results: payload.Results,
		ResponseTime: seconds(start), RequestID: RequestID(r.Context()), Diagnostics: payload.Diagnostics,
	}
	if len(resp.Results) == 0 {
		resp.Results = []SearchResult{}
	}
	credits := 1
	if v.SearchDepth == "advanced" {
		credits = 2
	}
	var outChars int
	for _, res := range resp.Results {
		outChars += len(res.Content)
	}
	s.recordUsage(r.Context(), usage.Delta{
		Credits: int64(credits), TokensIn: approxTokens(v.Query), TokensOut: int64((outChars + 3) / 4),
	})
	if v.IncludeUsage {
		resp.Usage = &Usage{Credits: credits}
	}
	if v.AutoParameters {
		resp.AutoParameters = autoParameters(v)
	}
	if bool(v.IncludeAnswer) {
		if ans, ok := s.generateAnswer(r.Context(), v.Query, resp.Results); ok {
			resp.Answer = &ans
		} else {
			w.Header().Set("X-Answer-Status", "unavailable")
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// noteEngines records engine health for metrics, logs and /health.
func (s *Server) noteEngines(used []string, suspended map[string]string) {
	now := time.Now()
	s.engineMu.Lock()
	prev := s.lastSuspended
	cooldown := s.cfg.SearchEngineCooldown
	for engine := range prev {
		if _, still := suspended[engine]; !still {
			delete(s.engineCooldown, engine)
		}
	}
	for engine := range suspended {
		s.engineCooldown[engine] = now.Add(cooldown)
	}
	s.lastEnginesUsed = append([]string(nil), used...)
	s.lastSuspended = suspended
	s.engineMu.Unlock()
	if s.metrics != nil {
		s.metrics.EnginesUsed.Observe(float64(len(used)))
		for engine := range prev {
			if _, still := suspended[engine]; !still {
				s.metrics.EngineSuspended.WithLabelValues(engine).Set(0)
			}
		}
		for engine, reason := range suspended {
			s.metrics.EngineSuspended.WithLabelValues(engine).Set(1)
			s.log.Warn("search engine suspended", "engine", engine, "reason", reason)
		}
	}
	if len(used) <= 1 {
		s.log.Warn("only one search engine served the query", "engines", used, "suspended", len(suspended))
	}
}

// pickEngines spreads queries over the configured engines, skipping engines in
// our own cooldown window. If every engine is cooling down, the one whose
// cooldown expires first is retried instead of failing discovery.
func (s *Server) pickEngines(now time.Time) []string {
	if s.cfg == nil || len(s.cfg.SearchEngines) == 0 {
		return nil
	}
	s.engineMu.Lock()
	defer s.engineMu.Unlock()
	if s.engineCooldown == nil {
		s.engineCooldown = map[string]time.Time{}
	}
	var healthy []string
	for _, engine := range s.cfg.SearchEngines {
		if until, cooling := s.engineCooldown[engine]; !cooling || now.After(until) {
			healthy = append(healthy, engine)
		}
	}
	if len(healthy) == 0 {
		best, bestUntil := "", time.Time{}
		for _, engine := range s.cfg.SearchEngines {
			until := s.engineCooldown[engine]
			if best == "" || until.Before(bestUntil) {
				best, bestUntil = engine, until
			}
		}
		healthy = []string{best}
	}
	n := s.cfg.SearchEnginesPerQuery
	if n > len(healthy) {
		n = len(healthy)
	}
	picked := make([]string, 0, n)
	start := s.engineRR % len(healthy)
	for i := 0; i < n; i++ {
		picked = append(picked, healthy[(start+i)%len(healthy)])
	}
	s.engineRR = (s.engineRR + n) % len(healthy)
	return picked
}

// discoveryResult is one SearXNG instance's contribution.
type discoveryResult struct {
	results []search.Result
	used    []string
	susp    map[string]string
	err     error
}

// discover queries the direct and proxied SearXNG instances. For basic and
// advanced it queries both in parallel and merges by URL, so one blocked engine
// instance cannot reduce a query to a single engine. fast/ultra-fast stay on
// the primary instance for latency, falling back only when it yields nothing.
func (s *Server) discover(ctx context.Context, primary, fallback *search.Client, sq search.Query, depth string) ([]search.Result, []string, map[string]string, error) {
	if fallback == nil || depth == "fast" || depth == "ultra-fast" {
		found, used, susp, err := primary.SearchMeta(ctx, sq)
		if (err != nil || len(found) == 0) && fallback != nil {
			alt, altUsed, altSusp, aerr := fallback.SearchMeta(ctx, sq)
			if aerr == nil && len(alt) > 0 {
				return alt, altUsed, altSusp, nil
			}
		}
		return found, used, susp, err
	}
	ch := make(chan discoveryResult, 2)
	run := func(c *search.Client) {
		found, used, susp, err := c.SearchMeta(ctx, sq)
		ch <- discoveryResult{results: found, used: used, susp: susp, err: err}
	}
	go run(primary)
	go run(fallback)
	var (
		all     []search.Result
		usedSet = map[string]struct{}{}
		susp    = map[string]string{}
		lastErr error
	)
	for i := 0; i < 2; i++ {
		select {
		case r := <-ch:
			all = append(all, r.results...)
			for _, e := range r.used {
				usedSet[e] = struct{}{}
			}
			for e, reason := range r.susp {
				susp[e] = reason
			}
			if r.err != nil {
				lastErr = r.err
			}
		case <-ctx.Done():
			lastErr = ctx.Err()
		}
	}
	used := make([]string, 0, len(usedSet))
	for e := range usedSet {
		used = append(used, e)
	}
	sort.Strings(used)
	// An engine that answered on one instance is not suspended for this query.
	for _, e := range used {
		delete(susp, e)
	}
	if len(all) == 0 {
		return nil, used, susp, lastErr
	}
	return all, used, susp, nil
}

// runSearch executes discovery → extraction → rerank. It returns cacheable=false
// for partial (deadline-truncated) payloads.
func (s *Server) runSearch(ctx context.Context, v *ValidatedSearch, t *observability.Timings) (*searchPayload, bool, error) {
	if v.MaxResults == 0 {
		return &searchPayload{Query: v.Query, Results: []SearchResult{}}, true, nil
	}
	wanted := v.MaxResults * 2
	if wanted < v.MaxResults+4 {
		wanted = v.MaxResults + 4
	}
	if wanted > 30 {
		wanted = 30
	}
	category := "general"
	if v.Topic == "news" {
		category = "news"
	}
	safeSearch := v.SafeSearch
	if v.SearchDepth == "fast" || v.SearchDepth == "ultra-fast" {
		safeSearch = false
	}
	sq := search.Query{
		Query:      search.BuildQuery(v.Query, v.IncludeDomains),
		Categories: category,
		TimeRange:  v.TimeRange,
		Language:   search.NormalizeLanguage(v.Language, v.Country),
		SafeSearch: safeSearch,
		EngineList: s.pickEngines(time.Now()),
	}
	// fast/ultra-fast prioritise latency: use the direct instance first and the
	// proxied instance only as fallback. basic/advanced keep proxy-first quality.
	primary, fallback := s.searcher, s.searcherProxy
	if (v.SearchDepth == "fast" || v.SearchDepth == "ultra-fast") && fallback != nil {
		primary, fallback = fallback, primary
	}
	start := time.Now()
	found, enginesUsed, suspended, err := s.discover(ctx, primary, fallback, sq, v.SearchDepth)
	s.noteEngines(enginesUsed, suspended)
	t.Add(observability.StageSearch, time.Since(start))
	if err != nil {
		if ctx.Err() != nil {
			return nil, false, ErrGatewayTimeout
		}
		return nil, false, ErrBadGateway
	}

	includeFilter := v.IncludeDomains
	if v.IncludeDomainsMode == "prefer" {
		includeFilter = nil
	}
	found = search.Filter(search.Dedupe(found), includeFilter, v.ExcludeDomains)
	if len(found) > wanted {
		found = found[:wanted]
	}
	if v.SearchDepth == "ultra-fast" {
		results := make([]SearchResult, 0, len(found))
		for i, r := range found {
			if i >= v.MaxResults {
				break
			}
			sr := SearchResult{ID: resultID(r.URL, i), Title: r.Title, URL: r.URL, Content: r.Content, Score: round4(r.Score)}
			if v.IncludePublishedDate || v.Topic == "news" {
				if r.PublishedDate != "" {
					d := r.PublishedDate
					sr.PublishedDate = &d
				}
			}
			if v.IncludeFavicon {
				sr.Favicon = faviconFor(r.URL)
			}
			results = append(results, sr)
		}
		return &searchPayload{Query: v.Query, Results: results}, true, nil
	}

	pubDates := make(map[string]string, len(found))
	cands := make([]extract.Candidate, 0, len(found))
	for _, r := range found {
		pubDates[r.URL] = r.PublishedDate
		cands = append(cands, extract.Candidate{URL: r.URL, Title: r.Title, Snippet: r.Content, Score: r.Score})
	}

	extractDepth := v.SearchDepth
	if extractDepth == "fast" {
		extractDepth = "basic"
	}
	// Score snippet-only candidates while page fetches run; snippets are short
	// and one batched TEI call overlaps the tier-1/2 work instead of adding to it.
	snippetCh := make(chan map[string]float64, 1)
	go func() { snippetCh <- s.preScoreSnippets(ctx, v.Query, cands, v.SearchDepth) }()

	// Fetch the extra pool lazily: only max_results+2 candidates up front, the
	// rest are used for backfill if the floor leaves us short.
	initial := cands
	if len(initial) > v.MaxResults+2 {
		initial = initial[:v.MaxResults+2]
	}
	var extractStats extract.ExtractStats
	docs := []extract.Document{}
	docs, extractStats = s.extractor.ExtractManyWithStats(ctx, initial, extractDepth, v.MaxResults, t, v.IncludeImages)
	_ = docs
	partial := ctx.Err() != nil
	if len(docs) == 0 {
		if partial {
			return nil, false, ErrGatewayTimeout
		}
		// No page could be extracted: degrade to discovery snippets rather than
		// returning an empty result set.
		fallback := make([]SearchResult, 0, min(v.MaxResults, len(found)))
		for i, r := range found {
			if i >= v.MaxResults {
				break
			}
			fallback = append(fallback, SearchResult{Title: r.Title, URL: r.URL, Content: r.Content, Score: 0.1})
		}
		return &searchPayload{Query: v.Query, Results: fallback, Partial: partial}, false, nil
	}

	var snippetScores map[string]float64
	select {
	case snippetScores = <-snippetCh:
	case <-ctx.Done():
	}
	results := s.rank(ctx, v, docs, pubDates, t, snippetScores)
	diag := &SearchDiagnostics{Candidates: len(cands), FetchedOK: extractStats.Good, SnippetOnly: extractStats.SnippetOnly,
		Timeout: extractStats.Timeout, Failed: extractStats.Failed, TooShort: extractStats.TooShort}
	preFloor := len(results)
	if v.IncludeDomainsMode == "prefer" && len(v.IncludeDomains) > 0 {
		for i := range results {
			if hostMatchesAny(results[i].URL, v.IncludeDomains) {
				results[i].Score = math.Min(0.999, results[i].Score+0.05)
			}
		}
		sort.SliceStable(results, func(a, b int) bool { return results[a].Score > results[b].Score })
	}
	if v.ExactMatch {
		results = filterExactMatch(results, v.Query)
	}
	results = applyScoreFloor(results, s.cfg.MinResultScore, s.cfg.MinRelativeScore, s.cfg.SnippetMinRelativeScore)
	diag.BelowFloor += preFloor - len(results)

	// Relevance floor can leave fewer results than requested. Backfill from the
	// unused candidates while the deadline allows it, never padding with junk.
	if len(results) < v.MaxResults && remaining(ctx) > 900*time.Millisecond {
		used := make(map[string]struct{}, len(docs))
		for _, d := range docs {
			used[d.URL] = struct{}{}
		}
		unused := make([]extract.Candidate, 0, len(cands))
		for _, c := range cands {
			if _, ok := used[c.URL]; !ok {
				unused = append(unused, c)
			}
		}
		if len(unused) > 0 {
			need := v.MaxResults - len(results)
			if extraDocs := s.extractor.ExtractMany(ctx, unused, v.SearchDepth, need, t, v.IncludeImages); len(extraDocs) > 0 {
				extra := s.rank(ctx, v, extraDocs, pubDates, t, snippetScores)
				results = mergeResults(results, extra)
				pre := len(results)
				results = applyScoreFloor(results, s.cfg.MinResultScore, s.cfg.MinRelativeScore, s.cfg.SnippetMinRelativeScore)
				diag.BelowFloor += pre - len(results)
			}
		}
	}
	if v.FilterByPublishedDate {
		results = filterPublishedDate(results, v.StartDate, v.EndDate)
	}
	if len(results) > v.MaxResults {
		results = results[:v.MaxResults]
	}
	var images []any
	if v.IncludeImages {
		images = s.fetchImageResults(ctx, v)
	}
	diag.Final = len(results)
	return &searchPayload{Query: v.Query, Results: results, Images: images, Diagnostics: diag, Partial: partial}, !partial, nil
}

type chunkRef struct {
	doc        int
	text       string
	start, end int
}

// scoringChars caps the text sent to the cross-encoder per chunk.
const scoringChars = 900

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// rank chunks the documents, scores the chunks, and assembles the final top-N.
// preScoreSnippets scores the discovery snippets in one batch. It is launched
// before extraction so the cross-encoder call overlaps page fetching.
func (s *Server) preScoreSnippets(ctx context.Context, query string, cands []extract.Candidate, depth string) map[string]float64 {
	out := make(map[string]float64, len(cands))
	urls := make([]string, 0, len(cands))
	texts := make([]string, 0, len(cands))
	for _, c := range cands {
		txt := strings.TrimSpace(c.Snippet)
		if txt == "" {
			continue
		}
		urls = append(urls, c.URL)
		texts = append(texts, txt)
	}
	if len(texts) == 0 {
		return out
	}
	scores := s.scorer.Lexical(query, texts)
	if depth != "fast" {
		if ce, err := s.scorer.Rerank(ctx, query, texts); err == nil && len(ce) == len(texts) {
			scores = ce
		}
	}
	for i, u := range urls {
		if i < len(scores) {
			out[u] = scores[i]
		}
	}
	return out
}

func (s *Server) rank(ctx context.Context, v *ValidatedSearch, docs []extract.Document, pubDates map[string]string, t *observability.Timings, preSnippets map[string]float64) []SearchResult {
	var refs []chunkRef
	for i, d := range docs {
		if d.SnippetOnly {
			continue
		}
		if len(refs) >= s.cfg.MaxChunksTotal {
			break
		}
		for _, ch := range s.chunker.Split(d.Markdown) {
			if len(refs) >= s.cfg.MaxChunksTotal {
				break
			}
			if !s.extractor.KeepChunk(ch.Text) {
				continue
			}
			refs = append(refs, chunkRef{doc: i, text: ch.Text, start: ch.Start, end: ch.End})
		}
	}
	if len(refs) == 0 {
		return nil
	}

	texts := make([]string, len(refs))
	for i, rf := range refs {
		texts[i] = rf.text
	}

	// Cheap lexical pre-filter: a cross-encoder pass over every chunk of every
	// page is the dominant cost on CPU rerankers. Keep the best few chunks per
	// document (topN+1, so the reranker still has a choice) and send only those.
	lexical := s.scorer.Lexical(v.Query, texts)
	selected := preselect(lexical, refs, len(docs), v.SearchDepth)
	selectedTexts := make([]string, len(selected))
	for i, ci := range selected {
		// Score only the opening of each chunk: cross-encoder cost grows roughly
		// linearly with tokens, and the query signal is concentrated early. The
		// full chunk is still what gets returned as content.
		selectedTexts[i] = truncateRunes(refs[ci].text, scoringChars)
	}

	start := time.Now()
	var picked []float64
	if v.SearchDepth == "fast" {
		picked = make([]float64, len(selected))
		lex := s.scorer.Lexical(v.Query, selectedTexts)
		copy(picked, lex)
	} else if remaining(ctx) < 500*time.Millisecond {
		picked = make([]float64, len(selected))
		for i, ci := range selected {
			picked[i] = lexical[ci]
		}
	} else {
		var err error
		picked, err = s.scorer.Rerank(ctx, v.Query, selectedTexts)
		if err != nil {
			picked = make([]float64, len(selected))
			for i, ci := range selected {
				picked[i] = lexical[ci]
			}
		}
	}
	t.Add(observability.StageRerank, time.Since(start))

	scores := make([]float64, len(refs))
	for i, ci := range selected {
		if i < len(picked) {
			scores[ci] = picked[i]
		}
	}

	byDoc := make([][]int, len(docs))
	for ci, rf := range refs {
		byDoc[rf.doc] = append(byDoc[rf.doc], ci)
	}
	topN := v.ChunksPerSource
	if topN < 1 {
		topN = 1
	}

	type scoredDoc struct {
		doc     extract.Document
		score   float64
		content string
	}
	perChunk := 500
	if v.SearchDepth == "advanced" {
		perChunk = 800
	}
	limit := perChunk * v.ChunksPerSource
	ranked := make([]scoredDoc, 0, len(docs))
	for i, d := range docs {
		idxs := byDoc[i]
		if len(idxs) == 0 {
			continue
		}
		sort.SliceStable(idxs, func(a, b int) bool { return scores[idxs[a]] > scores[idxs[b]] })
		pick := idxs
		if len(pick) > topN {
			pick = pick[:topN]
		}
		sort.Ints(pick)
		spans := make([]snippet.Span, 0, len(pick))
		best := 0.0
		for _, ci := range pick {
			spans = append(spans, snippet.Span{Start: refs[ci].start, End: refs[ci].end})
			if scores[ci] > best {
				best = scores[ci]
			}
		}
		content := snippet.Assemble(d.Markdown, spans, limit)
		content = strings.ReplaceAll(content, "\n\n[...]\n\n", " [...] ")
		ranked = append(ranked, scoredDoc{doc: d, score: best, content: content})
	}
	var snippetDocs []extract.Document
	for _, d := range docs {
		if d.SnippetOnly {
			snippetDocs = append(snippetDocs, d)
		}
	}
	if len(snippetDocs) > 0 {
		scores := make([]float64, len(snippetDocs))
		var missing []int
		for i, d := range snippetDocs {
			if sc, ok := preSnippets[d.URL]; ok {
				scores[i] = sc
			} else {
				missing = append(missing, i)
			}
		}
		if len(missing) > 0 {
			texts := make([]string, len(missing))
			for k, idx := range missing {
				texts[k] = strings.TrimSpace(snippetDocs[idx].Snippet)
			}
			fresh := s.scorer.Lexical(v.Query, texts)
			if v.SearchDepth != "fast" {
				if ce, err := s.scorer.Rerank(ctx, v.Query, texts); err == nil && len(ce) == len(texts) {
					fresh = ce
				}
			}
			for k, idx := range missing {
				if k < len(fresh) {
					scores[idx] = fresh[k]
				}
			}
		}
		for i, d := range snippetDocs {
			ranked = append(ranked, scoredDoc{doc: d, score: scores[i], content: strings.TrimSpace(d.Snippet)})
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].score > ranked[j].score })
	if len(ranked) > v.MaxResults {
		ranked = ranked[:v.MaxResults]
	}

	out := make([]SearchResult, 0, len(ranked))
	for i, sd := range ranked {
		sr := SearchResult{
			ID:          resultID(sd.doc.URL, i),
			SnippetOnly: sd.doc.SnippetOnly,
			Title:       sd.doc.Title,
			URL:         sd.doc.URL,
			Content:     sd.content,
			Score:       round4(sd.score),
		}
		if v.IncludePublishedDate || v.Topic == "news" || v.FilterByPublishedDate {
			d := pubDates[sd.doc.URL]
			if d == "" {
				d = sd.doc.PublishedDate
			}
			if d != "" {
				sr.PublishedDate = &d
			}
		}
		if v.IncludeFavicon {
			sr.Favicon = faviconFor(sd.doc.URL)
		}
		if v.IncludeImages && len(sd.doc.Images) > 0 {
			sr.Images = sd.doc.Images
		}
		if v.IncludeRawContent.Enabled {
			body := sd.doc.Markdown
			if v.IncludeRawContent.Text && strings.TrimSpace(sd.doc.Text) != "" {
				body = sd.doc.Text
			}
			rc := truncateAtParagraph(body, s.cfg.RawContentMaxChars)
			sr.RawContent = &rc
		}
		out = append(out, sr)
	}
	return out
}

// resultID builds a short stable id for a result within one response.
func resultID(url string, idx int) string {
	sum := sha1.Sum([]byte(url))
	return fmt.Sprintf("%x-%02d", sum[:3], idx+1)
}

// faviconFor derives the conventional favicon URL for a result host.
func faviconFor(raw string) *string {
	u, err := urlpkg.Parse(raw)
	if err != nil || u.Host == "" {
		return nil
	}
	f := u.Scheme + "://" + u.Host + "/favicon.ico"
	return &f
}

// applyScoreFloor drops results below an absolute and a relative cutoff, while
// always keeping the single best result.
func applyScoreFloor(results []SearchResult, minAbs, minRel, snippetRel float64) []SearchResult {
	if len(results) == 0 {
		return results
	}
	top := results[0].Score
	out := results[:0]
	for i, r := range results {
		if i == 0 {
			out = append(out, r)
			continue
		}
		if r.SnippetOnly {
			if r.Score >= top*snippetRel {
				out = append(out, r)
			}
			continue
		}
		if r.Score >= minAbs && r.Score >= top*minRel {
			out = append(out, r)
		}
	}
	return out
}

// mergeResults unions two ranked result sets by URL (keeping the higher score)
// and sorts by score descending.
func mergeResults(a, b []SearchResult) []SearchResult {
	seen := make(map[string]int, len(a)+len(b))
	out := make([]SearchResult, 0, len(a)+len(b))
	for _, r := range append(append([]SearchResult{}, a...), b...) {
		if idx, ok := seen[r.URL]; ok {
			if r.Score > out[idx].Score {
				out[idx] = r
			}
			continue
		}
		seen[r.URL] = len(out)
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}

// truncateAtParagraph caps s at max bytes on a paragraph boundary and marks the
// cut with our own "..." so it is never confused with page text.
func truncateAtParagraph(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	cut := max
	if idx := strings.LastIndex(s[:cut], "\n\n"); idx > max/2 {
		cut = idx
	} else if idx := strings.LastIndexAny(s[:cut], " \n"); idx > max/2 {
		cut = idx
	}
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimSpace(s[:cut]) + "\n\n..."
}

// chunkedContent reranks a page's chunks against the extract query and joins
// the best N in document order, matching Tavily's chunks_per_source output.
func (s *Server) chunkedContent(ctx context.Context, markdown, query string, n int) string {
	if n < 1 {
		n = 1
	}
	chunks := s.chunker.Split(markdown)
	if len(chunks) <= n {
		return strings.TrimSpace(markdown)
	}
	texts := make([]string, len(chunks))
	for i, ch := range chunks {
		texts[i] = ch.Text
	}
	scores, err := s.scorer.Rerank(ctx, query, texts)
	if err != nil || len(scores) != len(chunks) {
		scores = s.scorer.Lexical(query, texts)
	}
	type pair struct {
		idx   int
		score float64
	}
	ranked := make([]pair, len(chunks))
	for i := range chunks {
		ranked[i] = pair{idx: i, score: scores[i]}
	}
	sort.SliceStable(ranked, func(a, b int) bool { return ranked[a].score > ranked[b].score })
	if len(ranked) > n {
		ranked = ranked[:n]
	}
	sort.Slice(ranked, func(a, b int) bool { return ranked[a].idx < ranked[b].idx })
	parts := make([]string, 0, len(ranked))
	for _, p := range ranked {
		parts = append(parts, chunks[p.idx].Text)
	}
	return strings.Join(parts, " [...] ")
}

// fetchImageResults asks SearXNG for the image category; descriptions use the
// image result title. Failures are non-fatal.
func (s *Server) fetchImageResults(ctx context.Context, v *ValidatedSearch) []any {
	ictx, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
	defer cancel()
	res, err := s.searcher.Search(ictx, search.Query{Query: v.Query, Categories: "images", Language: search.NormalizeLanguage(v.Language, v.Country)})
	if err != nil {
		return []any{}
	}
	out := make([]any, 0, 5)
	seen := map[string]struct{}{}
	for _, r := range res {
		u := r.ImgSrc
		if u == "" {
			u = r.Thumbnail
		}
		if u == "" {
			continue
		}
		if _, dup := seen[u]; dup {
			continue
		}
		seen[u] = struct{}{}
		if v.IncludeImageDescriptions {
			out = append(out, map[string]any{"url": u, "description": r.Title})
		} else {
			out = append(out, u)
		}
		if len(out) >= 5 {
			break
		}
	}
	return out
}

// filterPublishedDate drops undated results when a date filter is active and
// enforces start_date/end_date when parseable.
func filterPublishedDate(results []SearchResult, start, end string) []SearchResult {
	var from, to time.Time
	if start != "" {
		from, _ = time.Parse("2006-01-02", start)
	}
	if end != "" {
		to, _ = time.Parse("2006-01-02", end)
	}
	out := results[:0]
	for _, r := range results {
		if r.PublishedDate == nil || *r.PublishedDate == "" {
			continue
		}
		d := parseAnyDate(*r.PublishedDate)
		if d.IsZero() {
			continue
		}
		if !from.IsZero() && d.Before(from) {
			continue
		}
		if !to.IsZero() && d.After(to.Add(24*time.Hour)) {
			continue
		}
		out = append(out, r)
	}
	return out
}

func parseAnyDate(s string) time.Time {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05Z07:00", "2006-01-02 15:04:05", "2006-01-02", time.RFC1123, time.RFC1123Z, "Mon, 02 Jan 2006 15:04:05 GMT"} {
		if t, err := time.Parse(layout, strings.TrimSpace(s)); err == nil {
			return t
		}
	}
	return time.Time{}
}

// approxTokens is the documented 4-chars-per-token estimate used only for
// usage accounting, never for truncation decisions.
func approxTokens(s string) int64 {
	return int64((utf8.RuneCountInString(s) + 3) / 4)
}

// recordUsage writes counters for the current key. It never blocks a response;
// Redis errors are intentionally ignored (usage is best-effort metadata).
func (s *Server) recordUsage(ctx context.Context, d usage.Delta) {
	if s.usage == nil {
		return
	}
	_ = s.usage.Record(ctx, APIKeyFromContext(ctx), d)
}

// generateAnswer asks the configured LLM for a short answer grounded in the
// retrieved snippets. It never fails the search.
func (s *Server) generateAnswer(ctx context.Context, query string, results []SearchResult) (string, bool) {
	if s.llm == nil || len(results) == 0 {
		return "", false
	}
	var b strings.Builder
	for i, r := range results {
		if i >= 5 {
			break
		}
		fmt.Fprintf(&b, "[%d] %s\n%s\n\n", i+1, r.URL, truncateAtParagraph(r.Content, 800))
	}
	msgs := []llm.Message{{Role: "user", Content: "Answer the question in 2-4 sentences using ONLY these sources. If they do not contain the answer, say so.\n\nQuestion: " + query + "\n\nSources:\n" + b.String()}}
	cctx, cancel := context.WithTimeout(ctx, s.cfg.LLMAnswerTimeout)
	defer cancel()
	noThink := false
	ans, _, err := s.llm.Chat(cctx, msgs, llm.Options{Thinking: &noThink, Temperature: 0.2, MaxTokens: 300})
	if err != nil || strings.TrimSpace(ans) == "" {
		return "", false
	}
	return ans, true
}

// autoParameters applies simple heuristics and reports the chosen values.
func autoParameters(v *ValidatedSearch) map[string]any {
	topic, depth, tr := v.Topic, "basic", v.TimeRange
	if v.Topic == "" {
		topic = "general"
	}
	if v.ExactMatch || len(v.Query) > 60 {
		depth = "advanced"
	}
	if tr == "" {
		l := strings.ToLower(v.Query)
		if strings.Contains(l, "latest") || strings.Contains(l, "today") || strings.Contains(l, "news") {
			tr = "week"
		}
	}
	return map[string]any{"search_depth": depth, "topic": topic, "time_range": tr}
}

func hostMatchesAny(raw string, domains []string) bool {
	u, err := urlpkg.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	for _, d := range domains {
		d = strings.TrimPrefix(strings.ToLower(d), "www.")
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

var reQuoted = regexp.MustCompile(`"([^"]{2,})"`)

func filterExactMatch(results []SearchResult, query string) []SearchResult {
	phrases := reQuoted.FindAllStringSubmatch(query, -1)
	if len(phrases) == 0 {
		return results
	}
	out := results[:0]
	for _, r := range results {
		content := strings.ToLower(r.Content)
		ok := true
		for _, m := range phrases {
			if !strings.Contains(content, strings.ToLower(m[1])) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, r)
		}
	}
	return out
}

// preselect returns the indices of the chunks worth sending to the reranker.
// basic keeps one chunk per document (the cross-encoder only has to order the
// documents, and the shown content is that chunk); advanced keeps three so the
// answer can be assembled from the best passages.
func preselect(lexical []float64, refs []chunkRef, docs int, depth string) []int {
	k := 1
	if depth == "advanced" {
		k = 3
	}
	perDoc := make([][]int, docs)
	for i, rf := range refs {
		perDoc[rf.doc] = append(perDoc[rf.doc], i)
	}
	out := make([]int, 0, len(refs))
	for _, idxs := range perDoc {
		sort.SliceStable(idxs, func(a, b int) bool { return lexical[idxs[a]] > lexical[idxs[b]] })
		if len(idxs) > k {
			idxs = idxs[:k]
		}
		sort.Ints(idxs)
		out = append(out, idxs...)
	}
	sort.Ints(out)
	return out
}

// HandleExtract implements POST /extract.
func (s *Server) HandleExtract(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	timings := observability.NewTimings()

	var req ExtractRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.authenticate(r, req.APIKey); err != nil {
		s.fail(w, r, err)
		return
	}
	v, err := req.Validate()
	if err != nil {
		s.fail(w, r, Errorf(http.StatusBadRequest, "%s", err.Error()))
		return
	}
	quotaCost := int64((len(v.URLs) + 4) / 5)
	if v.ExtractDepth == "advanced" {
		quotaCost *= 2
	}
	if !s.enforceQuota(r.Context(), w, r, quotaCost) {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(v.Timeout*float64(time.Second)))
	defer cancel()
	if r.URL.Query().Get("nocache") == "1" {
		ctx = extract.WithNoCache(ctx)
	}

	out := make([]ExtractResult, len(v.URLs))
	failed := make([]FailedResult, 0)
	errs := make([]error, len(v.URLs))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(max(1, s.cfg.FetchConcurrency))
	for i, u := range v.URLs {
		i, u := i, u
		g.Go(func() error {
			pageStart := time.Now()
			art, tier, ferr := s.extractor.FetchPage(gctx, u, v.ExtractDepth, v.IncludeImages)
			if tier == 2 {
				timings.Add(observability.StageTier2, time.Since(pageStart))
			} else {
				timings.Add(observability.StageTier1, time.Since(pageStart))
			}
			if ferr != nil {
				errs[i] = ferr
				return nil
			}
			content := strings.TrimSpace(art.Markdown)
			if v.Format == "text" {
				content = strings.TrimSpace(art.Text)
				if content == "" {
					content = art.Markdown
				}
			}
			if v.Query != "" && v.Format == "markdown" {
				content = s.chunkedContent(ctx, art.Markdown, v.Query, v.ChunksPerSource)
			}
			res := ExtractResult{URL: u, RawContent: truncateAtParagraph(content, s.cfg.RawContentMaxChars)}
			if v.IncludeFavicon {
				res.Favicon = faviconFor(u)
			}
			if v.IncludeImages {
				res.Images = art.Images
			}
			out[i] = res
			return nil
		})
	}
	_ = g.Wait()

	results := make([]ExtractResult, 0, len(out))
	for i, res := range out {
		if res.URL == "" {
			reason := "extraction failed"
			if errs[i] != nil {
				reason = publicError(errs[i])
			}
			failed = append(failed, FailedResult{URL: v.URLs[i], Error: reason})
			continue
		}
		results = append(results, res)
	}
	if failed == nil {
		failed = []FailedResult{}
	}
	s.finishTimings(timings)
	w.Header().Set("X-Timing", timings.Header())
	resp := ExtractResponse{
		Results: results, FailedResults: failed,
		ResponseTime: seconds(start), RequestID: RequestID(r.Context()),
	}
	credits := (len(results) + 4) / 5
	if v.ExtractDepth == "advanced" {
		credits *= 2
	}
	var outChars int
	for _, res := range results {
		outChars += len(res.RawContent)
	}
	s.recordUsage(r.Context(), usage.Delta{
		Credits: int64(credits), TokensIn: approxTokens(v.Query), TokensOut: int64((outChars + 3) / 4),
	})
	if v.IncludeUsage {
		resp.Usage = &Usage{Credits: credits}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	writeError(w, r, err)
}

func (s *Server) finishTimings(t *observability.Timings) {
	snap := t.Snapshot()
	for stage, d := range snap {
		s.metrics.ObserveStage(stage, d)
	}
}

func remaining(ctx context.Context) time.Duration {
	if dl, ok := ctx.Deadline(); ok {
		return time.Until(dl)
	}
	return time.Hour
}

func seconds(start time.Time) float64 { return math.Round(time.Since(start).Seconds()*1000) / 1000 }

func round4(v float64) float64 { return math.Round(v*10000) / 10000 }

// publicError sanitizes internal failures for the failed_results array.
func publicError(err error) string {
	if err == nil {
		return "extraction failed"
	}
	if errors.Is(err, security.ErrBlockedAddress) {
		return "URL blocked by security policy"
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "request timed out"
	}
	msg := err.Error()
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return msg
}
