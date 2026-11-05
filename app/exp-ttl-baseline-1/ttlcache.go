// Package ttlcache implements an in-memory cache with per-entry TTL,
// LRU eviction under a live-entry cap, and single-flight loading.
//
// All exported methods are safe for concurrent use.
package ttlcache

import (
	"context"
	"sync"
	"time"
)

// Options configures a Cache.
type Options struct {
	// TTL is how long an entry stays live after it was stored.
	// > 0: an entry expires TTL after it was stored.
	// 0: entries never expire.
	TTL time.Duration

	// MaxEntries caps the number of live entries.
	// > 0: at most this many live entries are kept; the least recently
	// used live entry is evicted when the cap would be exceeded.
	// 0: unbounded.
	MaxEntries int

	// Now is the clock used for expiry. nil means time.Now.
	Now func() time.Time
}

type entry[K comparable, V any] struct {
	value    V
	expireAt time.Time // zero means the entry never expires
	used     int64     // logical clock stamp for LRU ordering
}

// loadCall is the shared state of one in-flight load for a single key.
type loadCall[K comparable, V any] struct {
	done chan struct{} // closed once, when the load has finished
	// cancel cancels the context handed to load. It fires exactly when
	// every caller waiting on this load has given up.
	cancel context.CancelCauseFunc
	// waiters is the number of callers still waiting for this load.
	waiters int
	value   V
	err     error
}

// Cache is a TTL + LRU cache with single-flight loading. Use New to create one.
type Cache[K comparable, V any] struct {
	mu       sync.Mutex
	now      func() time.Time
	ttl      time.Duration
	max      int
	entries  map[K]*entry[K, V]
	clock    int64 // logical clock for LRU stamps
	inflight map[K]*loadCall[K, V]
}

// New returns an empty Cache configured by opts.
func New[K comparable, V any](opts Options) *Cache[K, V] {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Cache[K, V]{
		now:      now,
		ttl:      opts.TTL,
		max:      opts.MaxEntries,
		entries:  make(map[K]*entry[K, V]),
		inflight: make(map[K]*loadCall[K, V]),
	}
}

// Get returns the live entry for key, if any. A hit marks the entry as
// most recently used. An expired entry is treated as absent.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.getLocked(key)
}

// Set stores value under key, restarting its TTL. When the store would
// leave more than MaxEntries live entries, expired entries are dropped
// first and then the least recently used live entry is evicted.
func (c *Cache[K, V]) Set(key K, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setLocked(key, value)
}

// Delete removes key from the cache. It does not stop an in-flight load
// for key: when that load finishes successfully its result is still stored.
func (c *Cache[K, V]) Delete(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

// Len returns the number of live entries. Expired entries are not counted
// (and are dropped opportunistically).
func (c *Cache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for k, e := range c.entries {
		if expired(e, now) {
			delete(c.entries, k)
		}
	}
	return len(c.entries)
}

// GetOrLoad returns the live cached value for key, or, on a miss, loads it.
//
// Concurrent GetOrLoad calls for the same key share a single load call:
// at most one load per key runs at a time, and different keys load
// independently. If load returns an error, nothing is cached and every
// caller waiting on that load receives that same error. If a caller's ctx
// is done before the load finishes, that caller returns ctx.Err()
// promptly; the load keeps running for the remaining waiters. The context
// passed to load is cancelled exactly when every waiting caller has given
// up. If load returns successfully, the value is stored even if every
// caller has by then given up, with its TTL starting when load returned.
func (c *Cache[K, V]) GetOrLoad(ctx context.Context, key K, load func(ctx context.Context) (V, error)) (V, error) {
	c.mu.Lock()
	if v, ok := c.getLocked(key); ok {
		c.mu.Unlock()
		return v, nil
	}
	if err := ctx.Err(); err != nil {
		// The caller is already gone; do not start (or join) a load on
		// its behalf.
		c.mu.Unlock()
		var zero V
		return zero, err
	}
	if call, ok := c.inflight[key]; ok {
		call.waiters++
		c.mu.Unlock()
		return c.waitOn(ctx, call)
	}
	// The load's context is deliberately not derived from any single
	// caller's cancellation: it is cancelled only when every waiter has
	// given up. Values from the first caller's ctx are preserved.
	loadCtx, cancel := context.WithCancelCause(context.WithoutCancel(ctx))
	call := &loadCall[K, V]{done: make(chan struct{}), cancel: cancel, waiters: 1}
	c.inflight[key] = call
	c.mu.Unlock()

	go func() {
		v, err := load(loadCtx)
		c.mu.Lock()
		delete(c.inflight, key)
		if err == nil {
			// Stored even if every caller has given up (rule 9),
			// with the TTL starting now (when load returned).
			c.setLocked(key, v)
		}
		call.value, call.err = v, err
		close(call.done)
		c.mu.Unlock()
	}()
	return c.waitOn(ctx, call)
}

// waitOn blocks until the load finishes or ctx is done. It must be called
// with c.mu unlocked and having already registered as a waiter on call.
func (c *Cache[K, V]) waitOn(ctx context.Context, call *loadCall[K, V]) (V, error) {
	select {
	case <-call.done:
		return call.value, call.err
	default:
	}
	select {
	case <-call.done:
		return call.value, call.err
	case <-ctx.Done():
		c.mu.Lock()
		call.waiters--
		if call.waiters == 0 {
			// Every waiting caller has given up: cancel the load's
			// context so a well-behaved load can stop early.
			call.cancel(context.Canceled)
		}
		c.mu.Unlock()
		var zero V
		return zero, ctx.Err()
	}
}

// getLocked returns the live entry for key, if any, marking it used.
func (c *Cache[K, V]) getLocked(key K) (V, bool) {
	e, ok := c.entries[key]
	if !ok {
		var zero V
		return zero, false
	}
	if expired(e, c.now()) {
		delete(c.entries, key)
		var zero V
		return zero, false
	}
	c.clock++
	e.used = c.clock
	return e.value, true
}

// setLocked stores value under key and enforces the MaxEntries cap over
// live entries.
func (c *Cache[K, V]) setLocked(key K, value V) {
	var expireAt time.Time
	if c.ttl > 0 {
		expireAt = c.now().Add(c.ttl)
	}
	c.clock++
	c.entries[key] = &entry[K, V]{value: value, expireAt: expireAt, used: c.clock}
	if c.max <= 0 {
		return
	}
	now := c.now()
	for len(c.entries) > c.max {
		// Expired entries are not live: drop them first so they never
		// push a live entry out.
		for k, e := range c.entries {
			if expired(e, now) {
				delete(c.entries, k)
			}
		}
		if len(c.entries) <= c.max {
			return
		}
		// Still over the cap: evict the least recently used entry.
		var lruKey K
		var lru *entry[K, V]
		for k, e := range c.entries {
			if lru == nil || e.used < lru.used {
				lruKey, lru = k, e
			}
		}
		delete(c.entries, lruKey)
	}
}

func expired[K comparable, V any](e *entry[K, V], now time.Time) bool {
	return !e.expireAt.IsZero() && !now.Before(e.expireAt)
}
