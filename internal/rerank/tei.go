package rerank

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/sony/gobreaker"
	"golang.org/x/sync/errgroup"

	"github.com/astralabs/tavilyd/internal/config"
)

// TEIClient talks to Hugging Face Text Embeddings Inference's /rerank endpoint.
type TEIClient struct {
	base        string
	http        *http.Client
	timeout     time.Duration
	maxBatch    int
	parallelism int
	model       string
	breaker     *gobreaker.CircuitBreaker
}

// NewTEIClient builds the GPU reranker client.
func NewTEIClient(cfg *config.Config, hc *http.Client) *TEIClient {
	if hc == nil {
		hc = &http.Client{Timeout: cfg.TEITimeout}
	}
	return &TEIClient{
		base:        strings.TrimRight(cfg.TEIURL, "/"),
		http:        hc,
		timeout:     cfg.TEITimeout,
		maxBatch:    cfg.TEIMaxBatch,
		parallelism: cfg.TEIParallelism,
		model:       cfg.TEIModel,
		breaker: gobreaker.NewCircuitBreaker(gobreaker.Settings{
			Name:        "tei",
			MaxRequests: cfg.TEIBreaker.MaxRequests,
			Interval:    cfg.TEIBreaker.Interval,
			Timeout:     cfg.TEIBreaker.Timeout,
			ReadyToTrip: func(c gobreaker.Counts) bool {
				return c.ConsecutiveFailures >= 3
			},
		}),
	}
}

type rerankRequest struct {
	Query      string   `json:"query"`
	Texts      []string `json:"texts"`
	Truncate   bool     `json:"truncate"`
	RawScores  bool     `json:"raw_scores"`
	ReturnText bool     `json:"return_text"`
}

type rerankScore struct {
	Index int     `json:"index"`
	Score float64 `json:"score"`
	Text  string  `json:"text,omitempty"`
}

// Rerank scores docs against query, batching to the TEI limit with bounded
// parallelism. The returned slice matches the input order and is normalized to
// 0..1 (sigmoid applied when the server returns raw logits).
func (t *TEIClient) Rerank(ctx context.Context, query string, docs []string) ([]float64, error) {
	if len(docs) == 0 {
		return nil, nil
	}
	scores := make([]float64, len(docs))
	batches := (len(docs) + t.maxBatch - 1) / t.maxBatch
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(max(1, t.parallelism))
	for b := 0; b < batches; b++ {
		start := b * t.maxBatch
		end := min(start+t.maxBatch, len(docs))
		g.Go(func() error {
			part, err := t.rerankBatch(gctx, query, docs[start:end])
			if err != nil {
				return err
			}
			copy(scores[start:end], part)
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return NormalizeScores(scores), nil
}

func (t *TEIClient) rerankBatch(ctx context.Context, query string, docs []string) ([]float64, error) {
	payload, err := json.Marshal(rerankRequest{Query: query, Texts: docs, Truncate: true, RawScores: false})
	if err != nil {
		return nil, err
	}
	raw, err := t.breaker.Execute(func() (any, error) {
		rctx, cancel := context.WithTimeout(ctx, t.timeout)
		defer cancel()
		req, err := http.NewRequestWithContext(rctx, http.MethodPost, t.base+"/rerank", bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := t.http.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("tei status %d: %s", resp.StatusCode, truncate(body, 256))
		}
		return body, nil
	})
	if err != nil {
		return nil, err
	}
	var parsed []rerankScore
	if err := json.Unmarshal(raw.([]byte), &parsed); err != nil {
		return nil, fmt.Errorf("decode tei response: %w", err)
	}
	if len(parsed) != len(docs) {
		return nil, fmt.Errorf("tei returned %d scores for %d docs", len(parsed), len(docs))
	}
	out := make([]float64, len(docs))
	for i, s := range parsed {
		idx := s.Index
		if idx < 0 || idx >= len(out) {
			idx = i
		}
		out[idx] = s.Score
	}
	return out, nil
}

// Health probes TEI's /health endpoint.
func (t *TEIClient) Health(ctx context.Context) error {
	if t.breaker.State() == gobreaker.StateOpen {
		return fmt.Errorf("tei circuit open")
	}
	rctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, t.base+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := t.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<14))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("tei health status %d", resp.StatusCode)
	}
	return nil
}

// NormalizeScores maps arbitrary cross-encoder outputs to 0..1. Scores already
// inside the range are clamped; raw logits get a logistic squash.
func NormalizeScores(scores []float64) []float64 {
	if len(scores) == 0 {
		return scores
	}
	raw := false
	for _, s := range scores {
		if s < 0 || s > 1 {
			raw = true
			break
		}
	}
	out := make([]float64, len(scores))
	for i, s := range scores {
		if math.IsNaN(s) || math.IsInf(s, 0) {
			out[i] = 0
			continue
		}
		if raw {
			out[i] = 1 / (1 + math.Exp(-s))
		} else {
			out[i] = math.Min(1, math.Max(0, s))
		}
	}
	return out
}

func truncate(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
