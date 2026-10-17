// Package ttlcache provides a concurrency-safe in-memory cache with
// per-entry TTL expiry, LRU eviction under an optional entry cap, and
// single-flight loading via GetOrLoad.
package ttlcache

import (
	"container/list"
	"context"
	"errors"
	"sync"
	"time"
)

// Options configures a Cache.
//
//	TTL        > 0: an entry expires TTL after it was stored; 0: never expires.
//	MaxEntries > 0: at most this many live entries are kept; 0: unbounded.
//	Now        the clock used for expiry; nil means time.Now.
type Options struct {
	TTL        time.Duration
	MaxEntries int
	Now        func() time.Time
}

// Cache is a TTL + LRU cache safe for concurrent use. The zero value is
// not usable; construct caches with New.
type Cache[K comparable, V any] struct {
	mu      sync.Mutex
	ttl     time.Duration
	max     int
	now     func() time.Time
	entries map[K]*entry[K, V]
	lru     *list.List // front = most recently used; back = least recently used
	loads   map[K]*loadState[V]
}

// entry is a cached value. expires is the zero time when the entry never
// expires.
type entry[K comparable, V any] struct {
	key     K
	value   V
	expires time.Time
	elem    *list.Element
}

// expired reports whether the entry is no longer live at now: an entry
// stored at t is live only while now is before t+TTL.
func (e *entry[K, V]) expired(now time.Time) bool {
	return !e.expires.IsZero() && !now.Before(e.expires)
}

// loadState tracks one in-flight load and the callers waiting on it.
// All fields are guarded by the cache mutex; done additionally publishes
// value and err once closed.
type loadState[V any] struct {
	done    chan struct{} // closed once the load has finished and its result is recorded
	cancel  context.CancelFunc
	waiters int  // callers currently waiting on this load
	stopped bool // cancel has been called for this load

	value V
	err   error
}

// errNoLoad is returned by GetOrLoad on a cache miss with a nil load
// function. Such a call stores nothing.
var errNoLoad = errors.New("ttlcache: GetOrLoad called with nil load on a miss")

// New returns an empty cache configured by opts.
func New[K comparable, V any](opts Options) *Cache[K, V] {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Cache[K, V]{
		ttl:     opts.TTL,
		max:     opts.MaxEntries,
		now:     now,
		entries: make(map[K]*entry[K, V]),
		lru:     list.New(),
		loads:   make(map[K]*loadState[V]),
	}
}

// Get returns the live value stored for key, if any. A hit counts as a use
// for LRU purposes. Expired entries are never returned.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lookupLocked(key, now, true)
}

// Set stores value for key. An existing entry is replaced and its TTL
// restarts (counted from now). Storing counts as a use for LRU purposes.
func (c *Cache[K, V]) Set(key K, value V) {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.storeLocked(key, value, now)
}

// Delete removes key from the cache. It does not stop an in-flight load
// for key; that load's result is still stored when it completes.
func (c *Cache[K, V]) Delete(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.removeLocked(key)
}

// Len returns the number of live (non-expired) entries. Expired entries
// are purged along the way.
func (c *Cache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.purgeExpiredLocked(c.now())
	return len(c.entries)
}

// GetOrLoad returns the live value stored for key. On a miss it calls
// load, sharing any in-flight load for the same key with concurrent
// callers: at most one load per key runs at a time, while loads for
// different keys proceed independently.
//
// The context passed to load is cancelled exactly when every caller
// waiting on that load has given up (returned because its own context
// was done). If the caller's context is done while the load is still
// running, GetOrLoad returns ctx.Err() and does not wait for the load.
// If load returns an error, nothing is cached and every waiter gets that
// error. If load succeeds, the value is stored—even if every caller has
// given up—with its TTL starting when load returned.
func (c *Cache[K, V]) GetOrLoad(ctx context.Context, key K, load func(ctx context.Context) (V, error)) (V, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	now := c.now()
	c.mu.Lock()
	if v, ok := c.lookupLocked(key, now, true); ok {
		c.mu.Unlock()
		return v, nil
	}
	if st, ok := c.loads[key]; ok {
		// A load for this key is already in flight; join it.
		st.waiters++
		c.mu.Unlock()
		return c.waitForLoad(ctx, st)
	}
	if load == nil {
		c.mu.Unlock()
		var zero V
		return zero, errNoLoad
	}
	// The load context is fresh: it is cancelled only when all waiters on
	// this load have given up, never because a single caller was
	// cancelled or the cache was modified.
	lctx, cancel := context.WithCancel(context.Background())
	st := &loadState[V]{done: make(chan struct{}), cancel: cancel, waiters: 1}
	c.loads[key] = st
	c.mu.Unlock()

	go func() {
		defer cancel() // release the context's resources (idempotent)
		v, err := load(lctx)
		now := c.now()
		c.mu.Lock()
		st.value, st.err = v, err
		if err == nil {
			c.storeLocked(key, v, now)
		}
		delete(c.loads, key)
		c.mu.Unlock()
		close(st.done)
	}()
	return c.waitForLoad(ctx, st)
}

// waitForLoad blocks until the load finishes or ctx is done. Every waiter
// that gives up decrements waiters; the load context is cancelled when the
// last waiter leaves.
func (c *Cache[K, V]) waitForLoad(ctx context.Context, st *loadState[V]) (V, error) {
	select {
	case <-st.done:
		return st.value, st.err
	case <-ctx.Done():
		// The load may have finished just as ctx became done; prefer the
		// finished result.
		select {
		case <-st.done:
			return st.value, st.err
		default:
		}
		c.mu.Lock()
		st.waiters--
		if st.waiters <= 0 && !st.stopped {
			st.stopped = true
			st.cancel()
		}
		c.mu.Unlock()
		var zero V
		return zero, ctx.Err()
	}
}

// lookupLocked returns the live value for key. touch moves a hit to the
// front of the LRU list. An expired entry is removed lazily.
func (c *Cache[K, V]) lookupLocked(key K, now time.Time, touch bool) (V, bool) {
	e, ok := c.entries[key]
	if !ok {
		var zero V
		return zero, false
	}
	if e.expired(now) {
		c.lru.Remove(e.elem)
		delete(c.entries, key)
		var zero V
		return zero, false
	}
	if touch {
		c.lru.MoveToFront(e.elem)
	}
	return e.value, true
}

// storeLocked inserts or replaces an entry and enforces MaxEntries.
func (c *Cache[K, V]) storeLocked(key K, value V, now time.Time) {
	// Drop expired entries first, so their presence can never cause a
	// live entry to be evicted.
	c.purgeExpiredLocked(now)
	if e, ok := c.entries[key]; ok {
		e.value = value
		e.expires = c.expiryLocked(now)
		c.lru.MoveToFront(e.elem)
	} else {
		e := &entry[K, V]{key: key, value: value, expires: c.expiryLocked(now)}
		e.elem = c.lru.PushFront(e)
		c.entries[key] = e
	}
	if c.max > 0 {
		for c.lru.Len() > c.max {
			e := c.lru.Back()
			ent := e.Value.(*entry[K, V])
			c.lru.Remove(e)
			delete(c.entries, ent.key)
		}
	}
}

// removeLocked deletes an entry if present.
func (c *Cache[K, V]) removeLocked(key K) {
	if e, ok := c.entries[key]; ok {
		c.lru.Remove(e.elem)
		delete(c.entries, key)
	}
}

// purgeExpiredLocked removes all expired entries.
func (c *Cache[K, V]) purgeExpiredLocked(now time.Time) {
	for e := c.lru.Back(); e != nil; {
		prev := e.Prev()
		ent := e.Value.(*entry[K, V])
		if ent.expired(now) {
			c.lru.Remove(e)
			delete(c.entries, ent.key)
		}
		e = prev
	}
}

// expiryLocked returns the expiry time for an entry stored at now, or the
// zero time when entries never expire.
func (c *Cache[K, V]) expiryLocked(now time.Time) time.Time {
	if c.ttl > 0 {
		return now.Add(c.ttl)
	}
	return time.Time{}
}
