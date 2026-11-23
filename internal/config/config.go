// Package config parses and validates every tunable from the environment into
// one typed struct. Loading fails fast: an invalid value aborts startup with an
// aggregated error instead of silently falling back to a default.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the single source of truth for runtime tunables.
type Config struct {
	// HTTP server
	Addr              string
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
	MaxBodyBytes      int64
	RequestDeadline   time.Duration

	// Auth
	APIKeys []string

	// Content cleanup
	CleanEnabled            bool
	CleanMaxTokenChars      int
	CleanLinkDensity        float64
	CleanMinAlpha           float64
	CleanCTAPhrasesFile     string
	CleanCTAPhrases         []string
	MinResultScore          float64
	MinRelativeScore        float64
	SnippetMinRelativeScore float64
	SnippetFallback         bool
	SnippetMaxChars         int
	SnippetAdvancedChars    int
	RawContentMaxChars      int
	CacheVersion            string

	// Cache
	RedisURL         string
	RedisTimeout     time.Duration
	CacheTTL         time.Duration
	ExtractCacheTTL  time.Duration
	NegativeCacheTTL time.Duration
	LRUEntries       int

	// Discovery
	SearxURL              string
	SearxProxyURL         string
	SearchEngines         []string
	SearchEnginesPerQuery int
	SearchEngineCooldown  time.Duration
	SearxTimeout          time.Duration
	SearxCandidatePad     int
	SearxMaxConns         int
	SearxBreaker          BreakerConfig

	// Extraction
	FetchBasicTimeout    time.Duration
	FetchAdvancedTimeout time.Duration
	FetchTotalTimeout    time.Duration
	FetchMaxBytes        int64
	FetchConcurrency     int
	FetchUserAgent       string
	BrowserURL           string
	BrowserMaxTabs       int
	BrowserIdleTimeout   time.Duration
	BrowserBreaker       BreakerConfig

	// Reranking
	TEIURL            string
	TEITimeout        time.Duration
	TEIMaxBatch       int
	TEIParallelism    int
	TEIModel          string
	ChunkTokens       int
	ChunkOverlap      int
	MaxChunksPerPage  int
	MaxChunksTotal    int
	CharsPerToken     float64
	TokenSafetyMargin float64
	TEIBreaker        BreakerConfig

	// Proxy layer
	ProxyMode             string
	ProxyEndpoint         string
	ProxyStickyTemplate   string
	ProxyListFile         string
	ProxyPolicy           string
	ProxyHostRulesFile    string
	ProxyConnectTimeout   time.Duration
	ProxyFetchExtra       time.Duration
	ProxyMaxRetries       int
	ProxyMaxConnsPerProxy int
	ProxyHostMemoryTTL    time.Duration
	ProxySearXNG          bool
	ProxyTLSInsecure      bool

	// Limits / security
	RateLimitRPS    float64
	RateLimitBurst  int
	CreditLimit     int
	AllowPrivateIPs bool
	SSRFAllowlist   []string
	MaxRedirects    int

	// LLM backend (research)
	LLMBaseURL         string
	LLMAPIKey          string
	LLMModel           string
	LLMContextTokens   int
	LLMMaxOutputTokens int
	LLMTimeout         time.Duration
	LLMConnectTimeout  time.Duration
	LLMHealthTimeout   time.Duration
	LLMAnswerTimeout   time.Duration
	LLMMaxConcurrency  int
	LLMThinking        string
	LLMCAFile          string

	// Research
	ResearchEnabled         bool
	ResearchWorkers         int
	ResearchQueue           int
	ResearchResultTTL       time.Duration
	ResearchMaxJobsPerKey   int
	ResearchDeadlineMini    time.Duration
	ResearchDeadlinePro     time.Duration
	ResearchMaxLLMCallsMini int
	ResearchMaxLLMCallsPro  int
	ResearchMaxPagesMini    int
	ResearchMaxPagesPro     int
	ResearchMaxTokensMini   int
	ResearchMaxTokensPro    int
	ResearchCreateStatus    int

	// Observability
	LogLevel     slog.Level
	MetricsAddr  string
	PprofAddr    string
	OTelEnabled  bool
	OTelEndpoint string
	OTelInsecure bool
	ServiceName  string
}

// BreakerConfig configures a per-dependency circuit breaker.
type BreakerConfig struct {
	MaxRequests uint32
	Interval    time.Duration
	Timeout     time.Duration
}

// Load reads configuration from the process environment.
func Load() (*Config, error) { return LoadFrom(os.Getenv) }

// LoadFrom is Load with an injectable lookup function (used by tests).
func LoadFrom(getenv func(string) string) (*Config, error) {
	p := &parser{getenv: getenv}

	llmBase := p.str("LLM_BASE_URL", "")
	cfg := &Config{
		Addr:              p.str("HTTP_ADDR", ":8080"),
		ReadHeaderTimeout: p.dur("HTTP_READ_HEADER_TIMEOUT", 5*time.Second),
		ReadTimeout:       p.dur("HTTP_READ_TIMEOUT", 10*time.Second),
		WriteTimeout:      p.dur("HTTP_WRITE_TIMEOUT", 30*time.Second),
		IdleTimeout:       p.dur("HTTP_IDLE_TIMEOUT", 120*time.Second),
		ShutdownTimeout:   p.dur("HTTP_SHUTDOWN_TIMEOUT", 20*time.Second),
		MaxBodyBytes:      p.bytes("HTTP_MAX_BODY_BYTES", 1<<20),
		RequestDeadline:   p.dur("REQUEST_DEADLINE", 6*time.Second),

		APIKeys: p.list("API_KEY", nil),

		CleanEnabled:            p.boolean("CLEAN_ENABLED", true),
		CleanMaxTokenChars:      p.int("CLEAN_MAX_TOKEN_CHARS", 2000),
		CleanLinkDensity:        p.float("CLEAN_LINK_DENSITY", 0.5),
		CleanMinAlpha:           p.float("CLEAN_MIN_ALPHA", 0.4),
		CleanCTAPhrasesFile:     p.str("CLEAN_CTA_PHRASES_FILE", ""),
		MinResultScore:          p.float("MIN_RESULT_SCORE", 0.10),
		MinRelativeScore:        p.float("MIN_RELATIVE_SCORE", 0.05),
		SnippetMinRelativeScore: p.float("SNIPPET_MIN_RELATIVE_SCORE", 0.02),
		SnippetFallback:         p.boolean("SNIPPET_FALLBACK", true),
		SnippetMaxChars:         p.int("SNIPPET_MAX_CHARS", 1500),
		SnippetAdvancedChars:    p.int("SNIPPET_MAX_CHARS_ADVANCED", 3000),
		RawContentMaxChars:      p.int("RAW_CONTENT_MAX_CHARS", 60000),
		CacheVersion:            p.str("CACHE_VERSION", "7"),

		RedisURL:         p.str("REDIS_URL", "redis://redis:6379/0"),
		RedisTimeout:     p.dur("REDIS_TIMEOUT", 250*time.Millisecond),
		CacheTTL:         p.dur("CACHE_TTL", 15*time.Minute),
		ExtractCacheTTL:  p.dur("EXTRACT_CACHE_TTL", 24*time.Hour),
		NegativeCacheTTL: p.dur("NEGATIVE_CACHE_TTL", 5*time.Minute),
		LRUEntries:       p.int("LRU_ENTRIES", 2000),

		SearxURL:              p.str("SEARXNG_URL", "http://searxng:8080"),
		SearxProxyURL:         p.str("SEARXNG_PROXY_URL", ""),
		SearchEngines:         p.list("SEARCH_ENGINES", []string{"bing", "duckduckgo", "yahoo", "brave"}),
		SearchEnginesPerQuery: p.int("SEARCH_ENGINES_PER_QUERY", 2),
		SearchEngineCooldown:  p.dur("SEARCH_ENGINE_COOLDOWN", 2*time.Minute),
		SearxTimeout:          p.dur("SEARXNG_TIMEOUT", 2500*time.Millisecond),
		SearxCandidatePad:     p.int("SEARXNG_CANDIDATE_PAD", 8),
		SearxMaxConns:         p.int("SEARXNG_MAX_CONNS", 64),
		SearxBreaker: BreakerConfig{
			MaxRequests: uint32(p.int("SEARXNG_BREAKER_MAX_REQUESTS", 3)),
			Interval:    p.dur("SEARXNG_BREAKER_INTERVAL", 30*time.Second),
			Timeout:     p.dur("SEARXNG_BREAKER_TIMEOUT", 10*time.Second),
		},

		FetchBasicTimeout:    p.dur("FETCH_BASIC_TIMEOUT", 2*time.Second),
		FetchAdvancedTimeout: p.dur("FETCH_ADVANCED_TIMEOUT", 4*time.Second),
		FetchTotalTimeout:    p.dur("FETCH_TOTAL_TIMEOUT", 5*time.Second),
		FetchMaxBytes:        p.bytes("FETCH_MAX_BYTES", 5<<20),
		FetchConcurrency:     p.int("FETCH_CONCURRENCY", 12),
		FetchUserAgent:       p.str("FETCH_USER_AGENT", defaultUA),
		BrowserURL:           p.str("BROWSER_URL", "ws://browser:9222"),
		BrowserMaxTabs:       p.int("BROWSER_MAX_TABS", 8),
		BrowserIdleTimeout:   p.dur("BROWSER_IDLE_TIMEOUT", 3*time.Second),
		BrowserBreaker: BreakerConfig{
			MaxRequests: uint32(p.int("BROWSER_BREAKER_MAX_REQUESTS", 2)),
			Interval:    p.dur("BROWSER_BREAKER_INTERVAL", 30*time.Second),
			Timeout:     p.dur("BROWSER_BREAKER_TIMEOUT", 15*time.Second),
		},

		TEIURL:            p.str("TEI_URL", "http://reranker:80"),
		TEITimeout:        p.dur("TEI_TIMEOUT", 1500*time.Millisecond),
		TEIMaxBatch:       p.int("TEI_MAX_BATCH", 32),
		TEIParallelism:    p.int("TEI_PARALLELISM", 4),
		TEIModel:          p.str("TEI_MODEL", "BAAI/bge-reranker-v2-m3"),
		ChunkTokens:       p.int("CHUNK_TOKENS", 350),
		ChunkOverlap:      p.int("CHUNK_OVERLAP", 50),
		MaxChunksPerPage:  p.int("MAX_CHUNKS_PER_PAGE", 24),
		MaxChunksTotal:    p.int("MAX_CHUNKS_TOTAL", 200),
		CharsPerToken:     p.float("CHARS_PER_TOKEN", 4.0),
		TokenSafetyMargin: p.float("TOKEN_SAFETY_MARGIN", 1.15),
		TEIBreaker: BreakerConfig{
			MaxRequests: uint32(p.int("TEI_BREAKER_MAX_REQUESTS", 5)),
			Interval:    p.dur("TEI_BREAKER_INTERVAL", 30*time.Second),
			Timeout:     p.dur("TEI_BREAKER_TIMEOUT", 10*time.Second),
		},

		ProxyMode:             p.str("PROXY_MODE", "off"),
		ProxyEndpoint:         p.str("PROXY_ENDPOINT", ""),
		ProxyStickyTemplate:   p.str("PROXY_STICKY_TEMPLATE", ""),
		ProxyListFile:         p.str("PROXY_LIST_FILE", ""),
		ProxyPolicy:           p.str("PROXY_POLICY", "direct_first"),
		ProxyHostRulesFile:    p.str("PROXY_HOST_RULES", ""),
		ProxyConnectTimeout:   p.dur("PROXY_CONNECT_TIMEOUT", 2*time.Second),
		ProxyFetchExtra:       p.dur("PROXY_FETCH_TIMEOUT_EXTRA", time.Second),
		ProxyMaxRetries:       p.int("PROXY_MAX_RETRIES", 1),
		ProxyMaxConnsPerProxy: p.int("PROXY_MAX_CONNS_PER_PROXY", 64),
		ProxyHostMemoryTTL:    p.dur("PROXY_HOST_MEMORY_TTL", time.Hour),
		ProxySearXNG:          p.onoff("PROXY_SEARXNG", false),
		ProxyTLSInsecure:      p.boolean("PROXY_TLS_INSECURE", false),

		RateLimitRPS:    p.float("RATE_LIMIT_RPS", 10),
		RateLimitBurst:  p.int("RATE_LIMIT_BURST", 20),
		CreditLimit:     p.int("CREDIT_LIMIT", 0),
		AllowPrivateIPs: p.boolean("ALLOW_PRIVATE_IPS", false),
		SSRFAllowlist:   p.list("SSRF_ALLOWLIST", nil),
		MaxRedirects:    p.int("MAX_REDIRECTS", 5),

		LLMBaseURL:         llmBase,
		LLMAPIKey:          p.str("LLM_API_KEY", ""),
		LLMModel:           p.str("LLM_MODEL", "Qwen3.8-27B-FP8"),
		LLMContextTokens:   p.int("LLM_CONTEXT_TOKENS", 32768),
		LLMMaxOutputTokens: p.int("LLM_MAX_OUTPUT_TOKENS", 2048),
		LLMTimeout:         p.dur("LLM_TIMEOUT", 60*time.Second),
		LLMConnectTimeout:  p.dur("LLM_CONNECT_TIMEOUT", 5*time.Second),
		LLMHealthTimeout:   p.dur("LLM_HEALTH_TIMEOUT", 5*time.Second),
		LLMAnswerTimeout:   p.dur("LLM_ANSWER_TIMEOUT", 1500*time.Millisecond),
		LLMMaxConcurrency:  p.int("LLM_MAX_CONCURRENCY", 2),
		LLMThinking:        p.str("LLM_THINKING", "auto"),
		LLMCAFile:          p.str("LLM_CA_FILE", ""),

		ResearchEnabled:         p.boolean("RESEARCH_ENABLED", llmBase != ""),
		ResearchWorkers:         p.int("RESEARCH_WORKERS", 2),
		ResearchQueue:           p.int("RESEARCH_QUEUE", 16),
		ResearchResultTTL:       p.dur("RESEARCH_RESULT_TTL", 24*time.Hour),
		ResearchMaxJobsPerKey:   p.int("RESEARCH_MAX_JOBS_PER_KEY", 2),
		ResearchDeadlineMini:    p.dur("RESEARCH_DEADLINE_MINI", 90*time.Second),
		ResearchDeadlinePro:     p.dur("RESEARCH_DEADLINE_PRO", 300*time.Second),
		ResearchMaxLLMCallsMini: p.int("RESEARCH_MAX_LLM_CALLS_MINI", 12),
		ResearchMaxLLMCallsPro:  p.int("RESEARCH_MAX_LLM_CALLS_PRO", 40),
		ResearchMaxPagesMini:    p.int("RESEARCH_MAX_PAGES_MINI", 8),
		ResearchMaxPagesPro:     p.int("RESEARCH_MAX_PAGES_PRO", 20),
		ResearchCreateStatus:    p.int("RESEARCH_CREATE_STATUS", 200),
		ResearchMaxTokensMini:   p.int("RESEARCH_MAX_TOKENS_MINI", 40000),
		ResearchMaxTokensPro:    p.int("RESEARCH_MAX_TOKENS_PRO", 120000),

		LogLevel:     p.level("LOG_LEVEL", slog.LevelInfo),
		MetricsAddr:  p.str("METRICS_ADDR", "127.0.0.1:9090"),
		PprofAddr:    p.str("PPROF_ADDR", "127.0.0.1:6060"),
		OTelEnabled:  p.boolean("OTEL_ENABLED", false),
		OTelEndpoint: p.str("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
		OTelInsecure: p.boolean("OTEL_EXPORTER_OTLP_INSECURE", true),
		ServiceName:  p.str("SERVICE_NAME", "tavilyd"),
	}

	cfg.validate(p)
	if err := errors.Join(p.errs...); err != nil {
		return nil, err
	}
	return cfg, nil
}

const defaultUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

func (c *Config) validate(p *parser) {
	if c.Addr == "" {
		p.fail("HTTP_ADDR must not be empty")
	} else if _, _, err := net.SplitHostPort(c.Addr); err != nil {
		p.fail("HTTP_ADDR must be host:port: %v", err)
	}
	if c.MaxBodyBytes <= 0 {
		p.fail("HTTP_MAX_BODY_BYTES must be > 0")
	}
	if c.RequestDeadline <= 0 || c.RequestDeadline > 60*time.Second {
		p.fail("REQUEST_DEADLINE must be in (0, 60s]")
	}
	if c.ShutdownTimeout <= 0 {
		p.fail("HTTP_SHUTDOWN_TIMEOUT must be > 0")
	}
	if c.CacheTTL <= 0 || c.ExtractCacheTTL <= 0 || c.NegativeCacheTTL <= 0 {
		p.fail("cache TTLs must be > 0")
	}
	if c.LRUEntries < 0 {
		p.fail("LRU_ENTRIES must be >= 0")
	}
	if c.MinResultScore < 0 || c.MinResultScore > 1 {
		p.fail("MIN_RESULT_SCORE must be within [0,1]")
	}
	if c.MinRelativeScore < 0 || c.MinRelativeScore > 1 {
		p.fail("MIN_RELATIVE_SCORE must be within [0,1]")
	}
	if c.SnippetMinRelativeScore < 0 || c.SnippetMinRelativeScore > 1 {
		p.fail("SNIPPET_MIN_RELATIVE_SCORE must be within [0,1]")
	}
	if c.SnippetMaxChars < 200 || c.SnippetAdvancedChars < c.SnippetMaxChars {
		p.fail("SNIPPET_MAX_CHARS must be >= 200 and SNIPPET_MAX_CHARS_ADVANCED >= SNIPPET_MAX_CHARS")
	}
	if c.RawContentMaxChars < 1000 {
		p.fail("RAW_CONTENT_MAX_CHARS must be >= 1000")
	}
	if c.CleanMaxTokenChars < 50 {
		p.fail("CLEAN_MAX_TOKEN_CHARS must be >= 50")
	}
	if c.CleanLinkDensity < 0 || c.CleanLinkDensity > 1 {
		p.fail("CLEAN_LINK_DENSITY must be within [0,1]")
	}
	if c.CleanMinAlpha < 0 || c.CleanMinAlpha > 1 {
		p.fail("CLEAN_MIN_ALPHA must be within [0,1]")
	}
	if strings.TrimSpace(c.CacheVersion) == "" {
		p.fail("CACHE_VERSION must not be empty")
	}
	if c.CleanCTAPhrasesFile != "" {
		raw, err := os.ReadFile(c.CleanCTAPhrasesFile)
		if err != nil {
			p.fail("CLEAN_CTA_PHRASES_FILE: %v", err)
		} else {
			for _, line := range strings.Split(string(raw), "\n") {
				if phrase := strings.TrimSpace(line); phrase != "" && !strings.HasPrefix(phrase, "#") {
					c.CleanCTAPhrases = append(c.CleanCTAPhrases, phrase)
				}
			}
		}
	}
	if u, err := url.Parse(c.RedisURL); err != nil || (u.Scheme != "redis" && u.Scheme != "rediss") {
		p.fail("REDIS_URL must be a redis:// or rediss:// URL")
	}
	if u, err := url.Parse(c.SearxURL); err != nil || u.Host == "" {
		p.fail("SEARXNG_URL must be an absolute URL")
	}
	if c.SearxProxyURL != "" {
		if u, err := url.Parse(c.SearxProxyURL); err != nil || u.Host == "" {
			p.fail("SEARXNG_PROXY_URL must be an absolute URL when set")
		}
	}
	if c.SearxCandidatePad < 0 || c.SearxCandidatePad > 20 {
		p.fail("SEARXNG_CANDIDATE_PAD must be within [0,20]")
	}
	if c.FetchConcurrency < 1 || c.FetchConcurrency > 512 {
		p.fail("FETCH_CONCURRENCY must be within [1,512]")
	}
	if c.BrowserMaxTabs < 1 || c.BrowserMaxTabs > 64 {
		p.fail("BROWSER_MAX_TABS must be within [1,64]")
	}
	if c.FetchBasicTimeout <= 0 || c.FetchAdvancedTimeout <= 0 || c.FetchTotalTimeout <= 0 {
		p.fail("fetch timeouts must be > 0")
	}
	if c.FetchMaxBytes < 64<<10 {
		p.fail("FETCH_MAX_BYTES must be >= 64KiB")
	}
	if u, err := url.Parse(c.TEIURL); err != nil || u.Host == "" {
		p.fail("TEI_URL must be an absolute URL")
	}
	if c.TEIMaxBatch < 1 || c.TEIMaxBatch > 1024 {
		p.fail("TEI_MAX_BATCH must be within [1,1024]")
	}
	if c.TEIParallelism < 1 || c.TEIParallelism > 64 {
		p.fail("TEI_PARALLELISM must be within [1,64]")
	}
	if c.ChunkTokens < 50 || c.ChunkTokens > 4096 {
		p.fail("CHUNK_TOKENS must be within [50,4096]")
	}
	if c.ChunkOverlap < 0 || c.ChunkOverlap >= c.ChunkTokens {
		p.fail("CHUNK_OVERLAP must be >= 0 and < CHUNK_TOKENS")
	}
	if c.MaxChunksPerPage < 1 || c.MaxChunksTotal < 1 || c.MaxChunksTotal < c.MaxChunksPerPage {
		p.fail("MAX_CHUNKS_TOTAL must be >= MAX_CHUNKS_PER_PAGE >= 1")
	}
	if c.CharsPerToken <= 0 || c.TokenSafetyMargin < 1 {
		p.fail("CHARS_PER_TOKEN must be > 0 and TOKEN_SAFETY_MARGIN >= 1")
	}
	if c.RateLimitRPS < 0 || c.RateLimitBurst < 1 {
		p.fail("RATE_LIMIT_RPS must be >= 0 and RATE_LIMIT_BURST >= 1")
	}
	switch c.ProxyMode {
	case "off", "rotating", "list":
	default:
		p.fail("PROXY_MODE must be one of: off, rotating, list")
	}
	switch c.ProxyPolicy {
	case "direct_first", "proxy_first", "always":
	default:
		p.fail("PROXY_POLICY must be one of: direct_first, proxy_first, always")
	}
	if c.ProxyMode == "rotating" {
		if c.ProxyEndpoint == "" {
			p.fail("PROXY_ENDPOINT is required when PROXY_MODE=rotating")
		} else if u, err := url.Parse(c.ProxyEndpoint); err != nil || u.Host == "" ||
			(u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") {
			p.fail("PROXY_ENDPOINT must be an http(s):// or socks5:// URL with a host")
		}
	}
	if c.ProxyMode == "list" {
		if c.ProxyListFile == "" {
			p.fail("PROXY_LIST_FILE is required when PROXY_MODE=list")
		} else if _, err := os.Stat(c.ProxyListFile); err != nil {
			p.fail("PROXY_LIST_FILE: %v", err)
		}
	}
	if c.ProxyHostRulesFile != "" {
		if _, err := os.Stat(c.ProxyHostRulesFile); err != nil {
			p.fail("PROXY_HOST_RULES: %v", err)
		}
	}
	if c.ProxyMaxRetries < 0 || c.ProxyMaxRetries > 3 {
		p.fail("PROXY_MAX_RETRIES must be within [0,3]")
	}
	if c.ProxyMaxConnsPerProxy < 1 {
		p.fail("PROXY_MAX_CONNS_PER_PROXY must be >= 1")
	}
	if c.ProxyConnectTimeout <= 0 || c.ProxyHostMemoryTTL <= 0 {
		p.fail("PROXY_CONNECT_TIMEOUT and PROXY_HOST_MEMORY_TTL must be > 0")
	}
	if c.ResearchEnabled && c.LLMBaseURL != "" {
		if u, err := url.Parse(c.LLMBaseURL); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			p.fail("LLM_BASE_URL must be an http(s) URL")
		}
	}
	switch strings.ToLower(c.LLMThinking) {
	case "auto", "on", "off":
	default:
		p.fail("LLM_THINKING must be auto|on|off")
	}
	if c.LLMContextTokens < 1024 || c.LLMMaxOutputTokens < 128 || c.LLMMaxOutputTokens >= c.LLMContextTokens {
		p.fail("LLM_CONTEXT_TOKENS must be >=1024 and LLM_MAX_OUTPUT_TOKENS in [128, context)")
	}
	if c.LLMConnectTimeout <= 0 || c.LLMConnectTimeout > 30*time.Second {
		p.fail("LLM_CONNECT_TIMEOUT must be within (0,30s]")
	}
	if c.LLMHealthTimeout < 500*time.Millisecond || c.LLMHealthTimeout > 30*time.Second {
		p.fail("LLM_HEALTH_TIMEOUT must be within [500ms,30s]")
	}
	if c.LLMAnswerTimeout < 200*time.Millisecond || c.LLMAnswerTimeout > 10*time.Second {
		p.fail("LLM_ANSWER_TIMEOUT must be within [200ms,10s]")
	}
	if c.LLMMaxConcurrency < 1 || c.LLMMaxConcurrency > 16 {
		p.fail("LLM_MAX_CONCURRENCY must be within [1,16]")
	}
	if c.ResearchCreateStatus != 200 && c.ResearchCreateStatus != 201 {
		p.fail("RESEARCH_CREATE_STATUS must be 200 or 201")
	}
	if c.ResearchEnabled {
		if c.LLMBaseURL == "" {
			p.fail("LLM_BASE_URL is required when RESEARCH_ENABLED=true")
		}
		if c.ResearchWorkers < 1 || c.ResearchWorkers > 16 {
			p.fail("RESEARCH_WORKERS must be within [1,16]")
		}
		if c.ResearchQueue < 1 || c.ResearchMaxJobsPerKey < 1 {
			p.fail("RESEARCH_QUEUE and RESEARCH_MAX_JOBS_PER_KEY must be >= 1")
		}
		if c.ResearchDeadlineMini <= 0 || c.ResearchDeadlinePro < c.ResearchDeadlineMini {
			p.fail("research deadlines must be > 0 and PRO >= MINI")
		}
	}
	if c.MaxRedirects < 0 || c.MaxRedirects > 20 {
		p.fail("MAX_REDIRECTS must be within [0,20]")
	}
	for _, entry := range c.SSRFAllowlist {
		e := strings.TrimSpace(entry)
		if e == "" {
			continue
		}
		if strings.Contains(e, "/") {
			if _, _, err := net.ParseCIDR(e); err != nil {
				p.fail("SSRF_ALLOWLIST entry %q is not a CIDR", entry)
			}
			continue
		}
		if net.ParseIP(e) == nil && (strings.ContainsAny(e, " /?#") || !strings.Contains(e, ".")) {
			p.fail("SSRF_ALLOWLIST entry %q is not an IP, CIDR or hostname", entry)
		}
	}
	for name, b := range map[string]BreakerConfig{
		"SEARXNG": c.SearxBreaker, "TEI": c.TEIBreaker, "BROWSER": c.BrowserBreaker,
	} {
		if b.Interval <= 0 || b.Timeout <= 0 {
			p.fail("%s_BREAKER interval/timeout must be > 0", name)
		}
	}
}

// Redacted returns a copy safe to log: secrets are replaced by their length.
func (c *Config) Redacted() map[string]any {
	keys := make([]string, len(c.APIKeys))
	for i, k := range c.APIKeys {
		keys[i] = fmt.Sprintf("len=%d", len(k))
	}
	return map[string]any{
		"addr": c.Addr, "request_deadline": c.RequestDeadline.String(),
		"api_keys": keys, "searxng": c.SearxURL, "tei": c.TEIURL,
		"browser": c.BrowserURL, "redis": strings.SplitN(c.RedisURL, "@", 2)[len(strings.SplitN(c.RedisURL, "@", 2))-1],
		"fetch_concurrency": c.FetchConcurrency, "browser_max_tabs": c.BrowserMaxTabs,
		"proxy_mode": c.ProxyMode, "proxy_policy": c.ProxyPolicy,
	}
}

type parser struct {
	getenv func(string) string
	errs   []error
}

func (p *parser) fail(format string, args ...any) {
	p.errs = append(p.errs, fmt.Errorf(format, args...))
}

func (p *parser) str(key, def string) string {
	if v := strings.TrimSpace(p.getenv(key)); v != "" {
		return v
	}
	return def
}

func (p *parser) int(key string, def int) int {
	raw := strings.TrimSpace(p.getenv(key))
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		p.fail("%s: %q is not an integer", key, raw)
		return def
	}
	return v
}

func (p *parser) float(key string, def float64) float64 {
	raw := strings.TrimSpace(p.getenv(key))
	if raw == "" {
		return def
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		p.fail("%s: %q is not a number", key, raw)
		return def
	}
	return v
}

// onoff accepts on/off as well as true/false (PROXY_SEARXNG uses on|off).
func (p *parser) onoff(key string, def bool) bool {
	raw := strings.ToLower(strings.TrimSpace(p.getenv(key)))
	if raw == "" {
		return def
	}
	switch raw {
	case "on", "true", "1", "yes":
		return true
	case "off", "false", "0", "no":
		return false
	}
	p.fail("%s: %q is not on|off", key, raw)
	return def
}

func (p *parser) boolean(key string, def bool) bool {
	raw := strings.TrimSpace(p.getenv(key))
	if raw == "" {
		return def
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		p.fail("%s: %q is not a boolean", key, raw)
		return def
	}
	return v
}

func (p *parser) dur(key string, def time.Duration) time.Duration {
	raw := strings.TrimSpace(p.getenv(key))
	if raw == "" {
		return def
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		p.fail("%s: %q is not a duration (e.g. 1500ms, 6s)", key, raw)
		return def
	}
	return v
}

func (p *parser) bytes(key string, def int64) int64 {
	raw := strings.TrimSpace(p.getenv(key))
	if raw == "" {
		return def
	}
	const unit = int64(1 << 10)
	mult := unit
	switch strings.ToUpper(raw[len(raw)-1:]) {
	case "K":
		mult = unit
		raw = raw[:len(raw)-1]
	case "M":
		mult = unit * unit
		raw = raw[:len(raw)-1]
	case "G":
		mult = unit * unit * unit
		raw = raw[:len(raw)-1]
	}
	v, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		p.fail("%s: %q is not a byte size (e.g. 5242880, 5M)", key, raw)
		return def
	}
	return v * mult
}

func (p *parser) list(key string, def []string) []string {
	raw := strings.TrimSpace(p.getenv(key))
	if raw == "" {
		return def
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if s := strings.TrimSpace(part); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (p *parser) level(key string, def slog.Level) slog.Level {
	raw := strings.TrimSpace(p.getenv(key))
	if raw == "" {
		return def
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(raw)); err != nil {
		p.fail("%s: %q is not a log level (debug|info|warn|error)", key, raw)
		return def
	}
	return lvl
}
