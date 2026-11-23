package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
)

// SearchKeyInput is the normalized form of a /search request that determines
// cache identity. Anything that changes results must be part of it.
type SearchKeyInput struct {
	Query              string   `json:"q"`
	SearchDepth        string   `json:"d"`
	MaxResults         int      `json:"n"`
	Topic              string   `json:"t"`
	TimeRange          string   `json:"tr,omitempty"`
	IncludeDomains     []string `json:"inc,omitempty"`
	ExcludeDomains     []string `json:"exc,omitempty"`
	IncludeRawContent  bool     `json:"raw"`
	IncludeImages      bool     `json:"img"`
	ChunksPerSource    int      `json:"chunks,omitempty"`
	RawText            bool     `json:"rawtext,omitempty"`
	IncludeDomainsMode string   `json:"dmode,omitempty"`
	ExactMatch         bool     `json:"exact,omitempty"`
	StartDate          string   `json:"start,omitempty"`
	EndDate            string   `json:"end,omitempty"`
	MaxAgeHours        *int     `json:"maxage,omitempty"`
	CacheVersion       string   `json:"v,omitempty"`
}

// Normalize lowercases and canonicalizes every field so that semantically equal
// requests map to one cache entry.
func (in SearchKeyInput) Normalize() SearchKeyInput {
	out := in
	out.Query = normalizeSpace(strings.ToLower(in.Query))
	out.SearchDepth = strings.ToLower(strings.TrimSpace(in.SearchDepth))
	if out.SearchDepth == "" {
		out.SearchDepth = "basic"
	}
	out.Topic = strings.ToLower(strings.TrimSpace(in.Topic))
	if out.Topic == "" {
		out.Topic = "general"
	}
	out.TimeRange = strings.ToLower(strings.TrimSpace(in.TimeRange))
	if out.MaxResults <= 0 {
		out.MaxResults = 5
	}
	if out.ChunksPerSource <= 0 {
		out.ChunksPerSource = 3
	}
	if out.IncludeDomainsMode == "" {
		out.IncludeDomainsMode = "restrict"
	}
	out.IncludeDomains = normalizeDomains(in.IncludeDomains)
	out.ExcludeDomains = normalizeDomains(in.ExcludeDomains)
	return out
}

// Key hashes the normalized input with SHA-256.
func (in SearchKeyInput) Key() string {
	n := in.Normalize()
	b, err := json.Marshal(n)
	if err != nil { // cannot happen for these types
		b = []byte(n.Query)
	}
	sum := sha256.Sum256(b)
	return "srch:" + hex.EncodeToString(sum[:])
}

// URLKey returns the cache key for an extracted page. variant distinguishes
// markdown with and without inline image syntax; version invalidates entries
// produced by an older cleanup pipeline.
func URLKey(rawURL, depth, variant, version string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(rawURL)) + "|" +
		strings.ToLower(depth) + "|" + variant + "|" + version))
	return "page:" + hex.EncodeToString(sum[:])
}

// NegativeKey returns the cache key for a failed extraction.
func NegativeKey(rawURL, version string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(rawURL)) + "|" + version))
	return "neg:" + hex.EncodeToString(sum[:])
}

func normalizeDomains(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, d := range in {
		d = strings.ToLower(strings.TrimSpace(d))
		d = strings.TrimPrefix(d, "http://")
		d = strings.TrimPrefix(d, "https://")
		d = strings.TrimPrefix(d, "www.")
		d = strings.TrimSuffix(d, "/")
		if d == "" {
			continue
		}
		if _, dup := seen[d]; dup {
			continue
		}
		seen[d] = struct{}{}
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

func normalizeSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
