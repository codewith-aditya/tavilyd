package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/astralabs/tavilyd/internal/cache"
	"github.com/astralabs/tavilyd/internal/extract"
	"github.com/astralabs/tavilyd/internal/observability"
	"github.com/astralabs/tavilyd/internal/usage"
)

// The /exa/* endpoints implement Exa's public request/response shapes on top of
// the local discovery/extraction/rerank pipeline. They are a compatibility
// facade: results are ours, not Exa's index, and costDollars is always 0.

type exaSearchRequest struct {
	Query              string          `json:"query"`
	Type               string          `json:"type"`
	NumResults         *int            `json:"numResults"`
	Contents           json.RawMessage `json:"contents"`
	IncludeDomains     []string        `json:"includeDomains"`
	ExcludeDomains     []string        `json:"excludeDomains"`
	StartPublishedDate string          `json:"startPublishedDate"`
	EndPublishedDate   string          `json:"endPublishedDate"`
	Category           string          `json:"category"`
}

type exaContentsOptions struct {
	Text       json.RawMessage `json:"text"`
	Highlights json.RawMessage `json:"highlights"`
	Summary    json.RawMessage `json:"summary"`
}

type exaSearchResult struct {
	ID            string   `json:"id"`
	Title         string   `json:"title,omitempty"`
	URL           string   `json:"url"`
	PublishedDate string   `json:"publishedDate,omitempty"`
	Author        string   `json:"author,omitempty"`
	Image         string   `json:"image,omitempty"`
	Favicon       string   `json:"favicon,omitempty"`
	Text          string   `json:"text,omitempty"`
	Highlights    []string `json:"highlights,omitempty"`
	Summary       string   `json:"summary,omitempty"`
}

type exaSearchResponse struct {
	RequestID          string            `json:"requestId"`
	Results            []exaSearchResult `json:"results"`
	SearchTime         float64           `json:"searchTime"`
	ResolvedSearchType string            `json:"resolvedSearchType"`
	CostDollars        map[string]any    `json:"costDollars"`
}

type exaContentsRequest struct {
	IDs        []string        `json:"ids"`
	URLs       []string        `json:"urls"`
	Text       json.RawMessage `json:"text"`
	Highlights json.RawMessage `json:"highlights"`
	Summary    json.RawMessage `json:"summary"`
}

type exaContentsResponse struct {
	RequestID   string            `json:"requestId"`
	Results     []exaSearchResult `json:"results"`
	Statuses    []exaStatus       `json:"statuses,omitempty"`
	CostDollars map[string]any    `json:"costDollars"`
}

type exaStatus struct {
	ID     string    `json:"id"`
	Status string    `json:"status"`
	Error  *exaError `json:"error,omitempty"`
}

type exaError struct {
	Tag     string `json:"tag"`
	Message string `json:"message"`
}

type exaAPIError struct {
	RequestID string `json:"requestId"`
	Error     string `json:"error"`
	Tag       string `json:"tag"`
}

func exaErrorf(w http.ResponseWriter, r *http.Request, status int, tag, msg string) {
	writeJSON(w, status, exaAPIError{RequestID: RequestID(r.Context()), Error: msg, Tag: tag})
}

// wants reports whether an Exa content option is enabled. Exa accepts either a
// boolean or an options object; anything non-null/non-false counts as enabled.
func wants(raw json.RawMessage) bool {
	b := strings.TrimSpace(string(raw))
	if b == "" || b == "null" || b == "false" || b == "0" {
		return false
	}
	return true
}

func exaMaxChars(raw json.RawMessage) int {
	var obj struct {
		MaxCharacters int `json:"maxCharacters"`
	}
	if len(raw) > 0 && raw[0] == '{' {
		_ = json.Unmarshal(raw, &obj)
	}
	if obj.MaxCharacters > 0 {
		return obj.MaxCharacters
	}
	return 0
}

func exaSearchDepth(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "lite":
		return "fast"
	case "deep", "deep-lite":
		return "advanced"
	default:
		return "basic"
	}
}

func (r *exaSearchRequest) validated() (*ValidatedSearch, exaContentsOptions, error) {
	q := strings.TrimSpace(r.Query)
	if q == "" {
		return nil, exaContentsOptions{}, fmt.Errorf("query is required")
	}
	if len([]rune(q)) > 400 {
		return nil, exaContentsOptions{}, fmt.Errorf("query must be at most 400 characters")
	}
	n := 10
	if r.NumResults != nil {
		n = *r.NumResults
	}
	if n < 1 {
		return nil, exaContentsOptions{}, fmt.Errorf("numResults must be at least 1")
	}
	if n > 20 {
		// The local engine caps at 20; Exa allows up to 100. Clamp rather than
		// fail so an Exa client with numResults=50 still gets results.
		n = 20
	}
	var opts exaContentsOptions
	if len(r.Contents) > 0 {
		if err := json.Unmarshal(r.Contents, &opts); err != nil {
			return nil, opts, fmt.Errorf("contents: %w", err)
		}
	}
	v := &ValidatedSearch{
		Query:                q,
		SearchDepth:          exaSearchDepth(r.Type),
		MaxResults:           n,
		ChunksPerSource:      3,
		Topic:                "general",
		IncludeDomains:       r.IncludeDomains,
		ExcludeDomains:       r.ExcludeDomains,
		StartDate:            r.StartPublishedDate,
		EndDate:              r.EndPublishedDate,
		IncludeRawContent:    RawContentMode{Enabled: wants(opts.Text)},
		IncludeImages:        true,
		IncludeFavicon:       true,
		IncludePublishedDate: true,
	}
	return v, opts, nil
}

// HandleExaSearch implements POST /exa/search.
func (s *Server) HandleExaSearch(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	timings := observability.NewTimings()
	var req exaSearchRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.authenticate(r, ""); err != nil {
		exaErrorf(w, r, http.StatusUnauthorized, "INVALID_API_KEY", "missing or invalid API key")
		return
	}
	v, opts, err := req.validated()
	if err != nil {
		exaErrorf(w, r, http.StatusBadRequest, "INVALID_REQUEST_BODY", err.Error())
		return
	}
	// Exa clients often request per-result images; image extraction adds a
	// little page work, so the facade gets a small grace window over the core
	// 6 s search deadline. Tavily's /search keeps the hard 6 s budget.
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestDeadline+2*time.Second)
	defer cancel()
	if r.URL.Query().Get("nocache") == "1" {
		ctx = extract.WithNoCache(ctx)
	}
	key := cache.SearchKeyInput{
		Query: v.Query, SearchDepth: v.SearchDepth, MaxResults: v.MaxResults, Topic: v.Topic,
		TimeRange: v.TimeRange, IncludeDomains: v.IncludeDomains, ExcludeDomains: v.ExcludeDomains,
		IncludeRawContent: v.IncludeRawContent.Enabled, IncludeImages: v.IncludeImages,
		ChunksPerSource: v.ChunksPerSource, RawText: v.IncludeRawContent.Text,
		IncludeDomainsMode: v.IncludeDomainsMode, ExactMatch: v.ExactMatch,
		StartDate: v.StartDate, EndDate: v.EndDate, MaxAgeHours: v.MaxAgeHours,
		CacheVersion: s.cfg.CacheVersion,
	}.Key()

	raw, _, err := s.cache.DoWith(ctx, key, s.cfg.CacheTTL, func(ctx context.Context) ([]byte, bool, error) {
		p, cacheable, perr := s.runSearch(ctx, v, timings)
		if perr != nil {
			return nil, false, perr
		}
		b, merr := json.Marshal(p)
		return b, cacheable, merr
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			exaErrorf(w, r, http.StatusGatewayTimeout, "TIMEOUT", "search timed out")
			return
		}
		exaErrorf(w, r, http.StatusBadGateway, "SEARCH_FAILED", err.Error())
		return
	}
	var payload searchPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		exaErrorf(w, r, http.StatusInternalServerError, "INTERNAL_ERROR", "invalid search payload")
		return
	}
	s.finishTimings(timings)
	w.Header().Set("X-Timing", timings.Header())
	credits := 1
	if v.SearchDepth == "advanced" {
		credits = 2
	}
	var outChars int
	for _, res := range payload.Results {
		outChars += len(res.Content)
	}
	s.recordUsage(r.Context(), usage.Delta{
		Credits: int64(credits), TokensIn: approxTokens(v.Query), TokensOut: int64((outChars + 3) / 4),
	})
	textWanted := wants(opts.Text)
	highlightsWanted := wants(opts.Highlights)
	out := make([]exaSearchResult, 0, len(payload.Results))
	for _, res := range payload.Results {
		item := exaSearchResult{
			ID: res.URL, Title: res.Title, URL: res.URL,
			Favicon: derefString(res.Favicon),
		}
		if res.PublishedDate != nil {
			item.PublishedDate = *res.PublishedDate
		}
		if len(res.Images) > 0 {
			item.Image = res.Images[0]
		}
		if textWanted {
			item.Text = res.Content
			if res.RawContent != nil && *res.RawContent != "" {
				item.Text = *res.RawContent
			}
			if lim := exaMaxChars(opts.Text); lim > 0 && len(item.Text) > lim {
				item.Text = item.Text[:lim]
			}
		}
		if highlightsWanted && strings.TrimSpace(res.Content) != "" {
			item.Highlights = []string{res.Content}
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, exaSearchResponse{
		RequestID:          RequestID(r.Context()),
		Results:            out,
		SearchTime:         float64(time.Since(start).Microseconds()) / 1000,
		ResolvedSearchType: req.Type,
		CostDollars:        map[string]any{"total": 0},
	})
}

// HandleExaContents implements POST /exa/contents.
func (s *Server) HandleExaContents(w http.ResponseWriter, r *http.Request) {
	var req exaContentsRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.authenticate(r, ""); err != nil {
		exaErrorf(w, r, http.StatusUnauthorized, "INVALID_API_KEY", "missing or invalid API key")
		return
	}
	urls := req.IDs
	if len(urls) == 0 {
		urls = req.URLs
	}
	if len(urls) == 0 {
		exaErrorf(w, r, http.StatusBadRequest, "INVALID_REQUEST_BODY", "ids is required")
		return
	}
	if len(urls) > 20 {
		exaErrorf(w, r, http.StatusBadRequest, "INVALID_REQUEST_BODY", "at most 20 ids are allowed")
		return
	}
	if !s.enforceQuota(r.Context(), w, r, int64((len(urls)+4)/5)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestDeadline+2*time.Second)
	defer cancel()
	if r.URL.Query().Get("nocache") == "1" {
		ctx = extract.WithNoCache(ctx)
	}
	textWanted := wants(req.Text)
	highlightsWanted := wants(req.Highlights)
	results := make([]exaSearchResult, len(urls))
	statuses := make([]exaStatus, 0)
	var mu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(max(1, s.cfg.FetchConcurrency))
	for i, u := range urls {
		i, u := i, u
		g.Go(func() error {
			art, _, err := s.extractor.FetchPage(gctx, u, "basic", true)
			if err != nil {
				mu.Lock()
				statuses = append(statuses, exaStatus{ID: u, Status: "error", Error: &exaError{Tag: "CRAWL_FAILED", Message: truncateReason(err.Error())}})
				mu.Unlock()
				return nil
			}
			item := exaSearchResult{ID: u, URL: u, Title: art.Title}
			if textWanted {
				item.Text = art.Markdown
				if lim := exaMaxChars(req.Text); lim > 0 && len(item.Text) > lim {
					item.Text = item.Text[:lim]
				}
			}
			if highlightsWanted {
				item.Highlights = []string{truncateAtParagraph(art.Markdown, 1000)}
			}
			if art.PublishedDate != "" {
				item.PublishedDate = art.PublishedDate
			}
			if len(art.Images) > 0 {
				item.Image = art.Images[0]
			}
			item.Favicon = derefString(faviconFor(u))
			results[i] = item
			return nil
		})
	}
	_ = g.Wait()
	out := results[:0]
	var outChars int
	for _, res := range results {
		if res.URL != "" {
			out = append(out, res)
			outChars += len(res.Text)
		}
	}
	credits := (len(out) + 4) / 5
	s.recordUsage(r.Context(), usage.Delta{Credits: int64(credits), TokensOut: int64((outChars + 3) / 4)})
	writeJSON(w, http.StatusOK, exaContentsResponse{
		RequestID:   RequestID(r.Context()),
		Results:     out,
		Statuses:    statuses,
		CostDollars: map[string]any{"total": 0},
	})
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
