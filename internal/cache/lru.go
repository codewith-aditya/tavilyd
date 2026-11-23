package cache

import (
	"container/list"
	"sync"
	"time"
)

// lru is a size-bounded in-process cache with per-entry TTL.
type lru struct {
	mu      sync.Mutex
	max     int
	entries map[string]*list.Element
	order   *list.List // front = most recently used
	now     func() time.Time
	hits    uint64
	misses  uint64
}

type lruEntry struct {
	key     string
	value   []byte
	expires time.Time
}

func newLRU(max int) *lru {
	if max < 0 {
		max = 0
	}
	return &lru{max: max, entries: make(map[string]*list.Element, max), order: list.New(), now: time.Now}
}

func (c *lru) get(key string) ([]byte, bool) {
	if c == nil || c.max == 0 {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[key]
	if !ok {
		c.misses++
		return nil, false
	}
	e := el.Value.(*lruEntry)
	if c.now().After(e.expires) {
		c.order.Remove(el)
		delete(c.entries, key)
		c.misses++
		return nil, false
	}
	c.order.MoveToFront(el)
	c.hits++
	// Copy so callers cannot mutate cached state.
	out := make([]byte, len(e.value))
	copy(out, e.value)
	return out, true
}

func (c *lru) set(key string, value []byte, ttl time.Duration) {
	if c == nil || c.max == 0 || ttl <= 0 {
		return
	}
	stored := make([]byte, len(value))
	copy(stored, value)

	c.mu.Lock()
	defer c.mu.Unlock()
	exp := c.now().Add(ttl)
	if el, ok := c.entries[key]; ok {
		e := el.Value.(*lruEntry)
		e.value, e.expires = stored, exp
		c.order.MoveToFront(el)
		return
	}
	el := c.order.PushFront(&lruEntry{key: key, value: stored, expires: exp})
	c.entries[key] = el
	for c.order.Len() > c.max {
		last := c.order.Back()
		if last == nil {
			break
		}
		c.order.Remove(last)
		delete(c.entries, last.Value.(*lruEntry).key)
	}
}

func (c *lru) delete(key string) {
	if c == nil || c.max == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[key]; ok {
		c.order.Remove(el)
		delete(c.entries, key)
	}
}

func (c *lru) len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

func (c *lru) stats() (hits, misses uint64) {
	if c == nil {
		return 0, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.misses
}
