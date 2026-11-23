package research

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/astralabs/tavilyd/internal/llm"
)

// Tools is the search/read surface used by the engine (implemented by LiveTools
// in production and by fakes in tests).
type Tools interface {
	Search(ctx context.Context, query string, max int) ([]Source, error)
	Read(ctx context.Context, url, query string) (string, error)
}

// Engine runs one research job.
type Engine struct {
	llm    *llm.Client
	tools  Tools
	log    *slog.Logger
	tier   Tier
	budget budget
	mu     sync.Mutex
	emit   func(Event)
}

type budget struct {
	maxLLM    int
	maxPages  int
	maxTokens int
	llmCalls  int
	pages     int
	tokens    int
}

func (b *budget) addLLM(tokens int) {
	b.llmCalls++
	b.tokens += tokens
}

func (b *budget) exhausted() bool {
	return b.llmCalls >= b.maxLLM || (b.maxTokens > 0 && b.tokens >= b.maxTokens)
}

func (b *budget) pagesLeft() int {
	if b.pages >= b.maxPages {
		return 0
	}
	return b.maxPages - b.pages
}

var errBudget = errors.New("research budget exhausted")

// NewEngine builds an engine for one tier.
func NewEngine(client *llm.Client, tools Tools, tier Tier, log *slog.Logger) *Engine {
	return &Engine{
		llm: client, tools: tools, tier: tier, log: log,
		budget: budget{maxLLM: tier.MaxLLM, maxPages: tier.Pages, maxTokens: tier.MaxTokens},
	}
}

var (
	planSchema = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"aspects":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"queries":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"include_domains": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
		"required": []any{"queries"},
	}
	reflectSchema = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"enough":            map[string]any{"type": "boolean"},
			"gaps":              map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"follow_up_queries": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
		"required": []any{"enough"},
	}
	classifySchema = map[string]any{
		"type":       "object",
		"properties": map[string]any{"tier": map[string]any{"type": "string", "enum": []any{"mini", "pro"}}},
		"required":   []any{"tier"},
	}
)

// Run executes the loop and mutates job. Content/citations are finalized by the
// manager after Run returns.
func (e *Engine) Run(ctx context.Context, job *Job, emit func(Event)) error {
	e.emit = emit
	defer func() {
		e.mu.Lock()
		job.LLMCalls = e.budget.llmCalls
		job.PagesRead = e.budget.pages
		job.Tokens = e.budget.tokens
		e.mu.Unlock()
	}()

	if job.Model == "auto" {
		tier := e.classify(ctx, job.Input)
		if tier != "" && tier != job.Model {
			job.Model = tier
			e.applyTier(tier)
		}
	}
	e.progress("planning", "building a research plan")

	queries, _ := e.plan(ctx, job.Input)
	if e.log != nil {
		e.log.Info("research plan", "model", job.Model, "queries", queries)
	}
	var sources []Source
	byURL := map[string]*Source{}
	nextID := 1
	// Uploaded files are first-class sources with attached notes.
	for _, f := range job.Files {
		body := strings.TrimSpace(f.Content)
		if body == "" {
			continue
		}
		src := Source{ID: nextID, Title: f.Name, URL: "file://" + f.Name, Notes: trimToTokens(body, 3000), Read: true, Score: 1}
		nextID++
		sources = append(sources, src)
		byURL[src.URL] = &sources[len(sources)-1]
	}
	usedQueries := map[string]bool{}
	for _, q := range queries {
		usedQueries[strings.ToLower(q)] = true
	}

	for round := 0; round < e.tier.Rounds && !e.budget.exhausted(); round++ {
		if ctx.Err() != nil {
			break
		}
		job.Round = round + 1
		e.progress("searching", fmt.Sprintf("round %d: %d queries", round+1, len(queries)))
		found := e.searchAll(ctx, queries)
		newSources := 0
		for _, f := range found {
			if !domainAllowed(f.URL, job.IncludeDomains, job.ExcludeDomains) {
				continue
			}
			if s, ok := byURL[f.URL]; ok {
				if f.Score > s.Score {
					s.Score = f.Score
				}
				continue
			}
			f.ID = nextID
			nextID++
			byURL[f.URL] = &f
			sources = append(sources, f)
			newSources++
		}
		SortSources(sources)

		toRead := e.pickUnread(sources, byURL, e.budget.pagesLeft())
		if len(toRead) > 0 {
			e.progress("reading", fmt.Sprintf("%d sources", len(toRead)))
			notes := e.readAll(ctx, toRead, job.Input)
			for url, n := range notes {
				if cur := byURL[url]; cur != nil {
					cur.Notes = n
					cur.Read = true
				}
			}
		}
		for i := range sources {
			if byURL[sources[i].URL] != nil {
				sources[i] = *byURL[sources[i].URL]
			}
		}
		SortSources(sources)

		if round+1 >= e.tier.Rounds || e.budget.exhausted() || ctx.Err() != nil {
			break
		}
		e.progress("reflecting", "looking for gaps")
		enough, followups := e.reflect(ctx, job.Input, sources)
		if enough || len(followups) == 0 || newSources == 0 {
			break
		}
		queries = dedupeQueries(followups, usedQueries, e.tier.Queries)
		if len(queries) == 0 {
			break
		}
	}

	e.progress("writing", "synthesising the report")
	// Synthesis gets its own grace window so a consumed deadline still produces
	// a report from the notes gathered so far.
	writeCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if job.Schema != nil {
		raw, err := e.writeJSON(writeCtx, job.Input, sources, job.Schema)
		if err != nil {
			return fmt.Errorf("final JSON: %w", err)
		}
		job.ContentJSON = raw
		job.Content = string(raw)
		job.Sources = publicSources(sources)
		return nil
	}
	report, err := e.writeMarkdown(writeCtx, job.Input, sources)
	if err != nil {
		return fmt.Errorf("final report: %w", err)
	}
	cleaned, cited := VerifyCitations(report, sources)
	cleaned = RenderCitations(cleaned, cited, job.CitationFormat, time.Now())
	job.Content = cleaned
	job.Sources = publicSources(cited)
	if len(cited) == 0 {
		withNotes := 0
		for _, src := range sources {
			if strings.TrimSpace(src.Notes) != "" {
				withNotes++
			}
		}
		return fmt.Errorf("report produced no valid citations (sources=%d, with_notes=%d, report_chars=%d)", len(sources), withNotes, len(report))
	}
	return nil
}

func (e *Engine) applyTier(name string) {
	if name == "pro" {
		e.tier = ProTier()
	} else {
		e.tier = MiniTier()
	}
	e.budget.maxLLM = e.tier.MaxLLM
	e.budget.maxPages = e.tier.Pages
	e.budget.maxTokens = e.tier.MaxTokens
}

func (e *Engine) progress(stage, detail string) {
	if e.emit != nil {
		e.emit(Event{Type: "progress", Stage: stage, Detail: detail})
	}
}

func (e *Engine) classify(ctx context.Context, input string) string {
	msgs := []llm.Message{{Role: "user", Content: renderPrompt("classify", map[string]any{"Input": input})}}
	raw, err := e.chatJSON(ctx, msgs, classifySchema, false)
	if err != nil {
		return ""
	}
	var v struct {
		Tier string `json:"tier"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	return v.Tier
}

func (e *Engine) plan(ctx context.Context, input string) ([]string, []string) {
	msgs := []llm.Message{{Role: "user", Content: renderPrompt("plan", map[string]any{"Input": input, "Queries": e.tier.Queries})}}
	raw, err := e.chatJSON(ctx, msgs, planSchema, true)
	var plan struct {
		Aspects        []string `json:"aspects"`
		Queries        []string `json:"queries"`
		IncludeDomains []string `json:"include_domains"`
	}
	if err == nil {
		_ = json.Unmarshal(raw, &plan)
	}
	queries := dedupeQueries(plan.Queries, map[string]bool{}, e.tier.Queries)
	if len(queries) == 0 {
		queries = []string{input}
	}
	// A skeptical query is mandatory for balanced evidence.
	skeptical := false
	for _, q := range queries {
		l := strings.ToLower(q)
		if strings.Contains(l, "critic") || strings.Contains(l, "challenge") || strings.Contains(l, "risk") || strings.Contains(l, "limitation") || strings.Contains(l, "counter") {
			skeptical = true
		}
	}
	if !skeptical && e.tier.Queries > len(queries) {
		queries = append(queries, "criticism and limitations of "+input)
	}
	return queries, plan.Aspects
}

func dedupeQueries(in []string, used map[string]bool, max int) []string {
	out := make([]string, 0, max)
	for _, q := range in {
		q = strings.TrimSpace(q)
		if q == "" || len(q) > 300 {
			continue
		}
		key := strings.ToLower(q)
		if used[key] {
			continue
		}
		used[key] = true
		out = append(out, q)
		if len(out) >= max {
			break
		}
	}
	return out
}

func (e *Engine) searchAll(ctx context.Context, queries []string) []Source {
	var mu sync.Mutex
	out := make([]Source, 0, len(queries)*5)
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(4)
	for _, q := range queries {
		q := q
		g.Go(func() error {
			res, err := e.tools.Search(gctx, q, 6)
			if e.log != nil {
				n := 0
				if err == nil {
					n = len(res)
				}
				e.log.Info("research search", "query", q, "results", n, "error", err)
			}
			if err != nil {
				return nil
			}
			mu.Lock()
			out = append(out, res...)
			mu.Unlock()
			return nil
		})
	}
	_ = g.Wait()
	return out
}

func (e *Engine) pickUnread(sources []Source, byURL map[string]*Source, limit int) []Source {
	if limit <= 0 {
		return nil
	}
	out := make([]Source, 0, limit)
	for _, s := range sources {
		cur := byURL[s.URL]
		if cur == nil || cur.Read {
			continue
		}
		out = append(out, *cur)
		if len(out) >= limit {
			break
		}
	}
	return out
}

func (e *Engine) readAll(ctx context.Context, sources []Source, query string) map[string]string {
	var mu sync.Mutex
	notes := make(map[string]string, len(sources))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(3)
	for i := range sources {
		s := sources[i]
		g.Go(func() error {
			text, err := e.tools.Read(gctx, s.URL, query)
			if err != nil || strings.TrimSpace(text) == "" {
				return nil
			}
			n := e.notes(gctx, s, text)
			mu.Lock()
			e.budget.pages++
			notes[s.URL] = n
			mu.Unlock()
			if e.emit != nil {
				pub := s
				pub.Notes = n
				e.emit(Event{Type: "source", Source: &pub})
			}
			return nil
		})
	}
	_ = g.Wait()
	return notes
}

func (e *Engine) notes(ctx context.Context, s Source, text string) string {
	msgs := []llm.Message{{Role: "user", Content: renderPrompt("notes", map[string]any{
		"Query": s.Snippet, "Title": s.Title, "URL": s.URL,
		"Content": trimToTokens(text, 2500),
	})}}
	noThink := false
	out, tokens, err := e.llm.Chat(ctx, msgs, llm.Options{Thinking: &noThink, Temperature: 0.1, MaxTokens: 400})
	e.mu.Lock()
	e.budget.addLLM(tokens)
	e.mu.Unlock()
	if err != nil || strings.TrimSpace(out) == "" || strings.Contains(strings.ToUpper(out), "NO_RELEVANT_CONTENT") {
		return ""
	}
	return out
}

func (e *Engine) reflect(ctx context.Context, input string, sources []Source) (bool, []string) {
	var b strings.Builder
	for _, s := range sources {
		if s.Notes == "" {
			continue
		}
		fmt.Fprintf(&b, "[S%d] %s: %s\n", s.ID, s.URL, trimToTokens(s.Notes, 200))
	}
	msgs := []llm.Message{{Role: "user", Content: renderPrompt("reflect", map[string]any{
		"Input": input, "Notes": trimToTokens(b.String(), 3000),
	})}}
	raw, err := e.chatJSON(ctx, msgs, reflectSchema, false)
	if err != nil {
		return true, nil
	}
	var v struct {
		Enough    bool     `json:"enough"`
		Gaps      []string `json:"gaps"`
		FollowUps []string `json:"follow_up_queries"`
	}
	_ = json.Unmarshal(raw, &v)
	if v.Enough {
		return true, nil
	}
	return false, v.FollowUps
}

func (e *Engine) writeMarkdown(ctx context.Context, input string, sources []Source) (string, error) {
	msgs := []llm.Message{{Role: "user", Content: renderPrompt("write", map[string]any{
		"Input": input, "Sources": e.sourcesBlock(sources), "Format": "markdown",
	})}}
	noThink := false
	out, tokens, err := e.llm.Chat(ctx, msgs, llm.Options{Thinking: &noThink, Temperature: 0.3, MaxTokens: 4000})
	e.mu.Lock()
	e.budget.addLLM(tokens)
	e.mu.Unlock()
	if err != nil {
		return "", err
	}
	return out, nil
}

func (e *Engine) writeJSON(ctx context.Context, input string, sources []Source, schema map[string]any) (json.RawMessage, error) {
	msgs := []llm.Message{{Role: "user", Content: renderPrompt("write", map[string]any{
		"Input": input, "Sources": e.sourcesBlock(sources), "Format": "json",
	})}}
	noThink := false
	raw, tokens, err := e.llm.ChatJSON(ctx, msgs, schema, llm.Options{Thinking: &noThink, Temperature: 0.2, MaxTokens: 2500})
	e.mu.Lock()
	e.budget.addLLM(tokens)
	e.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func (e *Engine) sourcesBlock(sources []Source) string {
	var b strings.Builder
	for _, s := range sources {
		if s.Notes == "" {
			continue
		}
		fmt.Fprintf(&b, "<untrusted source id=\"S%d\" url=%q>\n%s\n</untrusted>\n\n", s.ID, s.URL, trimToTokens(s.Notes, 1200))
	}
	return b.String()
}

func (e *Engine) chatJSON(ctx context.Context, msgs []llm.Message, schema map[string]any, thinking bool) (json.RawMessage, error) {
	if e.budget.exhausted() {
		return nil, errBudget
	}
	raw, tokens, err := e.llm.ChatJSON(ctx, msgs, schema, llm.Options{Thinking: &thinking, Temperature: 0.2})
	e.mu.Lock()
	e.budget.addLLM(tokens)
	e.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// domainAllowed applies research include (soft) / exclude (hard) domains.
func domainAllowed(raw string, include, exclude []string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return true
	}
	host := strings.ToLower(strings.TrimPrefix(u.Hostname(), "www."))
	for _, d := range exclude {
		d = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(d), "www."))
		if d != "" && (host == d || strings.HasSuffix(host, "."+d)) {
			return false
		}
	}
	return true
}

func publicSources(sources []Source) []Source {
	out := make([]Source, 0, len(sources))
	for _, s := range sources {
		out = append(out, Source{ID: s.ID, Title: s.Title, URL: s.URL, Favicon: nil, Score: s.Score})
	}
	return out
}
