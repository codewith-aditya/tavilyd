package rerank

import (
	"context"
	"log/slog"
	"sync/atomic"

	"github.com/astralabs/tavilyd/internal/observability"
)

// Scorer prefers the GPU cross-encoder and silently degrades to BM25.
type Scorer struct {
	primary  *TEIClient
	fallback *BM25
	log      *slog.Logger
	metrics  *observability.Metrics
	degraded atomic.Bool
}

// NewScorer wires the primary and fallback scorers.
func NewScorer(primary *TEIClient, log *slog.Logger, m *observability.Metrics) *Scorer {
	return &Scorer{primary: primary, fallback: NewBM25(), log: log, metrics: m}
}

// Rerank scores docs. It only returns an error if both backends fail (the BM25
// path is in-process and cannot fail in practice).
func (s *Scorer) Rerank(ctx context.Context, query string, docs []string) ([]float64, error) {
	if len(docs) == 0 {
		return nil, nil
	}
	if s.primary != nil {
		scores, err := s.primary.Rerank(ctx, query, docs)
		if err == nil {
			if s.degraded.Swap(false) && s.log != nil {
				s.log.Info("reranker recovered, leaving degraded mode")
			}
			if s.metrics != nil {
				s.metrics.RerankCalls.WithLabelValues("tei", "ok").Inc()
			}
			return scores, nil
		}
		first := s.degraded.Swap(true)
		if s.log != nil {
			if first {
				s.log.Warn("reranker unavailable, falling back to BM25 for this and following requests", "error", err)
			} else {
				s.log.Debug("reranker still unavailable", "error", err)
			}
		}
		if s.metrics != nil {
			s.metrics.RerankCalls.WithLabelValues("tei", "error").Inc()
		}
	}
	if s.metrics != nil {
		s.metrics.RerankCalls.WithLabelValues("bm25", "ok").Inc()
	}
	return s.fallback.Score(query, docs), nil
}

// Lexical scores docs with BM25 only (used when the deadline leaves no time for
// a network round-trip).
func (s *Scorer) Lexical(query string, docs []string) []float64 {
	return s.fallback.Score(query, docs)
}

// Degraded reports whether the last call used the fallback.
func (s *Scorer) Degraded() bool { return s.degraded.Load() }

// Health reports reranker status for /health.
func (s *Scorer) Health(ctx context.Context) (string, error) {
	if s.primary == nil {
		return "degraded", nil
	}
	if err := s.primary.Health(ctx); err != nil {
		return "degraded", err
	}
	return "ok", nil
}
