package extract

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"github.com/sony/gobreaker"

	"github.com/astralabs/tavilyd/internal/config"
	"github.com/astralabs/tavilyd/internal/observability"
	"github.com/astralabs/tavilyd/internal/security"
)

// Browser is tier 2: a bounded pool of tabs on a remote chromedp/headless-shell
// container. Image, font, media and tracker requests are failed at the network
// layer, and every main-frame navigation is re-checked by the SSRF guard.
type Browser struct {
	allocCtx context.Context
	cancel   context.CancelFunc
	tabs     chan struct{}
	breaker  *gobreaker.CircuitBreaker
	timeout  time.Duration
	guard    *security.Guard
	log      *slog.Logger
	metrics  *observability.Metrics
	httpURL  string
	enabled  bool
}

// NewBrowser attaches to the remote browser. Connection is lazy: the first
// navigation allocates a tab, so a cold browser does not block startup.
func NewBrowser(parent context.Context, cfg *config.Config, guard *security.Guard, log *slog.Logger, m *observability.Metrics) *Browser {
	b := &Browser{
		tabs:    make(chan struct{}, cfg.BrowserMaxTabs),
		breaker: newBrowserBreaker(cfg.BrowserBreaker),
		timeout: cfg.FetchAdvancedTimeout,
		guard:   guard,
		log:     log,
		metrics: m,
		httpURL: httpEndpoint(cfg.BrowserURL),
		enabled: cfg.BrowserURL != "" && cfg.BrowserURL != "off",
	}
	if !b.enabled {
		return b
	}
	allocCtx, cancel := chromedp.NewRemoteAllocator(parent, cfg.BrowserURL)
	b.allocCtx, b.cancel = allocCtx, cancel
	return b
}

func newBrowserBreaker(c config.BreakerConfig) *gobreaker.CircuitBreaker {
	return gobreaker.NewCircuitBreaker(gobreaker.Settings{
		Name:        "browser",
		MaxRequests: c.MaxRequests,
		Interval:    c.Interval,
		Timeout:     c.Timeout,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			return counts.ConsecutiveFailures >= 3
		},
	})
}

// Name implements Fetcher.
func (b *Browser) Name() string { return "browser" }

// Fetch renders target.url and returns the post-JavaScript DOM.
func (b *Browser) Fetch(ctx context.Context, target *securityTarget) (*Page, error) {
	if b == nil || !b.enabled || b.allocCtx == nil {
		return nil, errors.New("browser pool disabled")
	}
	if target == nil {
		return nil, errors.New("nil target")
	}
	select {
	case b.tabs <- struct{}{}:
		defer func() { <-b.tabs }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	var page *Page
	_, err := b.breaker.Execute(func() (any, error) {
		p, err := b.render(ctx, target)
		if err != nil {
			return nil, err
		}
		page = p
		return nil, nil
	})
	if err != nil {
		if b.metrics != nil {
			b.metrics.Extractions.WithLabelValues("browser", "error").Inc()
		}
		return nil, err
	}
	if b.metrics != nil {
		b.metrics.Extractions.WithLabelValues("browser", "ok").Inc()
	}
	return page, nil
}

func (b *Browser) render(ctx context.Context, target *security.Target) (*Page, error) {
	tabCtx, tabCancel := chromedp.NewContext(b.allocCtx, chromedp.WithErrorf(func(format string, args ...any) {
		b.log.Debug("chromedp", "msg", fmt.Sprintf(format, args...))
	}))
	defer tabCancel()

	chromedp.ListenTarget(tabCtx, b.interceptor(tabCtx))

	runCtx, runCancel := context.WithTimeout(tabCtx, b.timeout)
	defer runCancel()

	var pageHTML string
	var finalURL string
	err := chromedp.Run(runCtx,
		fetch.Enable().WithPatterns([]*fetch.RequestPattern{{URLPattern: "*", RequestStage: fetch.RequestStageRequest}}),
		chromedp.Navigate(target.URL.String()),
		waitForIdle(250*time.Millisecond),
		chromedp.Location(&finalURL),
		chromedp.OuterHTML("html", &pageHTML, chromedp.ByQuery),
	)
	if err != nil {
		return nil, fmt.Errorf("browser render: %w", err)
	}
	return &Page{
		URL:         target.URL.String(),
		FinalURL:    finalURL,
		Status:      http.StatusOK,
		ContentType: "text/html; charset=utf-8",
		Body:        []byte(pageHTML),
		Tier:        2,
		FetchedAt:   time.Now(),
	}, nil
}

// interceptor fails blocked resource types and validates main-frame documents.
func (b *Browser) interceptor(ctx context.Context) func(ev any) {
	return func(ev any) {
		paused, ok := ev.(*fetch.EventRequestPaused)
		if !ok || paused == nil || paused.Request == nil {
			return
		}
		reqURL := paused.Request.URL
		// Resource-type blocking is cheap and synchronous.
		switch paused.ResourceType {
		case network.ResourceTypeImage, network.ResourceTypeFont, network.ResourceTypeMedia:
			go failRequest(ctx, paused.RequestID)
			return
		}
		if isTracker(reqURL) {
			go failRequest(ctx, paused.RequestID)
			return
		}
		if paused.ResourceType == network.ResourceTypeDocument {
			// The browser resolves DNS itself, so re-validate here to keep a
			// redirect from steering the renderer at an internal address.
			go func(id fetch.RequestID, raw string) {
				rctx, cancel := context.WithTimeout(ctx, 800*time.Millisecond)
				defer cancel()
				if _, err := b.guard.ValidateURL(rctx, raw); err != nil {
					b.log.Warn("blocked browser navigation", "url", raw, "error", err)
					failRequest(ctx, id)
					return
				}
				_ = fetch.ContinueRequest(id).Do(ctx)
			}(paused.RequestID, reqURL)
			return
		}
		go func(id fetch.RequestID) { _ = fetch.ContinueRequest(id).Do(ctx) }(paused.RequestID)
	}
}

func failRequest(ctx context.Context, id fetch.RequestID) {
	rctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	_ = fetch.FailRequest(id, network.ErrorReasonBlockedByClient).Do(rctx)
}

// waitForIdle waits until document.readyState is complete and the number of
// resource timing entries has stopped growing for two consecutive polls.
func waitForIdle(settle time.Duration) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		deadline := time.Now().Add(3 * time.Second)
		last := -1
		stable := 0
		for {
			var state string
			if err := chromedp.Evaluate(`document.readyState`, &state).Do(ctx); err != nil {
				return err
			}
			var entries int
			if err := chromedp.Evaluate(`performance.getEntriesByType("resource").length`, &entries).Do(ctx); err != nil {
				return err
			}
			if state == "complete" && entries == last {
				stable++
				if stable >= 2 {
					select {
					case <-time.After(settle):
					case <-ctx.Done():
						return ctx.Err()
					}
					return nil
				}
			} else {
				stable = 0
			}
			last = entries
			if time.Now().After(deadline) {
				return nil // bounded: render with whatever we have
			}
			select {
			case <-time.After(120 * time.Millisecond):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	})
}

var trackerHosts = []string{
	"google-analytics.com", "googletagmanager.com", "doubleclick.net", "googlesyndication.com",
	"facebook.net", "facebook.com/tr", "hotjar.com", "segment.io", "segment.com",
	"mixpanel.com", "hubspot.com", "criteo.com", "criteo.net", "taboola.com", "outbrain.com",
	"quantserve.com", "scorecardresearch.com", "mc.yandex.ru", "amplitude.com", "fullstory.com",
	"clarity.ms", "matomo.cloud", "newrelic.com", "nr-data.net", "sentry.io",
}

func isTracker(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	for _, t := range trackerHosts {
		if strings.Contains(t, "/") {
			if strings.HasPrefix(strings.ToLower(raw), "https://"+t) || strings.HasPrefix(strings.ToLower(raw), "http://"+t) {
				return true
			}
			continue
		}
		if host == t || strings.HasSuffix(host, "."+t) {
			return true
		}
	}
	return false
}

// Health checks the browser's DevTools HTTP endpoint.
func (b *Browser) Health(ctx context.Context) (string, error) {
	if b == nil || !b.enabled {
		return "degraded", errors.New("browser disabled")
	}
	if b.breaker.State() == gobreaker.StateOpen {
		return "degraded", errors.New("browser circuit open")
	}
	reqCtx, cancel := context.WithTimeout(ctx, 1200*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, b.httpURL+"/json/version", nil)
	if err != nil {
		return "down", err
	}
	// Chrome's DevTools HTTP server rejects requests whose Host is not an IP or
	// localhost (it answers 500), so set it explicitly.
	req.Host = "localhost"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "down", err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<14))
	if resp.StatusCode != http.StatusOK {
		return "down", fmt.Errorf("devtools status %d", resp.StatusCode)
	}
	return "ok", nil
}

// Close releases the allocator connection.
func (b *Browser) Close() {
	if b != nil && b.cancel != nil {
		b.cancel()
	}
}

// httpEndpoint converts ws://host:port[/path] to http://host:port.
func httpEndpoint(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	scheme := "http"
	if u.Scheme == "wss" {
		scheme = "https"
	}
	return scheme + "://" + u.Host
}
