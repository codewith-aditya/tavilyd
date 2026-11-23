package security

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// RateLimiter applies an independent token bucket per API key.
type RateLimiter struct {
	mu      sync.Mutex
	limit   rate.Limit
	burst   int
	ttl     time.Duration
	now     func() time.Time
	buckets map[string]*bucket
}

type bucket struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// NewRateLimiter builds a limiter; rps <= 0 disables limiting (Allow always
// returns true). ttl controls how long idle buckets are retained.
func NewRateLimiter(rps float64, burst int, ttl time.Duration) *RateLimiter {
	return &RateLimiter{
		limit:   rate.Limit(rps),
		burst:   burst,
		ttl:     ttl,
		now:     time.Now,
		buckets: make(map[string]*bucket),
	}
}

// Enabled reports whether limiting is active.
func (r *RateLimiter) Enabled() bool { return r != nil && r.limit > 0 }

// Allow consumes one token for key. When refused it returns a Retry-After hint.
func (r *RateLimiter) Allow(key string) (bool, time.Duration) {
	if !r.Enabled() {
		return true, 0
	}
	r.mu.Lock()
	b, ok := r.buckets[key]
	now := r.now()
	if !ok {
		b = &bucket{limiter: rate.NewLimiter(r.limit, r.burst)}
		r.buckets[key] = b
	}
	b.lastSeen = now
	r.pruneLocked(now)
	r.mu.Unlock()

	if b.limiter.AllowN(now, 1) {
		return true, 0
	}
	res := b.limiter.ReserveN(now, 1)
	delay := res.DelayFrom(now)
	res.CancelAt(now)
	if delay <= 0 {
		delay = time.Second
	}
	return false, delay
}

// pruneLocked drops buckets idle longer than ttl to bound memory.
func (r *RateLimiter) pruneLocked(now time.Time) {
	if r.ttl <= 0 {
		return
	}
	for k, b := range r.buckets {
		if now.Sub(b.lastSeen) > r.ttl {
			delete(r.buckets, k)
		}
	}
}
