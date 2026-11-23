package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/astralabs/tavilyd/internal/config"
	"github.com/astralabs/tavilyd/internal/llm"
)

func probeClient(t *testing.T, url string) *llm.Client {
	t.Helper()
	cfg := &config.Config{
		LLMBaseURL:        url,
		LLMModel:          "fake",
		LLMTimeout:        10 * time.Second,
		LLMConnectTimeout: 5 * time.Second,
		LLMHealthTimeout:  5 * time.Second,
		LLMMaxConcurrency: 2,
	}
	c, err := llm.New(cfg, nil)
	if err != nil {
		t.Fatalf("llm.New: %v", err)
	}
	return c
}

// A slow LLM (1.5 s) must not flip health to degraded: the probe timeout is 5 s
// and the monitor only degrades after three consecutive failures.
func TestHealthSlowLLMNotDegraded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	h := newLLMHealth(probeClient(t, srv.URL).Probe, 5*time.Second, 30*time.Second)
	h.probeOnce(context.Background())
	status, detail, _ := h.snapshot()
	if status != "ok" {
		t.Fatalf("slow but successful LLM marked %q (%s)", status, detail)
	}
	if h.failures != 0 {
		t.Fatalf("failures = %d, want 0", h.failures)
	}
}

// Three consecutive failed probes flip the optional dependency to degraded;
// one or two failures must not.
func TestHealthLLMDegradedAfterThreeFailures(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	h := newLLMHealth(probeClient(t, srv.URL).Probe, 5*time.Second, 30*time.Second)
	ctx := context.Background()
	h.probeOnce(ctx)
	h.probeOnce(ctx)
	if status, _, _ := h.snapshot(); status != "ok" {
		t.Fatalf("degraded after 2 failures, want ok (threshold 3)")
	}
	h.probeOnce(ctx)
	status, detail, _ := h.snapshot()
	if status != "degraded" {
		t.Fatalf("status after 3 failures = %q (%s), want degraded", status, detail)
	}
	if calls.Load() < 3 {
		t.Fatalf("probe calls = %d, want >=3", calls.Load())
	}
}

func TestOverallStatusCoreOnly(t *testing.T) {
	ok := map[string]depStatus{"redis": {Status: "ok"}, "searxng": {Status: "ok"}}
	if got := overallStatus(ok); got != "ok" {
		t.Fatalf("healthy core = %q", got)
	}
	down := map[string]depStatus{"redis": {Status: "ok"}, "searxng": {Status: "down"}}
	if got := overallStatus(down); got != "down" {
		t.Fatalf("down core = %q", got)
	}
	slow := map[string]depStatus{"redis": {Status: "ok"}, "searxng": {Status: "degraded"}}
	if got := overallStatus(slow); got != "degraded" {
		t.Fatalf("slow core = %q", got)
	}
}
