// Package ttlcache provides a concurrency-safe, size-bounded cache with TTLs.
package ttlcache

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// Options configures a Cache. Zero TTL and MaxEntries values mean unlimited.
type Options struct {
	TTL        time.Duration
	MaxEntries int
	Now        func() time.Time
}

// Cache is a concurrency-safe cache with optional expiry and LRU eviction.
type Cache[K comparable, V any] struct {
	mu      sync.Mutex
	ttl     time.Duration
	max     int
	now     func() time.Time
	entries map[K]*list.Element
	lru     list.List // front is most recently used
	loads   map[K]*call[V]
}

type entry[K comparable, V any] struct {
	key     K
	value   V
	expires time.Time
}

type call[V any] struct {
	done    chan struct{}
	cancel  context.CancelFunc
	waiters int
	value   V
	err     error
}

// New constructs an empty cache.
func New[K comparable, V any](opts Options) *Cache[K, V] {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Cache[K, V]{
		ttl:     opts.TTL,
		max:     opts.MaxEntries,
		now:     now,
		entries: make(map[K]*list.Element),
		loads:   make(map[K]*call[V]),
	}
}

// Get returns the live value for key and marks it as recently used.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.liveEntry(key); ok {
		c.lru.MoveToFront(c.entries[key])
		return e.value, true
	}
	var zero V
	return zero, false
}

// Set stores value, replacing any existing value and restarting its TTL.
func (c *Cache[K, V]) Set(key K, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.store(key, value)
}

func (c *Cache[K, V]) store(key K, value V) {
	c.removeExpired()
	expires := time.Time{}
	if c.ttl > 0 {
		expires = c.now().Add(c.ttl)
	}
	if elem, ok := c.entries[key]; ok {
		e := elem.Value.(*entry[K, V])
		e.value = value
		e.expires = expires
		c.lru.MoveToFront(elem)
	} else {
		elem := c.lru.PushFront(&entry[K, V]{key: key, value: value, expires: expires})
		c.entries[key] = elem
	}
	for c.max > 0 && len(c.entries) > c.max {
		oldest := c.lru.Back()
		delete(c.entries, oldest.Value.(*entry[K, V]).key)
		c.lru.Remove(oldest)
	}
}

// Delete removes key without affecting an in-flight load for it.
func (c *Cache[K, V]) Delete(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.remove(key)
}

// Len returns the number of live entries.
func (c *Cache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.removeExpired()
	return len(c.entries)
}

func (c *Cache[K, V]) liveEntry(key K) (*entry[K, V], bool) {
	elem, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	e := elem.Value.(*entry[K, V])
	if !e.expires.IsZero() && !c.now().Before(e.expires) {
		c.remove(key)
		return nil, false
	}
	return e, true
}

func (c *Cache[K, V]) removeExpired() {
	now := c.now()
	for elem := c.lru.Back(); elem != nil; {
		previous := elem.Prev()
		e := elem.Value.(*entry[K, V])
		if !e.expires.IsZero() && !now.Before(e.expires) {
			c.remove(e.key)
		}
		elem = previous
	}
}

func (c *Cache[K, V]) remove(key K) {
	if elem, ok := c.entries[key]; ok {
		delete(c.entries, key)
		c.lru.Remove(elem)
	}
}

// GetOrLoad returns a live cached value or shares a load for key with any
// concurrent callers. A cancelled caller stops waiting without stopping a load
// needed by another caller.
func (c *Cache[K, V]) GetOrLoad(ctx context.Context, key K, load func(context.Context) (V, error)) (V, error) {
	c.mu.Lock()
	if e, ok := c.liveEntry(key); ok {
		c.lru.MoveToFront(c.entries[key])
		value := e.value
		c.mu.Unlock()
		return value, nil
	}
	shared, ok := c.loads[key]
	if !ok {
		loadCtx, cancel := context.WithCancel(context.Background())
		shared = &call[V]{done: make(chan struct{}), cancel: cancel, waiters: 1}
		c.loads[key] = shared
		go c.runLoad(loadCtx, key, load, shared)
	} else {
		shared.waiters++
	}
	c.mu.Unlock()

	select {
	case <-shared.done:
		return shared.value, shared.err
	case <-ctx.Done():
		c.mu.Lock()
		shared.waiters--
		if shared.waiters == 0 {
			shared.cancel()
		}
		c.mu.Unlock()
		var zero V
		return zero, ctx.Err()
	}
}

func (c *Cache[K, V]) runLoad(ctx context.Context, key K, load func(context.Context) (V, error), shared *call[V]) {
	value, err := load(ctx)
	c.mu.Lock()
	if err == nil {
		c.store(key, value)
	}
	delete(c.loads, key)
	shared.value = value
	shared.err = err
	close(shared.done)
	shared.cancel()
	c.mu.Unlock()
}
