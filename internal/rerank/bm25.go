package rerank

import (
	"math"
	"strings"
	"unicode"
)

// BM25 is the lexical fallback scorer used when TEI is unreachable.
type BM25 struct {
	K1 float64
	B  float64
}

// NewBM25 returns a scorer with standard Okapi BM25 parameters.
func NewBM25() *BM25 { return &BM25{K1: 1.2, B: 0.75} }

// Score returns a 0..1 relevance score per document, preserving input order.
// Scores are squashed with s/(s+3) so a single strong hit does not pin at 1.0
// and weak hits remain visibly weak; ranking is unchanged by the squash.
func (b *BM25) Score(query string, docs []string) []float64 {
	out := make([]float64, len(docs))
	if len(docs) == 0 {
		return out
	}
	qTerms := tokenize(query)
	if len(qTerms) == 0 {
		return out
	}
	tokenized := make([][]string, len(docs))
	df := make(map[string]int, len(qTerms))
	lengths := make([]int, len(docs))
	totalLen := 0
	for i, d := range docs {
		tk := tokenize(d)
		tokenized[i] = tk
		lengths[i] = len(tk)
		totalLen += len(tk)
		seen := make(map[string]struct{}, len(qTerms))
		for _, t := range tk {
			if _, ok := seen[t]; ok {
				continue
			}
			seen[t] = struct{}{}
			if contains(qTerms, t) {
				df[t]++
			}
		}
	}
	n := float64(len(docs))
	avgdl := float64(totalLen) / n
	if avgdl == 0 {
		return out
	}
	for i, tk := range tokenized {
		tf := make(map[string]int, len(qTerms))
		for _, t := range tk {
			tf[t]++
		}
		var score float64
		for _, q := range qTerms {
			f := float64(tf[q])
			if f == 0 {
				continue
			}
			idf := math.Log(1 + (n-float64(df[q])+0.5)/(float64(df[q])+0.5))
			denom := f + b.K1*(1-b.B+b.B*float64(lengths[i])/avgdl)
			score += idf * f * (b.K1 + 1) / denom
		}
		out[i] = score / (score + 3)
	}
	return out
}

var stopwords = map[string]struct{}{
	"a": {}, "an": {}, "the": {}, "and": {}, "or": {}, "but": {}, "if": {}, "then": {},
	"of": {}, "to": {}, "in": {}, "on": {}, "at": {}, "for": {}, "with": {}, "by": {},
	"is": {}, "are": {}, "was": {}, "were": {}, "be": {}, "been": {}, "being": {},
	"do": {}, "does": {}, "did": {}, "how": {}, "what": {}, "who": {}, "why": {}, "when": {},
	"where": {}, "which": {}, "this": {}, "that": {}, "these": {}, "those": {}, "it": {},
	"its": {}, "as": {}, "from": {}, "into": {}, "about": {}, "over": {}, "after": {},
	"before": {}, "between": {}, "can": {}, "could": {}, "should": {}, "would": {},
	"will": {}, "shall": {}, "may": {}, "might": {}, "must": {}, "not": {}, "no": {},
	"yes": {}, "make": {}, "made": {}, "making": {}, "home": {}, "best": {}, "top": {},
	"new": {}, "latest": {}, "today": {}, "get": {}, "got": {}, "use": {}, "using": {},
}

func tokenize(s string) []string {
	if s == "" {
		return nil
	}
	out := make([]string, 0, len(s)/6)
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			continue
		}
		if b.Len() > 0 {
			tok := b.String()
			b.Reset()
			if len(tok) >= 3 {
				if _, stop := stopwords[tok]; !stop {
					out = append(out, tok)
				}
			}
		}
	}
	if b.Len() > 0 {
		tok := b.String()
		if len(tok) >= 3 {
			if _, stop := stopwords[tok]; !stop {
				out = append(out, tok)
			}
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
