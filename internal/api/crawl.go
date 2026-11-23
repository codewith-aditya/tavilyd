package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"

	"github.com/astralabs/tavilyd/internal/llm"
	"github.com/astralabs/tavilyd/internal/search"
	"github.com/astralabs/tavilyd/internal/usage"
)

// MapRequest is the POST /map body.
type MapRequest struct {
	URL            string   `json:"url"`
	Instructions   string   `json:"instructions"`
	MaxDepth       *int     `json:"max_depth"`
	MaxBreadth     *int     `json:"max_breadth"`
	Limit          *int     `json:"limit"`
	SelectPaths    []string `json:"select_paths"`
	ExcludePaths   []string `json:"exclude_paths"`
	SelectDomains  []string `json:"select_domains"`
	ExcludeDomains []string `json:"exclude_domains"`
	AllowExternal  *bool    `json:"allow_external"`
	Timeout        *int     `json:"timeout"`
	IncludeUsage   bool     `json:"include_usage"`
	APIKey         string   `json:"api_key"`
}

// CrawlRequest is the POST /crawl body (map plus extraction options).
type CrawlRequest struct {
	MapRequest
	ChunksPerSource *int   `json:"chunks_per_source"`
	IncludeImages   bool   `json:"include_images"`
	ExtractDepth    string `json:"extract_depth"`
	Format          string `json:"format"`
	IncludeFavicon  bool   `json:"include_favicon"`
}

// MapResponse is the /map response.
type MapResponse struct {
	BaseURL      string   `json:"base_url"`
	Results      []string `json:"results"`
	ResponseTime float64  `json:"response_time"`
	Usage        *Usage   `json:"usage,omitempty"`
	RequestID    string   `json:"request_id"`
}

// CrawlResult is one crawled page.
type CrawlResult struct {
	URL        string   `json:"url"`
	RawContent string   `json:"raw_content"`
	Favicon    *string  `json:"favicon,omitempty"`
	Images     []string `json:"images,omitempty"`
}

// CrawlResponse is the /crawl response.
type CrawlResponse struct {
	BaseURL      string        `json:"base_url"`
	Results      []CrawlResult `json:"results"`
	ResponseTime float64       `json:"response_time"`
	Usage        *Usage        `json:"usage,omitempty"`
	RequestID    string        `json:"request_id"`
}

func normalizeStartURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("No starting url provided")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid url")
	}
	u.Fragment = ""
	return u.String(), nil
}

type crawlOptions struct {
	maxDepth, maxBreadth, limit   int
	allowExternal                 bool
	selectPaths, excludePaths     *regexp.Regexp
	selectDomains, excludeDomains *regexp.Regexp
	instructions                  string
	deadline                      time.Time
	disallow                      []string
}

func compileRegexes(list []string) *regexp.Regexp {
	var parts []string
	for _, r := range list {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		parts = append(parts, "(?:"+r+")")
	}
	if len(parts) == 0 {
		return nil
	}
	re, err := regexp.Compile(strings.Join(parts, "|"))
	if err != nil {
		return nil
	}
	return re
}

func (s *Server) crawlOptions(req *MapRequest) (*crawlOptions, error) {
	start, err := normalizeStartURL(req.URL)
	if err != nil {
		return nil, err
	}
	req.URL = start
	o := &crawlOptions{maxDepth: 1, maxBreadth: 20, limit: 50, allowExternal: true}
	if req.MaxDepth != nil {
		o.maxDepth = *req.MaxDepth
	}
	if req.MaxBreadth != nil {
		o.maxBreadth = *req.MaxBreadth
	}
	if req.Limit != nil {
		o.limit = *req.Limit
	}
	if req.AllowExternal != nil {
		o.allowExternal = *req.AllowExternal
	}
	if o.maxDepth < 1 || o.maxDepth > 5 {
		return nil, fmt.Errorf("max_depth must be between 1 and 5")
	}
	if o.maxBreadth < 1 || o.maxBreadth > 500 {
		return nil, fmt.Errorf("max_breadth must be between 1 and 500")
	}
	if o.limit < 1 || o.limit > 1000 {
		return nil, fmt.Errorf("limit must be between 1 and 1000")
	}
	o.selectPaths = compileRegexes(req.SelectPaths)
	o.excludePaths = compileRegexes(req.ExcludePaths)
	o.selectDomains = compileRegexes(req.SelectDomains)
	o.excludeDomains = compileRegexes(req.ExcludeDomains)
	o.instructions = strings.TrimSpace(req.Instructions)
	timeout := 150
	if req.Timeout != nil {
		timeout = *req.Timeout
	}
	if timeout < 10 || timeout > 150 {
		return nil, fmt.Errorf("timeout must be between 10 and 150 seconds")
	}
	o.deadline = time.Now().Add(time.Duration(timeout) * time.Second)
	return o, nil
}

type frontierItem struct {
	url   string
	depth int
}

func (s *Server) walk(ctx context.Context, base string, o *crawlOptions, perPage func(context.Context, string) error) ([]string, error) {
	baseURL, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	o.disallow = s.fetchRobots(ctx, base)
	frontier := []frontierItem{{url: base, depth: 0}}
	for _, sm := range s.fetchSitemap(ctx, base, 3) {
		if allowedLink(sm, baseURL, o) {
			frontier = append(frontier, frontierItem{url: sm, depth: 1})
		}
		if len(frontier) >= o.limit {
			break
		}
	}
	results := make([]string, 0, o.limit)
	for len(frontier) > 0 && len(results) < o.limit {
		if time.Now().After(o.deadline) || ctx.Err() != nil {
			break
		}
		batch := frontier
		frontier = nil
		if len(batch) > o.maxBreadth {
			batch = batch[:o.maxBreadth]
		}
		type pageLinks struct {
			links []string
		}
		pages := make([]pageLinks, len(batch))
		sem := make(chan struct{}, 4)
		done := make(chan int, len(batch))
		for i, item := range batch {
			i, item := i, item
			sem <- struct{}{}
			go func() {
				defer func() { <-sem; done <- i }()
				fctx, cancel := context.WithDeadline(ctx, o.deadline)
				defer cancel()
				final, body, _, ferr := s.extractor.FetchHTML(fctx, item.url)
				if ferr != nil {
					return
				}
				if canon, cerr := search.Canonical(final); cerr == nil {
					final = canon
				}
				key := strings.ToLower(final)
				s.mu.Lock()
				if _, dup := seen[key]; dup {
					s.mu.Unlock()
					return
				}
				seen[key] = struct{}{}
				results = append(results, final)
				s.mu.Unlock()
				if perPage != nil {
					_ = perPage(fctx, final)
				}
				if item.depth >= o.maxDepth || len(body) == 0 {
					return
				}
				pages[i].links = extractLinks(body, final)
			}()
		}
		for range batch {
			<-done
		}
		type candidate struct {
			url   string
			depth int
		}
		var candidates []candidate
		depthOf := map[string]int{}
		for i := range pages {
			for _, link := range pages[i].links {
				d := batch[i].depth + 1
				candidates = append(candidates, candidate{url: link, depth: d})
				if prev, ok := depthOf[link]; !ok || d < prev {
					depthOf[link] = d
				}
			}
		}
		if o.instructions != "" && s.llm != nil && len(candidates) > 0 {
			urls := make([]string, 0, len(candidates))
			for _, c := range candidates {
				urls = append(urls, c.url)
			}
			keep := map[string]struct{}{}
			for _, u := range s.scoreInstructions(ctx, o.instructions, urls) {
				keep[u] = struct{}{}
			}
			filtered := candidates[:0]
			for _, c := range candidates {
				if _, ok := keep[c.url]; ok {
					filtered = append(filtered, c)
				}
			}
			candidates = filtered
		}
		for _, c := range candidates {
			if len(results)+len(frontier) >= o.limit {
				break
			}
			if !allowedLink(c.url, baseURL, o) {
				continue
			}
			if _, dup := seen[strings.ToLower(c.url)]; dup {
				continue
			}
			d := c.depth
			if pd, ok := depthOf[c.url]; ok && pd < d {
				d = pd
			}
			frontier = append(frontier, frontierItem{url: c.url, depth: d})
		}
	}
	return results, nil
}

func extractLinks(body []byte, pageURL string) []string {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return nil
	}
	base, err := url.Parse(pageURL)
	if err != nil {
		return nil
	}
	var out []string
	doc.Find("a[href]").Each(func(_ int, sel *goquery.Selection) {
		href, _ := sel.Attr("href")
		href = strings.TrimSpace(href)
		if href == "" || strings.HasPrefix(href, "#") || strings.HasPrefix(href, "mailto:") || strings.HasPrefix(href, "javascript:") {
			return
		}
		ref, err := url.Parse(href)
		if err != nil {
			return
		}
		abs := base.ResolveReference(ref)
		abs.Fragment = ""
		if abs.Scheme != "http" && abs.Scheme != "https" {
			return
		}
		out = append(out, abs.String())
	})
	return out
}

func allowedLink(raw string, base *url.URL, o *crawlOptions) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if !o.allowExternal && host != strings.ToLower(base.Hostname()) {
		return false
	}
	if o.selectDomains != nil && !o.selectDomains.MatchString(host) {
		return false
	}
	if o.excludeDomains != nil && o.excludeDomains.MatchString(host) {
		return false
	}
	if o.selectPaths != nil && !o.selectPaths.MatchString(u.Path) {
		return false
	}
	if o.excludePaths != nil && o.excludePaths.MatchString(u.Path) {
		return false
	}
	if o.instructions != "" && !instructionMatch(o.instructions, raw) {
		return false
	}
	for _, dis := range o.disallow {
		if dis != "" && strings.HasPrefix(u.Path, dis) {
			return false
		}
	}
	return true
}

// instructionMatch is the no-LLM fallback: keyword overlap on URL/anchor text.
func instructionMatch(instructions, raw string) bool {
	words := strings.Fields(strings.ToLower(instructions))
	u := strings.ToLower(raw)
	for _, w := range words {
		w = strings.Trim(w, ".,;:!?()[]\"'")
		if len(w) < 4 {
			continue
		}
		if strings.Contains(u, w) {
			return true
		}
	}
	return len(words) == 0
}

// fetchRobots returns Disallow path prefixes for the base host (conservative:
// all user-agent groups are merged).
func (s *Server) fetchRobots(ctx context.Context, base string) []string {
	u, err := url.Parse(base)
	if err != nil {
		return nil
	}
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, body, _, err := s.extractor.FetchHTML(rctx, u.Scheme+"://"+u.Host+"/robots.txt")
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(line), "disallow:") {
			path := strings.TrimSpace(line[len("disallow:"):])
			if path != "" && path != "/" {
				out = append(out, path)
			}
		}
	}
	return out
}

var reLoc = regexp.MustCompile(`(?is)<loc>\s*([^<]+?)\s*</loc>`)

// fetchSitemap seeds the frontier from sitemap.xml (and up to maxSitemaps child
// sitemaps for sitemap indexes).
func (s *Server) fetchSitemap(ctx context.Context, base string, maxSitemaps int) []string {
	u, err := url.Parse(base)
	if err != nil {
		return nil
	}
	rctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	root := u.Scheme + "://" + u.Host
	_, body, _, err := s.extractor.FetchHTML(rctx, root+"/sitemap.xml")
	if err != nil {
		return nil
	}
	locs := parseLocs(body)
	var out []string
	isIndex := strings.Contains(strings.ToLower(string(body)), "<sitemapindex")
	if isIndex {
		for i, child := range locs {
			if i >= maxSitemaps {
				break
			}
			_, childBody, _, cerr := s.extractor.FetchHTML(rctx, child)
			if cerr != nil {
				continue
			}
			out = append(out, parseLocs(childBody)...)
		}
	} else {
		out = append(out, locs...)
	}
	return out
}

func parseLocs(body []byte) []string {
	matches := reLoc.FindAllStringSubmatch(string(body), -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if len(m) == 2 {
			out = append(out, strings.TrimSpace(m[1]))
		}
	}
	return out
}

// scoreInstructions asks Qwen to keep only URLs matching the instructions; on
// error it falls back to keyword matching.
func (s *Server) scoreInstructions(ctx context.Context, instructions string, links []string) []string {
	const batch = 20
	out := make([]string, 0, len(links))
	for start := 0; start < len(links); start += batch {
		end := start + batch
		if end > len(links) {
			end = len(links)
		}
		part := links[start:end]
		payload := strings.Join(part, "\n")
		msgs := []llm.Message{{Role: "user", Content: "Keep only URLs relevant to these instructions. Reply JSON {\"keep\":[urls]}.\nInstructions: " + instructions + "\nURLs:\n" + payload}}
		schema := map[string]any{"type": "object", "properties": map[string]any{"keep": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}}, "required": []any{"keep"}}
		rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		raw, _, err := s.llm.ChatJSON(rctx, msgs, schema, llm.Options{Temperature: 0, MaxTokens: 800})
		cancel()
		if err != nil {
			for _, l := range part {
				if instructionMatch(instructions, l) {
					out = append(out, l)
				}
			}
			continue
		}
		var v struct {
			Keep []string `json:"keep"`
		}
		if json.Unmarshal(raw, &v) != nil || len(v.Keep) == 0 {
			for _, l := range part {
				if instructionMatch(instructions, l) {
					out = append(out, l)
				}
			}
			continue
		}
		out = append(out, v.Keep...)
	}
	return out
}

// HandleMap implements POST /map.
func (s *Server) HandleMap(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	var req MapRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	if err := s.authenticate(r, req.APIKey); err != nil {
		writeError(w, r, err)
		return
	}
	o, err := s.crawlOptions(&req)
	if err != nil {
		writeError(w, r, Errorf(http.StatusBadRequest, "%s", err.Error()))
		return
	}
	if !s.enforceQuota(r.Context(), w, r, int64((o.limit+9)/10)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Until(o.deadline))
	defer cancel()
	results, err := s.walk(ctx, req.URL, o, nil)
	if err != nil {
		writeError(w, r, Errorf(http.StatusBadRequest, "%s", err.Error()))
		return
	}
	if results == nil {
		results = []string{}
	}
	resp := MapResponse{BaseURL: req.URL, Results: results, ResponseTime: seconds(start), RequestID: RequestID(r.Context())}
	credits := (len(results) + 9) / 10
	s.recordUsage(r.Context(), usage.Delta{Credits: int64(credits), TokensOut: int64(len(results))})
	if req.IncludeUsage {
		resp.Usage = &Usage{Credits: credits}
	}
	writeJSON(w, http.StatusOK, resp)
}

// HandleCrawl implements POST /crawl.
func (s *Server) HandleCrawl(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	var req CrawlRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	if err := s.authenticate(r, req.APIKey); err != nil {
		writeError(w, r, err)
		return
	}
	o, err := s.crawlOptions(&req.MapRequest)
	if err != nil {
		writeError(w, r, Errorf(http.StatusBadRequest, "%s", err.Error()))
		return
	}
	if !s.enforceQuota(r.Context(), w, r, int64((o.limit+9)/10)) {
		return
	}
	depth := strings.ToLower(strings.TrimSpace(req.ExtractDepth))
	if depth == "" {
		depth = "basic"
	}
	if depth != "basic" && depth != "advanced" {
		writeError(w, r, Errorf(http.StatusBadRequest, "extract_depth must be basic or advanced"))
		return
	}
	format := strings.ToLower(strings.TrimSpace(req.Format))
	if format == "" {
		format = "markdown"
	}
	if format != "markdown" && format != "text" {
		writeError(w, r, Errorf(http.StatusBadRequest, "format must be markdown or text"))
		return
	}
	chunks := 3
	if req.ChunksPerSource != nil {
		chunks = *req.ChunksPerSource
	}
	if chunks < 1 || chunks > 5 {
		writeError(w, r, Errorf(http.StatusBadRequest, "chunks_per_source must be between 1 and 5"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Until(o.deadline))
	defer cancel()

	var mu sync.Mutex
	results := make([]CrawlResult, 0, o.limit)
	perPage := func(pctx context.Context, pageURL string) error {
		art, _, ferr := s.extractor.FetchPage(pctx, pageURL, depth, req.IncludeImages)
		if ferr != nil {
			return nil
		}
		content := art.Markdown
		if format == "text" && strings.TrimSpace(art.Text) != "" {
			content = art.Text
		}
		if req.Instructions != "" || req.ChunksPerSource != nil {
			content = s.chunkedContent(pctx, content, req.Instructions, chunks)
		}
		cr := CrawlResult{URL: pageURL, RawContent: content}
		if req.IncludeFavicon {
			cr.Favicon = faviconFor(pageURL)
		}
		if req.IncludeImages {
			cr.Images = art.Images
		}
		mu.Lock()
		results = append(results, cr)
		mu.Unlock()
		return nil
	}
	urls, err := s.walk(ctx, req.URL, o, perPage)
	if err != nil {
		writeError(w, r, Errorf(http.StatusBadRequest, "%s", err.Error()))
		return
	}
	_ = urls
	if results == nil {
		results = []CrawlResult{}
	}
	resp := CrawlResponse{BaseURL: req.URL, Results: results, ResponseTime: seconds(start), RequestID: RequestID(r.Context())}
	credits := (len(results) + 9) / 10
	var outChars int
	for _, res := range results {
		outChars += len(res.RawContent)
	}
	s.recordUsage(r.Context(), usage.Delta{Credits: int64(credits), TokensOut: int64((outChars + 3) / 4)})
	if req.IncludeUsage {
		resp.Usage = &Usage{Credits: credits}
	}
	writeJSON(w, http.StatusOK, resp)
}
