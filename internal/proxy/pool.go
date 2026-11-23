package proxy

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrNoHealthyProxy means every proxy is currently ejected.
var ErrNoHealthyProxy = errors.New("no healthy proxy available")

// Snapshot is the health view exposed by /health.
type Snapshot struct {
	Mode      string
	Total     int
	Healthy   int
	Degraded  bool
	LastProbe string
}

// Pool is the proxy selection surface.
type Pool interface {
	Pick(ctx context.Context, host, session string) (*Proxy, error)
	Report(p *Proxy, host string, outcome Outcome, latency time.Duration)
	Snapshot() Snapshot
	Close() error
}

type state struct {
	mu           sync.Mutex
	successes    int64
	failures     int64
	consecutive  int
	ewmaMS       float64
	ejectedUntil time.Time
	backoff      time.Duration
	lastOutcome  string
	lastProbe    time.Time
}

func (s *state) healthy(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !now.Before(s.ejectedUntil)
}

func (s *state) score(now time.Time) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if now.Before(s.ejectedUntil) {
		return -1
	}
	total := float64(s.successes + s.failures)
	rate := 0.9
	if total > 0 {
		rate = float64(s.successes) / total
	}
	latency := s.ewmaMS
	if latency <= 0 {
		latency = 500
	}
	return rate / (1 + latency/1000)
}

func (s *state) report(outcome Outcome, latency time.Duration, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastProbe = now
	s.lastOutcome = outcome.String()
	if s.ewmaMS == 0 {
		s.ewmaMS = float64(latency.Milliseconds())
	} else {
		s.ewmaMS = 0.8*s.ewmaMS + 0.2*float64(latency.Milliseconds())
	}
	switch outcome {
	case OutcomeSuccess:
		s.successes++
		s.consecutive = 0
		s.ejectedUntil = time.Time{}
		if s.backoff > 30*time.Second {
			s.backoff = 30 * time.Second
		}
	default:
		s.failures++
		s.consecutive++
		if s.consecutive >= 3 {
			if s.backoff == 0 {
				s.backoff = 30 * time.Second
			} else {
				s.backoff *= 2
				if s.backoff > 10*time.Minute {
					s.backoff = 10 * time.Minute
				}
			}
			s.ejectedUntil = now.Add(s.backoff)
		}
	}
}

// Rotating is a single endpoint (possibly rewriting the username per session).
type Rotating struct {
	base    *Proxy
	sticky  string
	metrics Metrics
	st      state
	closed  bool
	mu      sync.Mutex
}

// NewRotating builds a rotating pool.
func NewRotating(endpoint, sticky string, m Metrics, insecure ...bool) (*Rotating, error) {
	p, err := Parse(endpoint)
	if err != nil {
		return nil, err
	}
	if m == nil {
		m = nopMetrics{}
	}
	if len(insecure) > 0 {
		p.Insecure = insecure[0]
	}
	return &Rotating{base: p, sticky: sticky, metrics: m}, nil
}

// Pick renders the sticky username when configured.
func (r *Rotating) Pick(_ context.Context, _ string, session string) (*Proxy, error) {
	if r.base == nil || r.closed {
		return nil, ErrNoHealthyProxy
	}
	if !r.st.healthy(time.Now()) {
		return nil, ErrNoHealthyProxy
	}
	return RenderSticky(r.sticky, r.base, session)
}

// Report updates health and metrics.
func (r *Rotating) Report(p *Proxy, _ string, outcome Outcome, latency time.Duration) {
	r.st.report(outcome, latency, time.Now())
	r.metrics.ProxyRequest(outcome.String())
	r.metrics.ProxyLatency(latency)
	r.metrics.ProxyPoolSize("healthy", boolToInt(r.st.healthy(time.Now())))
	r.metrics.ProxyPoolSize("ejected", boolToInt(!r.st.healthy(time.Now())))
}

// Snapshot reports the rotating endpoint state.
func (r *Rotating) Snapshot() Snapshot {
	healthy := r.st.healthy(time.Now())
	n := 0
	if healthy {
		n = 1
	}
	r.st.mu.Lock()
	last := r.st.lastOutcome
	r.st.mu.Unlock()
	return Snapshot{Mode: "rotating", Total: 1, Healthy: n, Degraded: n == 0, LastProbe: last}
}

// Close marks the pool closed.
func (r *Rotating) Close() error { r.mu.Lock(); r.closed = true; r.mu.Unlock(); return nil }

// List is a file-backed pool, hot-reloaded on change or Reload().
type List struct {
	path     string
	sticky   string
	metrics  Metrics
	mu       sync.Mutex
	entries  []*entry
	modTime  time.Time
	stop     chan struct{}
	closed   bool
	insecure bool
}

type entry struct {
	proxy *Proxy
	st    state
}

// NewList loads the proxy list file and starts the file watcher.
func NewList(path, sticky string, m Metrics, insecure ...bool) (*List, error) {
	if m == nil {
		m = nopMetrics{}
	}
	insecureTLS := len(insecure) > 0 && insecure[0]
	l := &List{path: path, sticky: sticky, metrics: m, stop: make(chan struct{}), insecure: insecureTLS}
	if err := l.load(); err != nil {
		return nil, err
	}
	go l.watch()
	return l, nil
}

// Reload re-reads the file (SIGHUP hook).
func (l *List) Reload() error { return l.load() }

func (l *List) load() error {
	data, err := os.ReadFile(l.path)
	if err != nil {
		return fmt.Errorf("proxy list: %w", err)
	}
	info, _ := os.Stat(l.path)
	var entries []*entry
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		p, err := Parse(line)
		if err != nil {
			return fmt.Errorf("proxy list: %w", err)
		}
		p.Insecure = l.insecure
		entries = append(entries, &entry{proxy: p})
	}
	if len(entries) == 0 {
		return errors.New("proxy list is empty")
	}
	l.mu.Lock()
	l.entries = entries
	if info != nil {
		l.modTime = info.ModTime()
	}
	l.mu.Unlock()
	l.metrics.ProxyPoolSize("total", len(entries))
	return nil
}

func (l *List) watch() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-ticker.C:
			info, err := os.Stat(l.path)
			if err != nil {
				continue
			}
			l.mu.Lock()
			changed := info.ModTime().After(l.modTime)
			l.mu.Unlock()
			if changed {
				_ = l.load()
			}
		}
	}
}

// Pick chooses among healthy proxies with power-of-two-choices.
func (l *List) Pick(_ context.Context, _ string, session string) (*Proxy, error) {
	now := time.Now()
	l.mu.Lock()
	candidates := make([]*entry, 0, len(l.entries))
	for _, e := range l.entries {
		if e.st.healthy(now) {
			candidates = append(candidates, e)
		}
	}
	l.mu.Unlock()
	if len(candidates) == 0 {
		return nil, ErrNoHealthyProxy
	}
	chosen := candidates[rand.Intn(len(candidates))]
	if len(candidates) > 1 {
		other := candidates[rand.Intn(len(candidates))]
		if other != chosen && other.st.score(now) > chosen.st.score(now) {
			chosen = other
		}
	}
	return RenderSticky(l.sticky, chosen.proxy, session)
}

// Report updates the matching entry (by endpoint host:port).
func (l *List) Report(p *Proxy, _ string, outcome Outcome, latency time.Duration) {
	l.mu.Lock()
	var target *entry
	for _, e := range l.entries {
		if e.proxy.HostPort() == p.HostPort() {
			target = e
			break
		}
	}
	l.mu.Unlock()
	if target != nil {
		target.st.report(outcome, latency, time.Now())
	}
	l.metrics.ProxyRequest(outcome.String())
	l.metrics.ProxyLatency(latency)
	s := l.Snapshot()
	l.metrics.ProxyPoolSize("healthy", s.Healthy)
	l.metrics.ProxyPoolSize("total", s.Total)
}

// Snapshot counts healthy entries and the last probe outcome.
func (l *List) Snapshot() Snapshot {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	healthy := 0
	last := ""
	for _, e := range l.entries {
		if e.st.healthy(now) {
			healthy++
		}
		e.st.mu.Lock()
		if e.st.lastProbe.After(time.Time{}) {
			last = e.st.lastOutcome
		}
		e.st.mu.Unlock()
	}
	return Snapshot{Mode: "list", Total: len(l.entries), Healthy: healthy, Degraded: healthy == 0, LastProbe: last}
}

// Close stops the watcher.
func (l *List) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.closed {
		close(l.stop)
		l.closed = true
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// SortEndpoints returns the endpoints sorted by host:port (tests/debug only,
// never includes credentials).
func SortEndpoints(ps []*Proxy) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.HostPort())
	}
	sort.Strings(out)
	return out
}
