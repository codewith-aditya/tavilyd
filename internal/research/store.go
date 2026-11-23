package research

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Store persists jobs in Redis with a TTL.
type Store struct {
	rdb *redis.Client
	ttl time.Duration
}

// NewStore builds a Redis-backed store. The connection is lazy: creation does
// not fail when Redis is down; Ping decides availability per request.
func NewStore(redisURL string, ttl time.Duration) (*Store, error) {
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("redis url: %w", err)
	}
	opts.MaxRetries = 1
	opts.DialTimeout = 500 * time.Millisecond
	opts.ReadTimeout = 500 * time.Millisecond
	opts.WriteTimeout = 500 * time.Millisecond
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &Store{rdb: redis.NewClient(opts), ttl: ttl}, nil
}

func jobKey(id string) string { return "research:job:" + id }

// Save writes the job JSON.
func (s *Store) Save(ctx context.Context, job *Job) error {
	if s == nil {
		return ErrUnavailable
	}
	b, err := json.Marshal(job)
	if err != nil {
		return err
	}
	c, cancel := context.WithTimeout(ctx, 700*time.Millisecond)
	defer cancel()
	return s.rdb.Set(c, jobKey(job.ID), b, s.ttl).Err()
}

// Get loads a job.
func (s *Store) Get(ctx context.Context, id string) (*Job, error) {
	if s == nil {
		return nil, ErrUnavailable
	}
	c, cancel := context.WithTimeout(ctx, 700*time.Millisecond)
	defer cancel()
	b, err := s.rdb.Get(c, jobKey(id)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, ErrUnavailable
	}
	var job Job
	if err := json.Unmarshal(b, &job); err != nil {
		return nil, err
	}
	return &job, nil
}

// Ping reports store availability.
func (s *Store) Ping(ctx context.Context) error {
	if s == nil {
		return ErrUnavailable
	}
	c, cancel := context.WithTimeout(ctx, 700*time.Millisecond)
	defer cancel()
	return s.rdb.Ping(c).Err()
}

// Close releases Redis connections.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	return s.rdb.Close()
}
