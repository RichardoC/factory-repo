// Package ttlcache provides a small, concurrency-safe TTL/LRU cache.
package ttlcache

import (
	"context"
	"sync"
	"time"
)

// Options configures a Cache.
type Options struct {
	TTL        time.Duration
	MaxEntries int
	Now        func() time.Time
}

type entry[V any] struct {
	value   V
	expires time.Time
	used    uint64
}

type call[V any] struct {
	done    chan struct{}
	cancel  context.CancelFunc
	waiters int
	value   V
	err     error
}

// Cache is a concurrency-safe cache with optional expiration and LRU eviction.
// The zero value is not usable; use New.
type Cache[K comparable, V any] struct {
	mu      sync.Mutex
	ttl     time.Duration
	max     int
	now     func() time.Time
	entries map[K]entry[V]
	loads   map[K]*call[V]
	used    uint64
}

// New creates an empty cache.
func New[K comparable, V any](opts Options) *Cache[K, V] {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Cache[K, V]{
		ttl:     opts.TTL,
		max:     opts.MaxEntries,
		now:     now,
		entries: make(map[K]entry[V]),
		loads:   make(map[K]*call[V]),
	}
}

func (c *Cache[K, V]) live(e entry[V], now time.Time) bool {
	return e.expires.IsZero() || now.Before(e.expires)
}

// discardExpired must be called with c.mu held.
func (c *Cache[K, V]) discardExpired(now time.Time) {
	for key, e := range c.entries {
		if !c.live(e, now) {
			delete(c.entries, key)
		}
	}
}

// Get returns a live value and marks it as recently used.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var zero V
	e, ok := c.entries[key]
	if !ok || !c.live(e, c.now()) {
		if ok {
			delete(c.entries, key)
		}
		return zero, false
	}
	c.used++
	e.used = c.used
	c.entries[key] = e
	return e.value, true
}

// Set stores a value, restarting its expiration and marking it recently used.
func (c *Cache[K, V]) Set(key K, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setLocked(key, value, c.now())
}

// setLocked stores at the supplied time; c.mu must be held.
func (c *Cache[K, V]) setLocked(key K, value V, now time.Time) {
	c.discardExpired(now)
	c.used++
	e := entry[V]{value: value, used: c.used}
	if c.ttl > 0 {
		e.expires = now.Add(c.ttl)
	}
	c.entries[key] = e
	if c.max > 0 {
		for len(c.entries) > c.max {
			var oldest K
			var oldestUse uint64
			first := true
			for candidate, item := range c.entries {
				if first || item.used < oldestUse {
					oldest, oldestUse, first = candidate, item.used, false
				}
			}
			delete(c.entries, oldest)
		}
	}
}

// Delete removes key without affecting an in-flight load for it.
func (c *Cache[K, V]) Delete(key K) {
	c.mu.Lock()
	delete(c.entries, key)
	c.mu.Unlock()
}

// Len returns the number of live entries.
func (c *Cache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.discardExpired(c.now())
	return len(c.entries)
}

// GetOrLoad returns a cached value or shares one in-flight load for key.
func (c *Cache[K, V]) GetOrLoad(ctx context.Context, key K, load func(context.Context) (V, error)) (V, error) {
	if value, ok := c.Get(key); ok {
		return value, nil
	}

	c.mu.Lock()
	// Another caller may have populated the key between Get and this lock.
	if e, ok := c.entries[key]; ok && c.live(e, c.now()) {
		c.used++
		e.used = c.used
		c.entries[key] = e
		c.mu.Unlock()
		return e.value, nil
	}
	if current, ok := c.loads[key]; ok {
		current.waiters++
		c.mu.Unlock()
		return c.wait(ctx, key, current)
	}
	loadCtx, cancel := context.WithCancel(context.Background())
	current := &call[V]{done: make(chan struct{}), cancel: cancel, waiters: 1}
	c.loads[key] = current
	c.mu.Unlock()

	go func() {
		value, err := load(loadCtx)
		returned := c.now()
		c.mu.Lock()
		current.value, current.err = value, err
		delete(c.loads, key)
		if err == nil {
			c.setLocked(key, value, returned)
		}
		close(current.done)
		c.mu.Unlock()
	}()
	return c.wait(ctx, key, current)
}

func (c *Cache[K, V]) wait(ctx context.Context, key K, current *call[V]) (V, error) {
	select {
	case <-current.done:
		return current.value, current.err
	case <-ctx.Done():
		c.mu.Lock()
		current.waiters--
		if current.waiters == 0 {
			current.cancel()
		}
		c.mu.Unlock()
		var zero V
		return zero, ctx.Err()
	}
}
