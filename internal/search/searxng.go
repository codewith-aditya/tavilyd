// Package search talks to SearXNG and post-processes its results.
package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sony/gobreaker"

	"github.com/astralabs/tavilyd/internal/config"
	"github.com/astralabs/tavilyd/internal/observability"
)

// Result is one discovery hit.
type Result struct {
	URL           string  `json:"url"`
	Title         string  `json:"title"`
	Content       string  `json:"content"`
	Engine        string  `json:"engine"`
	PublishedDate string  `json:"publishedDate,omitempty"`
	Thumbnail     string  `json:"thumbnail,omitempty"`
	ImgSrc        string  `json:"img_src,omitempty"`
	Score         float64 `json:"score"`
	Category      string  `json:"category,omitempty"`
}

// Query is a normalized discovery request.
type Query struct {
	Query      string
	Categories string // "general" or "news"
	TimeRange  string // day|week|month|year
	Language   string
	PageNo     int
	SafeSearch bool
	Engines    string
	EngineList []string
}

// StatusError carries a non-200 SearXNG response.
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string { return fmt.Sprintf("searxng status %d: %s", e.Code, e.Body) }

// Client is a SearXNG JSON API client with retry and a circuit breaker.
type Client struct {
	base    string
	http    *http.Client
	timeout time.Duration
	breaker *gobreaker.CircuitBreaker
	log     *slog.Logger
	metrics *observability.Metrics
	now     func() time.Time
	sleep   func(context.Context, time.Duration) error
}

// NewClient builds the discovery client.
func NewClient(cfg *config.Config, hc *http.Client, log *slog.Logger, m *observability.Metrics) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: cfg.SearxTimeout}
	}
	return &Client{
		base:    strings.TrimRight(cfg.SearxURL, "/"),
		http:    hc,
		timeout: cfg.SearxTimeout,
		breaker: newBreaker("searxng", cfg.SearxBreaker),
		log:     log,
		metrics: m,
		now:     time.Now,
		sleep:   sleepCtx,
	}
}

func newBreaker(name string, cfg config.BreakerConfig) *gobreaker.CircuitBreaker {
	return gobreaker.NewCircuitBreaker(gobreaker.Settings{
		Name:        name,
		MaxRequests: cfg.MaxRequests,
		Interval:    cfg.Interval,
		Timeout:     cfg.Timeout,
		ReadyToTrip: func(c gobreaker.Counts) bool {
			failures := int(c.ConsecutiveFailures)
			return failures >= 3 && failures*2 >= int(c.Requests)
		},
	})
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

type searxResponse struct {
	Query               string   `json:"query"`
	Results             []Result `json:"results"`
	NumberOfResults     int      `json:"number_of_results"`
	UnresponsiveEngines [][]any  `json:"unresponsive_engines"`
}

// Search executes one query and returns only results.
func (c *Client) Search(ctx context.Context, q Query) ([]Result, error) {
	res, _, _, err := c.SearchMeta(ctx, q)
	return res, err
}

// SearchMeta also reports which engines contributed and which were suspended.
func (c *Client) SearchMeta(ctx context.Context, q Query) ([]Result, []string, map[string]string, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			// Jittered backoff keeps retries from synchronizing across replicas.
			jitter := time.Duration(25+rand.Intn(50)) * time.Millisecond
			if err := c.sleep(ctx, jitter); err != nil {
				return nil, nil, nil, err
			}
		}
		res, used, suspended, err := c.onceMeta(ctx, q)
		if err == nil {
			return res, used, suspended, nil
		}
		lastErr = err
		if !isTransient(err) {
			break
		}
		c.log.Warn("searxng transient failure, retrying", "attempt", attempt+1, "error", err)
	}
	return nil, nil, nil, lastErr
}

func (c *Client) onceMeta(ctx context.Context, q Query) ([]Result, []string, map[string]string, error) {
	if c.breaker.State() == gobreaker.StateOpen {
		return nil, nil, nil, fmt.Errorf("searxng circuit open")
	}
	u, err := url.Parse(c.base + "/search")
	if err != nil {
		return nil, nil, nil, err
	}
	params := u.Query()
	params.Set("q", q.Query)
	params.Set("format", "json")
	params.Set("categories", q.Categories)
	safe := "0"
	if q.SafeSearch {
		safe = "2"
	}
	params.Set("safesearch", safe)
	if q.TimeRange != "" {
		params.Set("time_range", q.TimeRange)
	}
	if q.Engines != "" {
		params.Set("engines", q.Engines)
	}
	if q.Language != "" {
		params.Set("language", q.Language)
	}
	if q.PageNo > 0 {
		params.Set("pageno", strconv.Itoa(q.PageNo))
	}
	u.RawQuery = params.Encode()

	reqCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, nil, nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "tavilyd/1.0 (+self-hosted search)")

	start := c.now()
	raw, err := c.breaker.Execute(func() (any, error) {
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, &StatusError{Code: resp.StatusCode, Body: truncate(string(body), 300)}
		}
		return body, nil
	})
	if c.metrics != nil {
		c.metrics.ObserveStage("searxng", c.now().Sub(start))
	}
	if err != nil {
		return nil, nil, nil, err
	}
	body, _ := raw.([]byte)
	var parsed searxResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, nil, nil, fmt.Errorf("decode searxng response: %w", err)
	}
	out := make([]Result, 0, len(parsed.Results))
	usedSet := map[string]struct{}{}
	for _, r := range parsed.Results {
		if strings.TrimSpace(r.URL) == "" {
			continue
		}
		if r.Category == "" {
			r.Category = q.Categories
		}
		if r.Engine != "" {
			usedSet[r.Engine] = struct{}{}
		}
		out = append(out, r)
	}
	suspended := map[string]string{}
	for _, entry := range parsed.UnresponsiveEngines {
		if len(entry) >= 2 {
			name := fmt.Sprint(entry[0])
			suspended[name] = fmt.Sprint(entry[1])
		}
	}
	used := make([]string, 0, len(usedSet))
	for e := range usedSet {
		used = append(used, e)
	}
	sort.Strings(used)
	if c.metrics != nil {
		c.metrics.SearchResults.Observe(float64(len(out)))
	}
	return out, used, suspended, nil
}

// Healthy pings SearXNG with a lightweight request.
func (c *Client) Healthy(ctx context.Context) error {
	reqCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, c.base+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode >= 500 {
		return fmt.Errorf("searxng health status %d", resp.StatusCode)
	}
	return nil
}

// NormalizeLanguage maps ISO codes or English names (and a few country hints)
// to a SearXNG language tag. Approximate: SearXNG may not know every locale.
func NormalizeLanguage(language, country string) string {
	l := strings.ToLower(strings.TrimSpace(language))
	names := map[string]string{
		"english": "en", "hindi": "hi", "spanish": "es", "french": "fr", "german": "de",
		"portuguese": "pt", "russian": "ru", "japanese": "ja", "korean": "ko",
		"chinese": "zh", "arabic": "ar", "italian": "it", "dutch": "nl", "turkish": "tr",
	}
	if code, ok := names[l]; ok {
		return code
	}
	if len(l) == 2 || (len(l) == 5 && strings.Contains(l, "-")) {
		return l
	}
	c := strings.ToLower(strings.TrimSpace(country))
	countryLang := map[string]string{
		"india": "en", "united states": "en-US", "united kingdom": "en-GB",
		"germany": "de", "france": "fr", "japan": "ja", "brazil": "pt-BR",
		"canada": "en-CA", "australia": "en-AU", "spain": "es", "italy": "it",
	}
	if code, ok := countryLang[c]; ok {
		return code
	}
	return ""
}

// IsOpen reports the breaker state (used by /health).
func (c *Client) IsOpen() bool { return c.breaker.State() == gobreaker.StateOpen }

func isTransient(err error) bool {
	if err == nil {
		return false
	}
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code == http.StatusTooManyRequests || se.Code >= 500
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	return true
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
