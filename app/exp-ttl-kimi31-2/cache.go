// Package ttlcache provides a concurrent, size-bounded cache with optional TTLs.
package ttlcache

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// Options configures a Cache. Zero TTL and MaxEntries values mean no expiry
// and no size limit, respectively.
type Options struct {
	TTL        time.Duration
	MaxEntries int
	Now        func() time.Time
}

type entry[V any] struct {
	key       any
	value     V
	expiresAt time.Time
	element   *list.Element
}

type call[V any] struct {
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	waiters int
	value   V
	err     error
}

// Cache is a concurrent-safe cache. Its zero value is not usable; use New.
type Cache[K comparable, V any] struct {
	mu      sync.Mutex
	ttl     time.Duration
	max     int
	now     func() time.Time
	entries map[K]*entry[V]
	lru     *list.List // front is most recently used; values are K
	loads   map[K]*call[V]
}

// New creates a cache configured by opts.
func New[K comparable, V any](opts Options) *Cache[K, V] {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Cache[K, V]{
		ttl:     opts.TTL,
		max:     opts.MaxEntries,
		now:     now,
		entries: make(map[K]*entry[V]),
		lru:     list.New(),
		loads:   make(map[K]*call[V]),
	}
}

func (c *Cache[K, V]) expired(e *entry[V], now time.Time) bool {
	return c.ttl > 0 && !now.Before(e.expiresAt)
}

// removeExpiredLocked removes every expired entry. c.mu must be held.
func (c *Cache[K, V]) removeExpiredLocked(now time.Time) {
	if c.ttl == 0 {
		return
	}
	for key, e := range c.entries {
		if c.expired(e, now) {
			delete(c.entries, key)
			c.lru.Remove(e.element)
		}
	}
}

func (c *Cache[K, V]) storeLocked(key K, value V, now time.Time) {
	e, ok := c.entries[key]
	if !ok {
		e = &entry[V]{key: key, element: c.lru.PushFront(key)}
		c.entries[key] = e
	} else {
		c.lru.MoveToFront(e.element)
	}
	e.value = value
	if c.ttl > 0 {
		e.expiresAt = now.Add(c.ttl)
	} else {
		e.expiresAt = time.Time{}
	}
}

// Get returns a live value and marks it as most recently used.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		var zero V
		return zero, false
	}
	if c.expired(e, c.now()) {
		delete(c.entries, key)
		c.lru.Remove(e.element)
		var zero V
		return zero, false
	}
	c.lru.MoveToFront(e.element)
	return e.value, true
}

// Set stores value, replacing any existing value and restarting its TTL.
func (c *Cache[K, V]) Set(key K, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.removeExpiredLocked(now)
	c.storeLocked(key, value, now)
	for c.max > 0 && len(c.entries) > c.max {
		oldestKey := c.lru.Back().Value.(K)
		oldest := c.entries[oldestKey]
		delete(c.entries, oldestKey)
		c.lru.Remove(oldest.element)
	}
}

// Delete removes key without affecting an in-flight load for it.
func (c *Cache[K, V]) Delete(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[key]; ok {
		delete(c.entries, key)
		c.lru.Remove(e.element)
	}
}

// Len returns the number of live entries.
func (c *Cache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.removeExpiredLocked(c.now())
	return len(c.entries)
}

// GetOrLoad returns a live cached value, or shares one in-flight load per key.
// The load has an independent context which is cancelled when its last waiter
// gives up. Its result is cached even if all waiters have given up.
func (c *Cache[K, V]) GetOrLoad(ctx context.Context, key K, load func(context.Context) (V, error)) (V, error) {
	c.mu.Lock()
	if e, ok := c.entries[key]; ok {
		if !c.expired(e, c.now()) {
			c.lru.MoveToFront(e.element)
			value := e.value
			c.mu.Unlock()
			return value, nil
		}
		delete(c.entries, key)
		c.lru.Remove(e.element)
	}
	if active, ok := c.loads[key]; ok {
		active.waiters++
		c.mu.Unlock()
		return c.wait(ctx, key, active)
	}
	loadCtx, cancel := context.WithCancel(context.Background())
	active := &call[V]{ctx: loadCtx, cancel: cancel, done: make(chan struct{}), waiters: 1}
	c.loads[key] = active
	c.mu.Unlock()
	go c.runLoad(key, active, load)
	return c.wait(ctx, key, active)
}

func (c *Cache[K, V]) wait(ctx context.Context, key K, active *call[V]) (V, error) {
	select {
	case <-active.done:
		return active.value, active.err
	case <-ctx.Done():
		c.mu.Lock()
		// The load may have completed concurrently, but this caller selected
		// cancellation and must no longer keep its context alive.
		active.waiters--
		if active.waiters == 0 {
			active.cancel()
		}
		c.mu.Unlock()
		var zero V
		return zero, ctx.Err()
	}
}

func (c *Cache[K, V]) runLoad(key K, active *call[V], load func(context.Context) (V, error)) {
	value, err := load(active.ctx)
	c.mu.Lock()
	if err == nil {
		now := c.now()
		c.removeExpiredLocked(now)
		c.storeLocked(key, value, now)
		// A completed load follows the same capacity rule as Set.
		for c.max > 0 && len(c.entries) > c.max {
			oldestKey := c.lru.Back().Value.(K)
			oldest := c.entries[oldestKey]
			delete(c.entries, oldestKey)
			c.lru.Remove(oldest.element)
		}
	}
	active.value, active.err = value, err
	delete(c.loads, key)
	close(active.done)
	c.mu.Unlock()
}
