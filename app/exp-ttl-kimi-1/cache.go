// Package ttlcache provides a generic in-memory cache with per-entry TTL,
// optional LRU eviction bounded by a maximum number of live entries, and
// singleflight-style coordinated loading with per-waiter context support.
//
// The cache is safe for concurrent use.
package ttlcache

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// Options configures a Cache.
type Options struct {
	// TTL, when > 0, makes an entry expire TTL after it was stored.
	// When 0, entries never expire.
	TTL time.Duration
	// MaxEntries, when > 0, bounds the number of live entries; storing
	// beyond it evicts the least recently used live entry.
	// When 0, the cache is unbounded.
	MaxEntries int
	// Now is the clock used for expiry. nil means time.Now.
	Now func() time.Time
}

type entry[V any] struct {
	value   V
	expires time.Time // zero when TTL == 0
	elem    *list.Element
}

// flight tracks one in-flight load for a key.
type flight[V any] struct {
	done    chan struct{} // closed when the load completes
	value   V
	err     error
	ctx     context.Context    // context handed to load
	cancel  context.CancelFunc // cancels ctx when the last waiter leaves
	waiters int
	doneAt  time.Time // when load returned, by the cache clock
}

// Cache is a concurrent-safe TTL/LRU cache.
type Cache[K comparable, V any] struct {
	ttl        time.Duration
	maxEntries int
	now        func() time.Time

	mu      sync.Mutex
	items   map[K]*entry[V]
	lru     *list.List // front = most recently used; holds keys (K)
	flights map[K]*flight[V]
}

// New returns a Cache configured by opts.
func New[K comparable, V any](opts Options) *Cache[K, V] {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Cache[K, V]{
		ttl:        opts.TTL,
		maxEntries: opts.MaxEntries,
		now:        now,
		items:      make(map[K]*entry[V]),
		lru:        list.New(),
		flights:    make(map[K]*flight[V]),
	}
}

// liveAt reports whether e is live at t.
func (e *entry[V]) liveAt(t time.Time) bool {
	return e.expires.IsZero() || t.Before(e.expires)
}

// getEntry returns the live entry for key, or nil after removing an expired
// one. c.mu must be held.
func (c *Cache[K, V]) getEntry(key K, t time.Time) *entry[V] {
	e, ok := c.items[key]
	if !ok {
		return nil
	}
	if !e.liveAt(t) {
		c.removeElem(e, key)
		return nil
	}
	return e
}

// removeElem unlinks e from the LRU list and the map. c.mu must be held.
func (c *Cache[K, V]) removeElem(e *entry[V], key K) {
	c.lru.Remove(e.elem)
	delete(c.items, key)
}

// evictExpired removes all expired entries. c.mu must be held.
func (c *Cache[K, V]) evictExpired(t time.Time) {
	for key, e := range c.items {
		if !e.liveAt(t) {
			c.removeElem(e, key)
		}
	}
}

// store inserts value for key with TTL starting at t, marks it most
// recently used, and enforces MaxEntries. It replaces any existing live
// entry. c.mu must be held.
func (c *Cache[K, V]) store(key K, value V, t time.Time) {
	if old, ok := c.items[key]; ok {
		c.lru.Remove(old.elem)
	}
	e := &entry[V]{value: value}
	if c.ttl > 0 {
		e.expires = t.Add(c.ttl)
	}
	e.elem = c.lru.PushFront(key)
	c.items[key] = e
	if c.maxEntries > 0 {
		// Expired entries never cause a live entry to be evicted: purge
		// them first so the limit applies to live entries only.
		c.evictExpired(t)
		for len(c.items) > c.maxEntries {
			back := c.lru.Back()
			if back == nil {
				break
			}
			k := back.Value.(K)
			le := c.items[k]
			if le == nil {
				c.lru.Remove(back)
				continue
			}
			c.lru.Remove(back)
			delete(c.items, k)
		}
	}
}

// Get returns the live value for key and marks it recently used.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var zero V
	e := c.getEntry(key, c.now())
	if e == nil {
		return zero, false
	}
	c.lru.MoveToFront(e.elem)
	return e.value, true
}

// Set stores value for key with the TTL starting now, replacing any
// existing entry. It marks the key most recently used.
func (c *Cache[K, V]) Set(key K, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.store(key, value, c.now())
}

// Delete removes key from the cache. An in-flight load for key is not
// stopped and its result is still stored when it completes.
func (c *Cache[K, V]) Delete(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		c.removeElem(e, key)
	}
}

// Len returns the number of live entries.
func (c *Cache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.evictExpired(c.now())
	return len(c.items)
}

// GetOrLoad returns the cached value on a live hit. On a miss it joins (or
// starts) the single in-flight load for key: concurrent callers for one key
// share one call of load, while different keys load independently.
//
// The context handed to load is cancelled when the last waiting caller has
// given up; each caller independently returns ctx.Err() when its own ctx is
// done. If load fails, nothing is cached and a later GetOrLoad loads again.
// A successful result is stored even if every caller gave up, with its TTL
// starting when load returned.
func (c *Cache[K, V]) GetOrLoad(ctx context.Context, key K, load func(ctx context.Context) (V, error)) (V, error) {
	c.mu.Lock()
	if e := c.getEntry(key, c.now()); e != nil {
		c.lru.MoveToFront(e.elem)
		v := e.value
		c.mu.Unlock()
		return v, nil
	}
	f, ok := c.flights[key]
	if !ok {
		fctx, cancel := context.WithCancel(context.Background())
		f = &flight[V]{
			done:    make(chan struct{}),
			ctx:     fctx,
			cancel:  cancel,
			waiters: 0,
		}
		c.flights[key] = f
		go c.runLoad(key, f, load)
	}
	f.waiters++
	c.mu.Unlock()

	var zero V
	select {
	case <-f.done:
		return f.value, f.err
	case <-ctx.Done():
		// This caller gives up; if it was the last waiter, cancel the
		// context load sees. The load itself is never blocked on this.
		c.mu.Lock()
		f.waiters--
		if f.waiters == 0 {
			f.cancel()
		}
		c.mu.Unlock()
		return zero, ctx.Err()
	}
}

// runLoad runs one shared load and publishes its result.
func (c *Cache[K, V]) runLoad(key K, f *flight[V], load func(ctx context.Context) (V, error)) {
	v, err := load(f.ctx)
	doneAt := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.flights, key)
	f.doneAt = doneAt
	if err != nil {
		f.err = err // per-flight copy of the error for every waiter
	} else {
		f.value = v
		c.store(key, v, doneAt)
	}
	f.cancel() // release the flight context; load already returned
	close(f.done)
}
