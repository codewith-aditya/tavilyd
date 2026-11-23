// Package usage stores per-key credit counters and feedback records in Redis.
package usage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// Store is a thin Redis wrapper.
type Store struct {
	rdb *redis.Client
}

// New builds the store (lazy connection).
func New(redisURL string) (*Store, error) {
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("redis url: %w", err)
	}
	opts.DialTimeout = 500 * time.Millisecond
	opts.ReadTimeout = 500 * time.Millisecond
	opts.WriteTimeout = 500 * time.Millisecond
	return &Store{rdb: redis.NewClient(opts)}, nil
}

// usageKey hashes the API key so Redis never stores plaintext credentials.
func usageKey(apiKey string) string {
	sum := sha256.Sum256([]byte(apiKey))
	return "usage:v2:" + hex.EncodeToString(sum[:])
}

// Delta is one atomic batch of per-key counters.
type Delta struct {
	Requests  int64
	Searches  int64
	Extracts  int64
	Maps      int64
	Crawls    int64
	Research  int64
	Credits   int64
	TokensIn  int64
	TokensOut int64
}

// Snapshot is the all-time per-key usage view.
type Snapshot struct {
	Requests  int64 `json:"requests"`
	Searches  int64 `json:"searches"`
	Extracts  int64 `json:"extracts"`
	Maps      int64 `json:"maps"`
	Crawls    int64 `json:"crawls"`
	Research  int64 `json:"research"`
	Credits   int64 `json:"credits"`
	TokensIn  int64 `json:"tokens_in"`
	TokensOut int64 `json:"tokens_out"`
	LastUsed  int64 `json:"last_used,omitempty"`
}

// QuotaAllows reports whether a request costing cost credits fits under limit.
// limit <= 0 disables the quota.
func QuotaAllows(used, limit, cost int64) bool {
	if limit <= 0 {
		return true
	}
	if cost < 0 {
		cost = 0
	}
	return used+cost <= limit
}

// Record atomically increments any non-zero counters for a key.
func (s *Store) Record(ctx context.Context, apiKey string, d Delta) error {
	if s == nil {
		return nil
	}
	c, cancel := context.WithTimeout(ctx, 900*time.Millisecond)
	defer cancel()
	key := usageKey(apiKey)
	pipe := s.rdb.Pipeline()
	fields := map[string]int64{
		"requests": d.Requests, "searches": d.Searches, "extracts": d.Extracts,
		"maps": d.Maps, "crawls": d.Crawls, "research": d.Research,
		"credits": d.Credits, "tokens_in": d.TokensIn, "tokens_out": d.TokensOut,
	}
	for field, n := range fields {
		if n != 0 {
			pipe.HIncrBy(c, key, field, n)
		}
	}
	if d.Requests != 0 {
		pipe.HSet(c, key, "last_used", time.Now().Unix())
	}
	_, err := pipe.Exec(c)
	return err
}

// Snapshot reads all counters for a key.
func (s *Store) Snapshot(ctx context.Context, apiKey string) (Snapshot, error) {
	var out Snapshot
	if s == nil {
		return out, nil
	}
	c, cancel := context.WithTimeout(ctx, 700*time.Millisecond)
	defer cancel()
	m, err := s.rdb.HGetAll(c, usageKey(apiKey)).Result()
	if err != nil {
		return out, err
	}
	parse := func(field string) int64 {
		n, _ := strconv.ParseInt(m[field], 10, 64)
		return n
	}
	out.Requests, out.Searches, out.Extracts = parse("requests"), parse("searches"), parse("extracts")
	out.Maps, out.Crawls, out.Research = parse("maps"), parse("crawls"), parse("research")
	out.Credits, out.TokensIn, out.TokensOut = parse("credits"), parse("tokens_in"), parse("tokens_out")
	out.LastUsed = parse("last_used")
	return out, nil
}

// Add increments the credit counter for an API key.
func (s *Store) Add(ctx context.Context, apiKey string, credits int) error {
	if credits <= 0 {
		return nil
	}
	return s.Record(ctx, apiKey, Delta{Credits: int64(credits)})
}

// Get returns the credit counter for an API key.
func (s *Store) Get(ctx context.Context, apiKey string) (int64, error) {
	snap, err := s.Snapshot(ctx, apiKey)
	return snap.Credits, err
}

// SaveFeedback stores one feedback payload with a TTL.
func (s *Store) SaveFeedback(ctx context.Context, id string, payload []byte, ttl time.Duration) error {
	if s == nil {
		return fmt.Errorf("usage store unavailable")
	}
	c, cancel := context.WithTimeout(ctx, 700*time.Millisecond)
	defer cancel()
	return s.rdb.Set(c, "feedback:"+id, payload, ttl).Err()
}

// Ping reports availability.
func (s *Store) Ping(ctx context.Context) error {
	if s == nil {
		return fmt.Errorf("usage store unavailable")
	}
	c, cancel := context.WithTimeout(ctx, 700*time.Millisecond)
	defer cancel()
	return s.rdb.Ping(c).Err()
}

// Close releases connections.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	return s.rdb.Close()
}
