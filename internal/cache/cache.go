// Package cache implements the two-tier cache (in-process LRU + Redis) with
// single-flight de-duplication. Redis failures degrade to "no cache": reads
// report a miss, writes are dropped, and the request continues.
package cache

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"

	"github.com/astralabs/tavilyd/internal/config"
	"github.com/astralabs/tavilyd/internal/observability"
)

// Cache is the cache facade used by handlers.
type Cache struct {
	rdb     *redis.Client
	lru     *lru
	sf      singleflight.Group
	timeout time.Duration
	log     *slog.Logger
	metrics *observability.Metrics
	up      atomic.Bool
}

// New builds the cache. A Redis connection failure does not abort startup; the
// service runs from the LRU and reports Redis as down.
func New(cfg *config.Config, log *slog.Logger, metrics *observability.Metrics) (*Cache, error) {
	opts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return nil, fmt.Errorf("redis url: %w", err)
	}
	opts.MaxRetries = 1
	opts.DialTimeout = cfg.RedisTimeout
	opts.ReadTimeout = cfg.RedisTimeout
	opts.WriteTimeout = cfg.RedisTimeout
	opts.PoolSize = 64
	opts.MinIdleConns = 4
	opts.PoolTimeout = cfg.RedisTimeout

	c := &Cache{
		rdb:     redis.NewClient(opts),
		lru:     newLRU(cfg.LRUEntries),
		timeout: cfg.RedisTimeout,
		log:     log,
		metrics: metrics,
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.RedisTimeout*4)
	defer cancel()
	if err := c.rdb.Ping(ctx).Err(); err != nil {
		log.Warn("redis unavailable at startup, running cacheless", "error", err)
		c.up.Store(false)
	} else {
		c.up.Store(true)
	}
	return c, nil
}

// Get looks in the LRU first, then Redis. Redis errors are swallowed.
func (c *Cache) Get(ctx context.Context, key string) ([]byte, bool) {
	if b, ok := c.lru.get(key); ok {
		c.metrics.CacheLookups.WithLabelValues("hit_l1").Inc()
		return b, true
	}
	if !c.up.Load() {
		c.metrics.CacheLookups.WithLabelValues("miss").Inc()
		return nil, false
	}
	rctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	b, err := c.rdb.Get(rctx, key).Bytes()
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			c.markDown(err)
			c.metrics.CacheLookups.WithLabelValues("error").Inc()
			return nil, false
		}
		c.metrics.CacheLookups.WithLabelValues("miss").Inc()
		return nil, false
	}
	c.up.Store(true)
	c.metrics.CacheLookups.WithLabelValues("hit_redis").Inc()
	// Rehydrate L1 with a short local TTL: Redis remains the source of truth.
	c.lru.set(key, b, 2*time.Minute)
	return b, true
}

// Set writes through to both tiers. ttl <= 0 disables caching.
func (c *Cache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) {
	if ttl <= 0 || len(value) == 0 {
		return
	}
	c.lru.set(key, value, ttl)
	if !c.up.Load() {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if err := c.rdb.Set(rctx, key, value, ttl).Err(); err != nil {
		c.markDown(err)
	}
}

// Delete removes a key from both tiers.
func (c *Cache) Delete(ctx context.Context, key string) {
	c.lru.delete(key)
	if !c.up.Load() {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if err := c.rdb.Del(rctx, key).Err(); err != nil {
		c.markDown(err)
	}
}

// Do returns the cached value for key, otherwise runs fn exactly once across
// concurrent callers (single-flight) and stores its result.
func (c *Cache) Do(ctx context.Context, key string, ttl time.Duration, fn func(context.Context) ([]byte, error)) (value []byte, cached bool, err error) {
	return c.DoWith(ctx, key, ttl, func(ctx context.Context) ([]byte, bool, error) {
		v, err := fn(ctx)
		return v, err == nil, err
	})
}

type doResult struct {
	value     []byte
	fromCache bool
}

// DoWith is Do with control over whether the produced value is stored (partial
// results must not be cached). The bool result reports whether the value came
// from the cache.
func (c *Cache) DoWith(ctx context.Context, key string, ttl time.Duration, fn func(context.Context) ([]byte, bool, error)) (value []byte, fromCache bool, err error) {
	if b, ok := c.Get(ctx, key); ok {
		return b, true, nil
	}
	ch := c.sf.DoChan(key, func() (any, error) {
		// Another process may have filled Redis between our Get and now.
		if b, ok := c.Get(ctx, key); ok {
			return doResult{value: b, fromCache: true}, nil
		}
		b, cacheable, ferr := fn(ctx)
		if ferr != nil {
			return nil, ferr
		}
		if cacheable {
			c.Set(ctx, key, b, ttl)
		}
		return doResult{value: b, fromCache: false}, nil
	})
	select {
	case <-ctx.Done():
		return nil, false, ctx.Err()
	case res := <-ch:
		if res.Err != nil {
			return nil, false, res.Err
		}
		r, ok := res.Val.(doResult)
		if !ok {
			return nil, false, errors.New("cache: unexpected singleflight value type")
		}
		return r.value, r.fromCache, nil
	}
}

// Health reports Redis reachability for /health.
func (c *Cache) Health(ctx context.Context) (string, error) {
	rctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if err := c.rdb.Ping(rctx).Err(); err != nil {
		c.up.Store(false)
		return "down", err
	}
	c.up.Store(true)
	return "ok", nil
}

// Stats exposes the local tier counters.
func (c *Cache) Stats() (hits, misses uint64, entries int) {
	h, m := c.lru.stats()
	return h, m, c.lru.len()
}

func (c *Cache) markDown(err error) {
	// A cancelled request context says nothing about Redis health.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	if c.up.Swap(false) {
		c.log.Warn("redis failure, degrading to local cache", "error", err)
	}
}

// Close releases Redis connections.
func (c *Cache) Close() error { return c.rdb.Close() }
