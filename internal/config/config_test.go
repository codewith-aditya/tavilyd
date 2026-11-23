package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := LoadFrom(envMap(map[string]string{}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Addr != ":8080" || cfg.RequestDeadline != 6*time.Second {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if cfg.ChunkTokens != 350 || cfg.ChunkOverlap != 50 || cfg.MaxChunksTotal != 200 {
		t.Fatalf("chunk defaults wrong: %+v", cfg)
	}
	if cfg.FetchMaxBytes != 5<<20 {
		t.Fatalf("fetch max bytes: %d", cfg.FetchMaxBytes)
	}
	if cfg.BrowserMaxTabs != 8 || cfg.FetchConcurrency != 12 {
		t.Fatalf("concurrency defaults wrong")
	}
}

func TestLoadOverrides(t *testing.T) {
	cfg, err := LoadFrom(envMap(map[string]string{
		"HTTP_ADDR":           "0.0.0.0:9000",
		"REQUEST_DEADLINE":    "8s",
		"FETCH_MAX_BYTES":     "2M",
		"CHUNK_TOKENS":        "500",
		"CHUNK_OVERLAP":       "80",
		"API_KEY":             "one,two",
		"LOG_LEVEL":           "debug",
		"RATE_LIMIT_RPS":      "25.5",
		"BROWSER_MAX_TABS":    "4",
		"TEI_MAX_BATCH":       "16",
		"MAX_CHUNKS_PER_PAGE": "10",
		"MAX_CHUNKS_TOTAL":    "40",
		"ALLOW_PRIVATE_IPS":   "true",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.FetchMaxBytes != 2<<20 {
		t.Fatalf("bytes parse: %d", cfg.FetchMaxBytes)
	}
	if len(cfg.APIKeys) != 2 || cfg.APIKeys[1] != "two" {
		t.Fatalf("api keys: %v", cfg.APIKeys)
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Fatalf("log level: %v", cfg.LogLevel)
	}
	if cfg.RateLimitRPS != 25.5 || cfg.ChunkTokens != 500 || cfg.ChunkOverlap != 80 {
		t.Fatalf("numeric overrides wrong")
	}
	if !cfg.AllowPrivateIPs {
		t.Fatal("boolean override ignored")
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	cases := map[string]map[string]string{
		"bad duration":     {"REQUEST_DEADLINE": "soon"},
		"bad int":          {"FETCH_CONCURRENCY": "x"},
		"overlap too big":  {"CHUNK_OVERLAP": "400", "CHUNK_TOKENS": "350"},
		"bad redis url":    {"REDIS_URL": "http://nope"},
		"bad searx url":    {"SEARXNG_URL": "not a url"},
		"bad log level":    {"LOG_LEVEL": "verbose"},
		"zero concurrency": {"FETCH_CONCURRENCY": "0"},
		"tabs too high":    {"BROWSER_MAX_TABS": "1000"},
		"deadline too big": {"REQUEST_DEADLINE": "120s"},
		"bad bytes":        {"FETCH_MAX_BYTES": "lots"},
		"bad link density": {"CLEAN_LINK_DENSITY": "1.5"},
		"bad min alpha":    {"CLEAN_MIN_ALPHA": "-0.1"},
		"bad token cap":    {"CLEAN_MAX_TOKEN_CHARS": "10"},
		"missing cta file": {"CLEAN_CTA_PHRASES_FILE": "/nonexistent/cta.txt"},
		"chunk totals":     {"MAX_CHUNKS_TOTAL": "5", "MAX_CHUNKS_PER_PAGE": "10"},
	}
	for name, env := range cases {
		if _, err := LoadFrom(envMap(env)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestLoadRejectsInvalidAddr(t *testing.T) {
	if _, err := LoadFrom(envMap(map[string]string{"HTTP_ADDR": "8080"})); err == nil {
		t.Fatal("expected invalid addr to fail")
	}
}

func TestRedactedHidesKeys(t *testing.T) {
	cfg, err := LoadFrom(envMap(map[string]string{"API_KEY": "supersecret", "REDIS_URL": "redis://:pass@redis:6379/0"}))
	if err != nil {
		t.Fatal(err)
	}
	red := cfg.Redacted()
	keys, _ := red["api_keys"].([]string)
	if len(keys) != 1 || strings.Contains(keys[0], "supersecret") {
		t.Fatalf("api key leaked: %v", keys)
	}
	if s, _ := red["redis"].(string); strings.Contains(s, "pass") {
		t.Fatalf("redis password leaked: %s", s)
	}
}
