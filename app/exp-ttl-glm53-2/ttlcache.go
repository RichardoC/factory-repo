// Package ttlcache implements a generic, concurrency-safe in-memory cache
// with per-entry TTL expiry, LRU eviction under a bound on live entries, and
// single-flight loading (at most one load per key at a time).
//
// Semantics, in short:
//
//   - An entry stored at time t (by the cache's clock, Options.Now) is live
//     while Now() is before t+TTL. TTL 0 means entries never expire.
//   - Set on an existing key replaces the value and restarts its TTL.
//   - When storing a new entry would leave more than Options.MaxEntries live
//     entries, the least recently used live entry is evicted. Expired
//     entries never cause a live entry to be evicted.
//   - GetOrLoad on a miss calls load once per key for all concurrent
//     callers; the context passed to load is cancelled exactly when every
//     caller waiting on that load has given up; a successful load result is
//     stored (TTL starting at load return) even if every caller gave up.
package ttlcache

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// Options configures a Cache returned by New. The zero value means: entries
// never expire, the cache is unbounded, and time.Now is the clock.
type Options struct {
	// TTL is how long an entry stays live after it was stored. Zero or
	// negative means entries never expire.
	TTL time.Duration
	// MaxEntries bounds the number of live entries. Zero or negative means
	// unbounded.
	MaxEntries int
	// Now is the clock the cache reads. Nil means time.Now.
	Now func() time.Time
}

// Cache is a TTL+LRU cache with single-flight loading. It is safe for
// concurrent use. The zero value is not usable; call New.
type Cache[K comparable, V any] struct {
	mu      sync.Mutex
	ttl     time.Duration
	max     int
	now     func() time.Time
	entries map[K]*entry[V]
	order   *list.List // front = most recently used; Element.Value holds a K
	loads   map[K]*loadState[V]
}

// entry is one cached value. expireAt zero means it never expires.
type entry[V any] struct {
	value    V
	expireAt time.Time
	elem     *list.Element
}

// live reports whether the entry is still live at time now.
func (e *entry[V]) live(now time.Time) bool {
	return e.expireAt.IsZero() || now.Before(e.expireAt)
}

// loadState tracks one in-flight load and the callers waiting on it.
type loadState[V any] struct {
	ctx     context.Context // passed to load; cancelled when waiters drops to zero
	cancel  context.CancelFunc
	done    chan struct{} // closed once, when the load has finished
	waiters int           // callers currently waiting on this load
	value   V             // the load's result
	err     error
}

// New creates a Cache with the given options.
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
		order:   list.New(),
		loads:   make(map[K]*loadState[V]),
	}
}

// Set stores value under key. If the key already exists, the value is
// replaced and the entry's TTL restarts. Storing counts as a use.
func (c *Cache[K, V]) Set(key K, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.storeLocked(key, value)
}

// Get returns the live cached value for key, if any. A hit counts as a use.
// Expired entries are never returned.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.getLocked(key)
}

// Delete removes key from the cache. It does not stop an in-flight load for
// the key; a load that completes afterwards still stores its result.
func (c *Cache[K, V]) Delete(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[key]; ok {
		c.removeLocked(e.elem, key)
	}
}

// Len returns the number of live entries. Expired entries are not counted.
func (c *Cache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	n := 0
	for _, e := range c.entries {
		if e.live(now) {
			n++
		}
	}
	return n
}

// GetOrLoad returns the live cached value for key. On a miss it calls load,
// single-flight: concurrent callers for the same key share one call of load
// and get the same result, while loads for different keys run independently.
//
// A caller whose ctx is done before the load finishes returns ctx.Err()
// promptly; the load keeps running for the remaining callers. The context
// passed to load is cancelled exactly when every caller waiting on that load
// has given up. A successful result is stored (its TTL starting when load
// returned) even if all callers gave up; an error result caches nothing.
func (c *Cache[K, V]) GetOrLoad(ctx context.Context, key K, load func(ctx context.Context) (V, error)) (V, error) {
	var zero V
	c.mu.Lock()
	if v, ok := c.getLocked(key); ok {
		c.mu.Unlock()
		return v, nil
	}
	st := c.loads[key]
	if st == nil {
		// This caller is the first: start the load. Its context is fresh, so
		// no single caller's cancellation can cancel it alone.
		lctx, cancel := context.WithCancel(context.Background())
		st = &loadState[V]{ctx: lctx, cancel: cancel, done: make(chan struct{}), waiters: 1}
		c.loads[key] = st
		go c.runLoad(key, st, load)
	} else {
		st.waiters++
	}
	c.mu.Unlock()

	select {
	case <-ctx.Done():
		c.mu.Lock()
		st.waiters--
		if st.waiters == 0 {
			// Every waiter has given up: cancel the load's context.
			st.cancel()
		}
		c.mu.Unlock()
		return zero, ctx.Err()
	case <-st.done:
		c.mu.Lock()
		st.waiters--
		if st.waiters == 0 {
			// Release the load's context; the load itself has finished.
			st.cancel()
		}
		c.mu.Unlock()
		if st.err != nil {
			return zero, st.err
		}
		return st.value, nil
	}
}

// runLoad runs one load to completion and publishes its result to the cache
// and to the waiters. It is the only goroutine the cache starts, and it ends
// when load returns.
func (c *Cache[K, V]) runLoad(key K, st *loadState[V], load func(ctx context.Context) (V, error)) {
	v, err := load(st.ctx)
	c.mu.Lock()
	st.value, st.err = v, err
	if err == nil {
		// Store even if every caller gave up (the load still produced a
		// value); the TTL starts now, when load returned.
		c.storeLocked(key, v)
	}
	if cur, ok := c.loads[key]; ok && cur == st {
		delete(c.loads, key)
	}
	close(st.done) // wakes the waiters; their reads of st.value/st.err are safe
	c.mu.Unlock()
}

// storeLocked stores value under key and records recency. Callers must hold
// c.mu.
func (c *Cache[K, V]) storeLocked(key K, value V) {
	now := c.now()
	var expireAt time.Time
	if c.ttl > 0 {
		expireAt = now.Add(c.ttl)
	}
	if e, ok := c.entries[key]; ok {
		// Replace: the count of live entries does not grow.
		e.value = value
		e.expireAt = expireAt
		c.order.MoveToFront(e.elem)
		return
	}
	c.evictLRULocked(now)
	e := &entry[V]{value: value, expireAt: expireAt}
	e.elem = c.order.PushFront(key)
	c.entries[key] = e
}

// evictLRULocked makes room for one new entry when the cache is bounded: if
// the number of live entries is already Options.MaxEntries, it evicts the
// least recently used live entry. Expired entries it walks past are dropped,
// but they never count toward the bound. Callers must hold c.mu.
func (c *Cache[K, V]) evictLRULocked(now time.Time) {
	if c.max <= 0 {
		return
	}
	seen := 0
	var lruElem *list.Element
	var lruKey K
	for el := c.order.Back(); el != nil; {
		key := el.Value.(K)
		e := c.entries[key]
		prev := el.Prev()
		if e.live(now) {
			if seen == 0 {
				lruElem, lruKey = el, key
			}
			seen++
			if seen == c.max {
				// Inserting one more would exceed the bound: evict the least
				// recently used live entry.
				c.removeLocked(lruElem, lruKey)
				return
			}
		} else {
			// Expired: drop it; it does not count and does not evict anything.
			c.removeLocked(el, key)
		}
		el = prev
	}
}

// getLocked returns the live value for key and records the use, or reports a
// miss, dropping the entry if it has expired. Callers must hold c.mu.
func (c *Cache[K, V]) getLocked(key K) (V, bool) {
	var zero V
	e, ok := c.entries[key]
	if !ok {
		return zero, false
	}
	if !e.live(c.now()) {
		c.removeLocked(e.elem, key)
		return zero, false
	}
	c.order.MoveToFront(e.elem)
	return e.value, true
}

// removeLocked drops one entry. Callers must hold c.mu.
func (c *Cache[K, V]) removeLocked(el *list.Element, key K) {
	c.order.Remove(el)
	delete(c.entries, key)
}
