// Package extract turns candidate URLs into clean Markdown using a two-tier
// fetch strategy: raw HTTP with a browser TLS fingerprint first, then a real
// headless Chromium when the page needs JavaScript or is challenge-gated.
package extract

import (
	"context"
	"time"

	"github.com/astralabs/tavilyd/internal/security"
)

// Page is the raw result of one fetch.
type Page struct {
	URL         string
	FinalURL    string
	Status      int
	ContentType string
	Body        []byte
	Tier        int
	FetchedAt   time.Time
}

// securityTarget aliases the pinned SSRF target so callers of this package do
// not need to import security just for the type.
type securityTarget = security.Target

// Fetcher fetches a validated target. Implementations must honour ctx and cap
// the response size.
type Fetcher interface {
	Fetch(ctx context.Context, target *securityTarget) (*Page, error)
	Name() string
}

// Article is the extracted, renderable form of a page.
type Article struct {
	Title         string
	Byline        string
	SiteName      string
	Excerpt       string
	Markdown      string
	Text          string
	Images        []string
	Length        int
	PublishedDate string
	// HTTPStatus and RawBytes are diagnostics only (never cached).
	HTTPStatus int
	RawBytes   int
	// Clean reports bytes removed per cleanup rule and CleanDuration how long
	// the cleanup layer took (for metrics only, never cached).
	Clean         CleanReport
	CleanDuration time.Duration
}

// ExtractStats is the per-request extraction funnel.
type ExtractStats struct {
	Candidates  int
	Good        int
	SnippetOnly int
	Timeout     int
	Failed      int
	TooShort    int
}

// Document is one usable search result after extraction.
type Document struct {
	URL           string
	Title         string
	Markdown      string
	Text          string
	Images        []string
	Snippet       string
	Tier          int
	Cached        bool
	SnippetOnly   bool
	PublishedDate string
	Took          time.Duration
}
