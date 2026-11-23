package api

import (
	"context"
	"sync"
	"time"
)

// llmHealth keeps the LLM probe out of the /health request path. A background
// loop probes every interval; a dependency is only marked degraded after
// failureThreshold consecutive failures, and the snapshot is served from memory.
type llmHealth struct {
	probe     func(context.Context) error
	timeout   time.Duration
	interval  time.Duration
	threshold int

	mu       sync.Mutex
	failures int
	status   string
	detail   string
	checked  time.Time
}

func newLLMHealth(probe func(context.Context) error, timeout, interval time.Duration) *llmHealth {
	return &llmHealth{
		probe: probe, timeout: timeout, interval: interval, threshold: 3,
		status: "ok", detail: "probe pending",
	}
}

// Start launches the background prober. It probes immediately, then every
// interval until ctx is cancelled.
func (h *llmHealth) Start(ctx context.Context) {
	if h == nil || h.probe == nil {
		return
	}
	go func() {
		h.probeOnce(ctx)
		ticker := time.NewTicker(h.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				h.probeOnce(ctx)
			}
		}
	}()
}

// probeOnce runs one probe under its own timeout and updates the snapshot.
func (h *llmHealth) probeOnce(ctx context.Context) {
	probeCtx, cancel := context.WithTimeout(ctx, h.timeout)
	err := h.probe(probeCtx)
	cancel()

	h.mu.Lock()
	defer h.mu.Unlock()
	h.checked = time.Now()
	if err == nil {
		h.failures = 0
		h.status = "ok"
		h.detail = ""
		return
	}
	h.failures++
	h.detail = truncateReason(err.Error())
	if h.failures >= h.threshold {
		h.status = "degraded"
		return
	}
	// Below the threshold the optional dependency stays ok; the detail shows a
	// probe is failing so operators can see it before it flips.
	h.status = "ok"
}

func (h *llmHealth) snapshot() (status, detail string, checked time.Time) {
	if h == nil {
		return "ok", "", time.Time{}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.status, h.detail, h.checked
}
