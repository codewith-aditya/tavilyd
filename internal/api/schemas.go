// Package api implements Tavily-compatible HTTP handlers.
package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// FlexibleBool accepts true/false, "true"/"false" and Tavily's
// include_answer values "basic"/"advanced".
type FlexibleBool bool

// UnmarshalJSON implements json.Unmarshaler.
func (f *FlexibleBool) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		*f = false
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "", "false", "0", "no", "none":
			*f = false
		default: // "true", "basic", "advanced"
			*f = true
		}
		return nil
	}
	var v bool
	if err := json.Unmarshal(b, &v); err != nil {
		// Numeric 0/1 is accepted for lenient clients.
		var n float64
		if err2 := json.Unmarshal(b, &n); err2 != nil {
			return fmt.Errorf("expected bool or string, got %s", string(b))
		}
		*f = n != 0
		return nil
	}
	*f = FlexibleBool(v)
	return nil
}

// MarshalJSON implements json.Marshaler.
func (f FlexibleBool) MarshalJSON() ([]byte, error) { return json.Marshal(bool(f)) }

// StringList accepts a single string or an array of strings.
type StringList []string

// UnmarshalJSON implements json.Unmarshaler.
func (s *StringList) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		*s = nil
		return nil
	}
	if b[0] == '"' {
		var one string
		if err := json.Unmarshal(b, &one); err != nil {
			return err
		}
		*s = []string{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*s = many
	return nil
}

// RawContentMode accepts false, true, "markdown" or "text".
type RawContentMode struct {
	Enabled bool
	Text    bool
}

// UnmarshalJSON implements json.Unmarshaler.
func (m *RawContentMode) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" || string(b) == "false" {
		*m = RawContentMode{}
		return nil
	}
	if string(b) == "true" {
		*m = RawContentMode{Enabled: true}
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("include_raw_content must be bool or markdown|text")
	}
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "markdown":
		*m = RawContentMode{Enabled: true}
	case "text":
		*m = RawContentMode{Enabled: true, Text: true}
	case "", "false":
		*m = RawContentMode{}
	default:
		return fmt.Errorf("include_raw_content must be bool or markdown|text")
	}
	return nil
}

// SearchRequest is the POST /search body, matching Tavily's public schema.
type SearchRequest struct {
	Query                    string         `json:"query"`
	SearchDepth              string         `json:"search_depth"`
	MaxResults               *int           `json:"max_results"`
	ChunksPerSource          *int           `json:"chunks_per_source"`
	Topic                    string         `json:"topic"`
	TimeRange                string         `json:"time_range"`
	StartDate                string         `json:"start_date"`
	EndDate                  string         `json:"end_date"`
	Days                     *int           `json:"days"`
	IncludeAnswer            FlexibleBool   `json:"include_answer"`
	IncludeRawContent        RawContentMode `json:"include_raw_content"`
	IncludeImages            bool           `json:"include_images"`
	IncludeImageDescriptions bool           `json:"include_image_descriptions"`
	IncludeFavicon           bool           `json:"include_favicon"`
	IncludePublishedDate     bool           `json:"include_published_date"`
	FilterByPublishedDate    bool           `json:"filter_by_published_date"`
	MaxAgeHours              *int           `json:"max_age_hours"`
	IncludeDomains           []string       `json:"include_domains"`
	ExcludeDomains           []string       `json:"exclude_domains"`
	IncludeDomainsMode       string         `json:"include_domains_mode"`
	Country                  string         `json:"country"`
	Language                 string         `json:"language"`
	FilterByLanguage         bool           `json:"filter_by_language"`
	AutoParameters           bool           `json:"auto_parameters"`
	ExactMatch               bool           `json:"exact_match"`
	IncludeUsage             bool           `json:"include_usage"`
	SafeSearch               bool           `json:"safe_search"`
	APIKey                   string         `json:"api_key"`
}

// ValidatedSearch is the normalized, validated request used by the pipeline.
type ValidatedSearch struct {
	Query                    string
	SearchDepth              string
	MaxResults               int
	ChunksPerSource          int
	Topic                    string
	TimeRange                string
	StartDate                string
	EndDate                  string
	IncludeAnswer            FlexibleBool
	IncludeRawContent        RawContentMode
	IncludeImages            bool
	IncludeImageDescriptions bool
	IncludeFavicon           bool
	IncludePublishedDate     bool
	FilterByPublishedDate    bool
	MaxAgeHours              *int
	IncludeDomains           []string
	ExcludeDomains           []string
	IncludeDomainsMode       string
	Country                  string
	Language                 string
	FilterByLanguage         bool
	AutoParameters           bool
	ExactMatch               bool
	IncludeUsage             bool
	SafeSearch               bool
}

// Validate enforces Tavily's documented limits.
func (r *SearchRequest) Validate() (*ValidatedSearch, error) {
	q := strings.TrimSpace(r.Query)
	if q == "" {
		return nil, fmt.Errorf("query is required")
	}
	if len([]rune(q)) > 400 {
		return nil, fmt.Errorf("query must be at most 400 characters")
	}
	depth := strings.ToLower(strings.TrimSpace(r.SearchDepth))
	if depth == "" {
		depth = "basic"
	}
	switch depth {
	case "basic", "advanced", "fast", "ultra-fast":
	default:
		return nil, fmt.Errorf("search_depth must be one of: basic, advanced, fast, ultra-fast")
	}
	maxResults := 10
	if r.MaxResults != nil {
		maxResults = *r.MaxResults
	}
	if maxResults < 0 || maxResults > 20 {
		return nil, fmt.Errorf("max_results must be between 0 and 20")
	}
	chunks := 3
	if r.ChunksPerSource != nil {
		chunks = *r.ChunksPerSource
	}
	if chunks < 1 || chunks > 3 {
		return nil, fmt.Errorf("chunks_per_source must be between 1 and 3")
	}
	topic := strings.ToLower(strings.TrimSpace(r.Topic))
	if topic == "" {
		topic = "general"
	}
	if topic != "general" && topic != "news" && topic != "finance" {
		return nil, fmt.Errorf("topic must be one of: general, news, finance")
	}
	tr := strings.ToLower(strings.TrimSpace(r.TimeRange))
	switch tr {
	case "d":
		tr = "day"
	case "w":
		tr = "week"
	case "m":
		tr = "month"
	case "y":
		tr = "year"
	}
	switch tr {
	case "", "day", "week", "month", "year":
	default:
		return nil, fmt.Errorf("time_range must be one of: day, week, month, year, d, w, m, y")
	}
	for _, d := range []string{r.StartDate, r.EndDate} {
		if d == "" {
			continue
		}
		if _, err := time.Parse("2006-01-02", d); err != nil {
			return nil, fmt.Errorf("start_date/end_date must be YYYY-MM-DD")
		}
	}
	if r.MaxAgeHours != nil && (*r.MaxAgeHours < -1 || *r.MaxAgeHours > 720) {
		return nil, fmt.Errorf("max_age_hours must be between -1 and 720")
	}
	domainsMode := strings.ToLower(strings.TrimSpace(r.IncludeDomainsMode))
	if domainsMode == "" {
		domainsMode = "restrict"
	}
	if domainsMode != "restrict" && domainsMode != "prefer" {
		domainModeErr := fmt.Errorf("include_domains_mode must be one of: restrict, prefer")
		return nil, domainModeErr
	}
	if domainsMode == "prefer" && len(r.IncludeDomains) == 0 {
		return nil, fmt.Errorf("include_domains_mode requires include_domains")
	}
	if r.FilterByLanguage && strings.TrimSpace(r.Language) == "" {
		return nil, fmt.Errorf("filter_by_language requires language")
	}
	country := strings.ToLower(strings.TrimSpace(r.Country))
	language := strings.TrimSpace(r.Language)
	inc, err := validateDomains(r.IncludeDomains)
	if err != nil {
		return nil, fmt.Errorf("include_domains: %w", err)
	}
	exc, err := validateDomains(r.ExcludeDomains)
	if err != nil {
		return nil, fmt.Errorf("exclude_domains: %w", err)
	}
	if len(inc) > 300 {
		return nil, fmt.Errorf("Max 300 include_domains are allowed.")
	}
	if len(exc) > 150 {
		return nil, fmt.Errorf("Max 150 exclude_domains are allowed.")
	}
	return &ValidatedSearch{
		Query: q, SearchDepth: depth, MaxResults: maxResults, ChunksPerSource: chunks,
		Topic: topic, TimeRange: tr, StartDate: r.StartDate, EndDate: r.EndDate,
		IncludeAnswer: r.IncludeAnswer, IncludeRawContent: r.IncludeRawContent,
		IncludeImages: r.IncludeImages, IncludeImageDescriptions: r.IncludeImageDescriptions,
		IncludeFavicon: r.IncludeFavicon, IncludePublishedDate: r.IncludePublishedDate,
		FilterByPublishedDate: r.FilterByPublishedDate, MaxAgeHours: r.MaxAgeHours,
		IncludeDomains: inc, ExcludeDomains: exc, IncludeDomainsMode: domainsMode,
		Country: country, Language: language, FilterByLanguage: r.FilterByLanguage,
		AutoParameters: r.AutoParameters, ExactMatch: r.ExactMatch, IncludeUsage: r.IncludeUsage,
		SafeSearch: r.SafeSearch,
	}, nil
}

func validateDomains(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, d := range in {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "" {
			continue
		}
		if strings.Contains(d, "://") {
			u, err := url.Parse(d)
			if err != nil || u.Hostname() == "" {
				return nil, fmt.Errorf("invalid domain %q", d)
			}
			d = u.Hostname()
		}
		d = strings.TrimPrefix(d, "www.")
		d = strings.TrimSuffix(d, "/")
		if strings.ContainsAny(d, " /?#") || !strings.Contains(d, ".") {
			return nil, fmt.Errorf("invalid domain %q", d)
		}
		if _, dup := seen[d]; dup {
			continue
		}
		seen[d] = struct{}{}
		out = append(out, d)
	}
	return out, nil
}

// ExtractRequest is the POST /extract body.
type ExtractRequest struct {
	URLs                 StringList `json:"urls"`
	Query                string     `json:"query"`
	ChunksPerSource      *int       `json:"chunks_per_source"`
	ExtractDepth         string     `json:"extract_depth"`
	Format               string     `json:"format"`
	IncludeImages        bool       `json:"include_images"`
	IncludeFavicon       bool       `json:"include_favicon"`
	IncludePublishedDate bool       `json:"include_published_date"`
	Timeout              *float64   `json:"timeout"`
	IncludeUsage         bool       `json:"include_usage"`
	APIKey               string     `json:"api_key"`
}

// ValidatedExtract is the normalized extract request.
type ValidatedExtract struct {
	URLs                 []string
	Query                string
	ChunksPerSource      int
	ExtractDepth         string
	Format               string
	IncludeImages        bool
	IncludeFavicon       bool
	IncludePublishedDate bool
	Timeout              float64
	IncludeUsage         bool
}

// Validate enforces Tavily's documented limits.
func (r *ExtractRequest) Validate() (*ValidatedExtract, error) {
	if len(r.URLs) == 0 {
		return nil, fmt.Errorf("urls is required")
	}
	if len(r.URLs) > 20 {
		return nil, fmt.Errorf("Max 20 URLs are allowed.")
	}
	seen := make(map[string]struct{}, len(r.URLs))
	out := make([]string, 0, len(r.URLs))
	for _, raw := range r.URLs {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, fmt.Errorf("invalid url %q", raw)
		}
		if _, dup := seen[raw]; dup {
			continue
		}
		seen[raw] = struct{}{}
		out = append(out, raw)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("All URLs failed validation.")
	}
	depth := strings.ToLower(strings.TrimSpace(r.ExtractDepth))
	if depth == "" {
		depth = "basic"
	}
	if depth != "basic" && depth != "advanced" {
		return nil, fmt.Errorf("extract_depth must be one of: basic, advanced")
	}
	format := strings.ToLower(strings.TrimSpace(r.Format))
	if format == "" {
		format = "markdown"
	}
	if format != "markdown" && format != "text" {
		return nil, fmt.Errorf("format must be one of: markdown, text")
	}
	chunks := 3
	if r.ChunksPerSource != nil {
		chunks = *r.ChunksPerSource
	}
	if chunks < 1 || chunks > 5 {
		return nil, fmt.Errorf("chunks_per_source must be between 1 and 5")
	}
	timeout := 10.0
	if depth == "advanced" {
		timeout = 30.0
	}
	if r.Timeout != nil {
		if *r.Timeout < 1 || *r.Timeout > 60 {
			return nil, fmt.Errorf("timeout must be between 1 and 60 seconds")
		}
		timeout = *r.Timeout
	}
	return &ValidatedExtract{
		URLs: out, Query: strings.TrimSpace(r.Query), ChunksPerSource: chunks,
		ExtractDepth: depth, Format: format, IncludeImages: r.IncludeImages,
		IncludeFavicon: r.IncludeFavicon, IncludePublishedDate: r.IncludePublishedDate,
		Timeout: timeout, IncludeUsage: r.IncludeUsage,
	}, nil
}

// SearchResponse is the Tavily-compatible /search response.
type SearchResponse struct {
	Query          string             `json:"query"`
	Answer         *string            `json:"answer,omitempty"`
	Images         []any              `json:"images"`
	Results        []SearchResult     `json:"results"`
	AutoParameters map[string]any     `json:"auto_parameters,omitempty"`
	Usage          *Usage             `json:"usage,omitempty"`
	Diagnostics    *SearchDiagnostics `json:"diagnostics,omitempty"`
	ResponseTime   float64            `json:"response_time"`
	RequestID      string             `json:"request_id"`
}

// SearchDiagnostics is the per-request discovery/extraction funnel.
type SearchDiagnostics struct {
	Candidates  int `json:"candidates"`
	FetchedOK   int `json:"fetched_ok"`
	SnippetOnly int `json:"snippet_only"`
	Timeout     int `json:"timeout"`
	Failed      int `json:"failed"`
	TooShort    int `json:"too_short"`
	BelowFloor  int `json:"below_floor"`
	Final       int `json:"final"`
}

// Usage emulates Tavily's credit accounting for client compatibility.
type Usage struct {
	Credits int `json:"credits"`
}

// SearchResult is one ranked result.
type SearchResult struct {
	ID            string   `json:"id,omitempty"`
	Title         string   `json:"title"`
	URL           string   `json:"url"`
	Content       string   `json:"content"`
	Score         float64  `json:"score"`
	RawContent    *string  `json:"raw_content,omitempty"`
	Favicon       *string  `json:"favicon,omitempty"`
	PublishedDate *string  `json:"published_date,omitempty"`
	Images        []string `json:"images,omitempty"`
	SnippetOnly   bool     `json:"-"`
}

// ExtractResponse is the Tavily-compatible /extract response.
type ExtractResponse struct {
	Results       []ExtractResult `json:"results"`
	FailedResults []FailedResult  `json:"failed_results"`
	Usage         *Usage          `json:"usage,omitempty"`
	ResponseTime  float64         `json:"response_time"`
	RequestID     string          `json:"request_id"`
}

// ExtractResult is one successfully extracted URL.
type ExtractResult struct {
	URL           string   `json:"url"`
	RawContent    string   `json:"raw_content"`
	Images        []string `json:"images,omitempty"`
	Favicon       *string  `json:"favicon,omitempty"`
	PublishedDate *string  `json:"published_date,omitempty"`
}

// FailedResult explains why one URL could not be extracted.
type FailedResult struct {
	URL   string `json:"url"`
	Error string `json:"error"`
}
