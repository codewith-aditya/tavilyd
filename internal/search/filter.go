package search

import (
	"errors"
	"strings"
)

var errEmptyHost = errors.New("url has no host")

// BuildQuery appends site: operators for included domains. Engines that honour
// it pre-filter, which measurably reduces wasted extraction work.
func BuildQuery(query string, include []string) string {
	q := strings.TrimSpace(query)
	if len(include) == 0 {
		return q
	}
	var b strings.Builder
	b.WriteString(q)
	seen := make(map[string]struct{}, len(include))
	for _, d := range include {
		d = normalizeDomain(d)
		if d == "" {
			continue
		}
		if _, dup := seen[d]; dup {
			continue
		}
		seen[d] = struct{}{}
		b.WriteString(" site:")
		b.WriteString(d)
	}
	return b.String()
}

// HostAllowed matches host against a domain list, accepting subdomains.
func HostAllowed(host string, domains []string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	host = strings.TrimPrefix(host, "www.")
	if len(domains) == 0 {
		return true
	}
	for _, d := range domains {
		d = normalizeDomain(d)
		if d == "" {
			continue
		}
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// Filter applies include/exclude lists to results. include is usually already
// enforced by the search engine, but is re-checked here because engines may
// ignore site: operators.
func Filter(results []Result, include, exclude []string) []Result {
	out := results[:0]
	for _, r := range results {
		host := hostOf(r.URL)
		if host == "" {
			continue
		}
		if len(include) > 0 && !HostAllowed(host, include) {
			continue
		}
		if len(exclude) > 0 && HostAllowed(host, exclude) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// Dedupe removes duplicate URLs after canonicalization, keeping the first
// (highest ranked) occurrence and upgrading empty titles/snippets.
func Dedupe(results []Result) []Result {
	seen := make(map[string]int, len(results))
	out := make([]Result, 0, len(results))
	for _, r := range results {
		key, err := Canonical(r.URL)
		if err != nil {
			continue
		}
		if idx, dup := seen[key]; dup {
			if out[idx].Title == "" {
				out[idx].Title = r.Title
			}
			if len(out[idx].Content) < len(r.Content) {
				out[idx].Content = r.Content
			}
			continue
		}
		r.URL = key
		seen[key] = len(out)
		out = append(out, r)
	}
	return out
}

func normalizeDomain(d string) string {
	d = strings.ToLower(strings.TrimSpace(d))
	d = strings.TrimPrefix(d, "http://")
	d = strings.TrimPrefix(d, "https://")
	d = strings.TrimPrefix(d, "www.")
	return strings.TrimSuffix(d, "/")
}

func hostOf(raw string) string {
	if i := strings.Index(raw, "://"); i >= 0 {
		raw = raw[i+3:]
	}
	if i := strings.IndexAny(raw, "/?#"); i >= 0 {
		raw = raw[:i]
	}
	if i := strings.LastIndex(raw, "@"); i >= 0 {
		raw = raw[i+1:]
	}
	if h, _, found := strings.Cut(raw, ":"); found {
		raw = h
	}
	return strings.TrimPrefix(strings.ToLower(raw), "www.")
}
