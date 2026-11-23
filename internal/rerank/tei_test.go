package rerank

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sony/gobreaker"
)

func TestBM25RanksRelevantFirst(t *testing.T) {
	b := NewBM25()
	docs := []string{
		"Postgres connection pooling with pgbouncer and prepared statements.",
		"Go concurrency: goroutines, channels, and the scheduler explained.",
		"Baking sourdough bread at home with a starter.",
		"Golang concurrency patterns: worker pools and context cancellation.",
	}
	scores := b.Score("golang concurrency patterns", docs)
	if len(scores) != len(docs) {
		t.Fatalf("score count mismatch")
	}
	if !(scores[3] > scores[1] && scores[1] > scores[0]) {
		t.Fatalf("unexpected ranking: %v", scores)
	}
	for i, s := range scores {
		if s < 0 || s > 1 {
			t.Fatalf("score %d out of range: %v", i, s)
		}
	}
}

func TestBM25EmptyInputs(t *testing.T) {
	b := NewBM25()
	if got := b.Score("", []string{"x"}); len(got) != 1 || got[0] != 0 {
		t.Fatalf("unexpected: %v", got)
	}
	if got := b.Score("x", nil); len(got) != 0 {
		t.Fatalf("unexpected: %v", got)
	}
}

func TestNormalizeScores(t *testing.T) {
	in := []float64{0.2, 0.8}
	out := NormalizeScores(in)
	if out[0] != 0.2 || out[1] != 0.8 {
		t.Fatalf("in-range scores must be preserved: %v", out)
	}
	logits := NormalizeScores([]float64{-2, 0, 2})
	if logits[0] > logits[1] || logits[1] > logits[2] {
		t.Fatalf("sigmoid must be monotonic: %v", logits)
	}
	if logits[0] < 0.1 || logits[0] > 0.13 || logits[2] < 0.87 || logits[2] > 0.89 {
		t.Fatalf("unexpected sigmoid values: %v", logits)
	}
	if got := NormalizeScores([]float64{0.5, 1.7, -0.3}); got[1] <= got[0] || got[0] <= got[2] {
		t.Fatalf("mixed raw scores must be squashed monotonically: %v", got)
	}
}

func TestTEIClientBatchesAndMapsIndices(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		calls.Add(1)
		var req rerankRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode: %v", err)
		}
		// Return scores in reverse order to prove index-based mapping.
		out := make([]rerankScore, 0, len(req.Texts))
		for i := len(req.Texts) - 1; i >= 0; i-- {
			out = append(out, rerankScore{Index: i, Score: float64(i + 1)})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()

	c := &TEIClient{
		base: srv.URL, http: srv.Client(), timeout: 2 * time.Second, maxBatch: 2, parallelism: 3,
		breaker: gobreaker.NewCircuitBreaker(gobreaker.Settings{Name: "test"}),
	}
	docs := []string{"d0", "d1", "d2", "d3", "d4"}
	scores, err := c.Rerank(context.Background(), "q", docs)
	if err != nil {
		t.Fatalf("rerank: %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("expected 3 batches, got %d", calls.Load())
	}
	if len(scores) != len(docs) {
		t.Fatalf("score count %d, want %d", len(scores), len(docs))
	}
	// Sigmoid of 1..2 within each batch, mapped to the right positions.
	if !(scores[1] > scores[0] && scores[3] > scores[2] && scores[0] > 0.7) {
		t.Fatalf("scores not mapped by index: %v", scores)
	}
	if err := c.Health(context.Background()); err != nil {
		t.Fatalf("health: %v", err)
	}
}
