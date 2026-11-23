package proxy

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Route is one policy decision.
type Route struct {
	UseProxy bool
	Proxy    *Proxy
	Reason   string
}

// Engine implements PROXY_POLICY and per-host memory.
type Engine struct {
	mu       sync.Mutex
	conns    map[string]chan struct{}
	maxConns int
	mode     string
	policy   string
	pool     Pool
	rules    []Rule
	mem      *hostMemory
	metrics  Metrics
	degraded atomic.Bool
	now      func() time.Time
}

// NewEngine builds the policy engine. mode=off disables proxying entirely.
func NewEngine(mode, policy string, pool Pool, rules []Rule, memoryTTL time.Duration, m Metrics) *Engine {
	if m == nil {
		m = nopMetrics{}
	}
	if memoryTTL <= 0 {
		memoryTTL = time.Hour
	}
	return &Engine{
		mode: mode, policy: policy, pool: pool, rules: rules,
		mem:     &hostMemory{ttl: memoryTTL, entries: make(map[string]memEntry)},
		metrics: m, now: time.Now, maxConns: 64, conns: make(map[string]chan struct{}),
	}
}

// SetMaxConns bounds concurrent tunnels per proxy endpoint.
func (e *Engine) SetMaxConns(n int) {
	if n < 1 {
		n = 64
	}
	e.mu.Lock()
	e.maxConns = n
	e.mu.Unlock()
}

// Acquire reserves a connection slot for p, blocking until ctx expires.
func (e *Engine) Acquire(ctx context.Context, p *Proxy) error {
	if e == nil || p == nil {
		return nil
	}
	e.mu.Lock()
	ch, ok := e.conns[p.HostPort()]
	if !ok {
		ch = make(chan struct{}, e.maxConns)
		e.conns[p.HostPort()] = ch
	}
	e.mu.Unlock()
	select {
	case ch <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Release frees a slot acquired for p.
func (e *Engine) Release(p *Proxy) {
	if e == nil || p == nil {
		return
	}
	e.mu.Lock()
	ch := e.conns[p.HostPort()]
	e.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case <-ch:
	default:
	}
}

// Enabled reports whether the engine can proxy at all.
func (e *Engine) Enabled() bool { return e != nil && e.mode != "off" && e.pool != nil }

// Decide returns the route for a host. It never fails: an unhealthy pool
// degrades to direct (with Degraded() set) so callers never return a 500.
func (e *Engine) Decide(ctx context.Context, host, session string) Route {
	if e == nil || !e.Enabled() {
		return Route{Reason: "proxy_off"}
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if action, ok := MatchRule(e.rules, host); ok {
		if action == RuleDirect {
			return Route{Reason: "host_rule_direct"}
		}
		return e.pickProxy(ctx, host, session, "host_rule_proxy")
	}
	if e.mem.preferProxy(host, e.now()) {
		return e.pickProxy(ctx, host, session, "host_memory_blocked")
	}
	switch e.policy {
	case "always":
		return e.pickProxy(ctx, host, session, "policy_always")
	case "proxy_first":
		return e.pickProxy(ctx, host, session, "policy_proxy_first")
	default: // direct_first
		if e.mem.preferDirect(host, e.now()) {
			return Route{Reason: "host_memory_friendly"}
		}
		return Route{Reason: "policy_direct_first"}
	}
}

// DecideProxy forces a proxy route (used for the single retry after a direct
// request is blocked).
func (e *Engine) DecideProxy(ctx context.Context, host, session, reason string) Route {
	if e == nil || !e.Enabled() {
		return Route{Reason: "proxy_off"}
	}
	return e.pickProxy(ctx, host, session, reason)
}

func (e *Engine) pickProxy(ctx context.Context, host, session, reason string) Route {
	if e.pool == nil {
		return Route{Reason: "no_pool"}
	}
	p, err := e.pool.Pick(ctx, host, session)
	if err != nil {
		e.degraded.Store(true)
		return Route{Reason: "pool_unhealthy"}
	}
	e.degraded.Store(false)
	return Route{UseProxy: true, Proxy: p, Reason: reason}
}

// Report feeds one outcome into the pool and the per-host memory.
func (e *Engine) Report(host string, p *Proxy, outcome Outcome, latency time.Duration, blocked bool) {
	if e == nil || !e.Enabled() {
		return
	}
	if p != nil {
		e.pool.Report(p, host, outcome, latency)
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	switch {
	case blocked:
		e.mem.markProxy(host, e.now())
	case p == nil && outcome == OutcomeSuccess:
		e.mem.markDirect(host, e.now())
	}
}

// Degraded reports whether the last decision had to fall back to direct.
func (e *Engine) Degraded() bool { return e != nil && e.degraded.Load() }

// Snapshot exposes pool health without credentials.
func (e *Engine) Snapshot() Snapshot {
	if e == nil || e.pool == nil {
		return Snapshot{Mode: "off"}
	}
	s := e.pool.Snapshot()
	s.Mode = e.mode
	// Degraded must reflect the pool now, not a stale failure from an earlier
	// pick. direct_first can go a long time without picking a proxy, so OR-ing
	// the engine's last-pick flag used to leave /health degraded forever even
	// after the proxy recovered.
	if s.Healthy == 0 {
		s.Degraded = true
	}
	return s
}

// Healthy reports whether the pool can currently serve at least one proxy.
func (e *Engine) Healthy() bool {
	s := e.Snapshot()
	return s.Mode == "off" || s.Healthy > 0
}

// Close releases pool resources.
func (e *Engine) Close() error {
	if e == nil || e.pool == nil {
		return nil
	}
	return e.pool.Close()
}

type memEntry struct {
	proxyUntil  time.Time
	directUntil time.Time
}

type hostMemory struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries map[string]memEntry
	now     func() time.Time
}

func (h *hostMemory) preferProxy(host string, now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	e, ok := h.entries[host]
	if !ok {
		return false
	}
	if now.After(e.proxyUntil) {
		delete(h.entries, host)
		return false
	}
	return true
}

func (h *hostMemory) preferDirect(host string, now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	e, ok := h.entries[host]
	if !ok {
		return false
	}
	if now.After(e.directUntil) {
		delete(h.entries, host)
		return false
	}
	return true
}

func (h *hostMemory) markProxy(host string, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	e := h.entries[host]
	e.proxyUntil = now.Add(h.ttl)
	h.entries[host] = e
}

func (h *hostMemory) markDirect(host string, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	e := h.entries[host]
	e.directUntil = now.Add(h.ttl)
	h.entries[host] = e
}

// DecisionMemoryLen is used by tests.
func (e *Engine) DecisionMemoryLen() int {
	if e == nil {
		return 0
	}
	e.mem.mu.Lock()
	defer e.mem.mu.Unlock()
	return len(e.mem.entries)
}
