// Package ttlcache implements a generic in-memory cache with per-entry TTL
// expiry, LRU eviction of live entries, and singleflight semantics for
// concurrent loads of the same key.
//
// This is a model experiment (issue #82, baseline copy 2); it is not meant
// for merging.
package ttlcache

import (
	"container/list"
	"context"
	"errors"
	"sync"
	"time"
)

// Options configures a Cache.
type Options struct {
	// TTL > 0: an entry expires TTL after it was stored.
	// TTL == 0: entries never expire.
	TTL time.Duration

	// MaxEntries > 0: at most this many live entries are kept; storing
	// beyond that evicts the least recently used live entry.
	// MaxEntries == 0: unbounded.
	MaxEntries int

	// Now is the clock used for expiry; nil means time.Now.
	Now func() time.Time
}

// entry is a stored value plus its expiry deadline and its LRU node.
type entry[K comparable, V any] struct {
	key       K
	value     V
	expiresAt time.Time // zero means never expires
	elem      *list.Element
}

// loadCall is a single in-flight load shared by every GetOrLoad caller
// that arrives while it runs.
type loadCall[K comparable, V any] struct {
	done chan struct{} // closed exactly once, when load has returned

	// Written by the load goroutine before done is closed, and read by
	// waiters only after done is closed, so no extra locking is needed.
	value V
	err   error

	cancel context.CancelFunc // cancels the context passed to load

	// waiters is guarded by the owning cache's mutex.
	waiters int
}

// Cache is a TTL + LRU cache. It is safe for concurrent use.
type Cache[K comparable, V any] struct {
	mu      sync.Mutex
	now     func() time.Time
	ttl     time.Duration
	maxEnt  int
	entries map[K]*entry[K, V]
	lru     *list.List // front = most recently used; elem.Value is *entry[K, V]
	loads   map[K]*loadCall[K, V]
}

// New returns a Cache configured by opts.
func New[K comparable, V any](opts Options) *Cache[K, V] {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Cache[K, V]{
		now:     now,
		ttl:     opts.TTL,
		maxEnt:  opts.MaxEntries,
		entries: make(map[K]*entry[K, V]),
		lru:     list.New(),
		loads:   make(map[K]*loadCall[K, V]),
	}
}

// live reports whether e is live at the current clock reading.
func (c *Cache[K, V]) live(e *entry[K, V], now time.Time) bool {
	return e.expiresAt.IsZero() || now.Before(e.expiresAt)
}

// Get returns the live value stored for key, if any. A hit marks the entry
// as most recently used; an expired entry is dropped and reported as a miss.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.getLocked(key, c.now())
}

// getLocked returns the live value for key, dropping it if expired, and
// marks a hit as most recently used. c.mu must be held.
func (c *Cache[K, V]) getLocked(key K, now time.Time) (V, bool) {
	e, ok := c.entries[key]
	if !ok {
		var zero V
		return zero, false
	}
	if !c.live(e, now) {
		c.removeLocked(e)
		var zero V
		return zero, false
	}
	c.lru.MoveToFront(e.elem)
	return e.value, true
}

// Set stores value for key, replacing any previous value and restarting its
// TTL, then evicts least-recently-used live entries if needed. Expired
// entries never cause a live entry to be evicted.
func (c *Cache[K, V]) Set(key K, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.storeLocked(key, value, c.now())
}

// Delete removes key. It does not stop an in-flight load for the key: that
// load's result is still stored when it completes.
func (c *Cache[K, V]) Delete(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[key]; ok {
		c.removeLocked(e)
	}
}

// Len returns the number of live entries. Expired entries that have not yet
// been touched are not counted.
func (c *Cache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	n := 0
	for _, e := range c.entries {
		if c.live(e, now) {
			n++
		}
	}
	return n
}

// GetOrLoad returns the cached value for key, or on a miss loads it with
// load, stores it and returns it.
//
// Concurrent GetOrLoad callers for the same key share a single call of load.
// If load fails, nothing is cached and every waiter receives that same
// error. A caller whose ctx is done before the load finishes returns ctx.Err
// promptly; the load keeps running for the remaining waiters, and the
// context passed to load is cancelled exactly when every waiter has given
// up (or the load has returned). A successful result is stored even if every
// caller gave up, with its TTL starting when load returned.
func (c *Cache[K, V]) GetOrLoad(ctx context.Context, key K, load func(ctx context.Context) (V, error)) (V, error) {
	if v, ok := c.Get(key); ok {
		return v, nil
	}
	if load == nil {
		return *new(V), errors.New("ttlcache: nil load function")
	}

	c.mu.Lock()
	lc, ok := c.loads[key]
	if !ok {
		// The load for this key may have completed between our first
		// Get and here, already storing the value; don't load again.
		if v, ok := c.getLocked(key, c.now()); ok {
			c.mu.Unlock()
			return v, nil
		}
		lc = &loadCall[K, V]{done: make(chan struct{}), waiters: 1}
		c.loads[key] = lc
		// The load's context carries ctx's values but not its
		// cancellation: it is cancelled only when every waiter has
		// given up, which may be never, long after this caller left.
		loadCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		lc.cancel = cancel
		go c.runLoad(key, lc, load, loadCtx)
	} else {
		lc.waiters++ // join the in-flight load
	}
	c.mu.Unlock()
	return c.waitLoad(ctx, lc)
}

// waitLoad blocks until the load finishes or the caller's ctx is done.
func (c *Cache[K, V]) waitLoad(ctx context.Context, lc *loadCall[K, V]) (V, error) {
	select {
	case <-lc.done:
		return lc.value, lc.err
	case <-ctx.Done():
	}

	c.mu.Lock()
	// The load may have finished in the same instant the context fired;
	// prefer its result in that case.
	select {
	case <-lc.done:
		c.mu.Unlock()
		return lc.value, lc.err
	default:
	}
	lc.waiters--
	if lc.waiters <= 0 {
		// Every caller has given up; stop the load.
		lc.cancel()
	}
	c.mu.Unlock()

	var zero V
	return zero, ctx.Err()
}

// runLoad runs load once for a loadCall, broadcasts the result to every
// waiter and stores a successful value.
func (c *Cache[K, V]) runLoad(key K, lc *loadCall[K, V], load func(ctx context.Context) (V, error), loadCtx context.Context) {
	v, err := load(loadCtx)

	c.mu.Lock()
	lc.value, lc.err = v, err
	delete(c.loads, key)
	close(lc.done)
	if err == nil {
		// Rule 9: store even if every caller gave up; the TTL starts
		// when load returned, not when it was started.
		c.storeLocked(key, v, c.now())
	}
	c.mu.Unlock()

	lc.cancel() // the load is served; release the context's resources
}

// storeLocked stores value for key at time now and evicts if needed.
// c.mu must be held.
func (c *Cache[K, V]) storeLocked(key K, value V, now time.Time) {
	if e, ok := c.entries[key]; ok {
		if !c.live(e, now) {
			// The stored entry has expired: drop it and store this
			// as a fresh entry, so the eviction count below still
			// holds (an expired entry occupied no live slot).
			c.removeLocked(e)
		} else {
			// Replacement: new value, restarted TTL, most
			// recently used; the live-entry count is unchanged.
			e.value = value
			if c.ttl > 0 {
				e.expiresAt = now.Add(c.ttl)
			} else {
				e.expiresAt = time.Time{}
			}
			c.lru.MoveToFront(e.elem)
			return
		}
	}

	e := &entry[K, V]{key: key, value: value}
	if c.ttl > 0 {
		e.expiresAt = now.Add(c.ttl)
	}
	e.elem = c.lru.PushFront(e)
	c.entries[key] = e

	c.evictLocked(now)
}

// evictLocked drops expired entries met along the way and evicts
// least-recently-used live entries until at most maxEnt live entries remain.
// c.mu must be held.
func (c *Cache[K, V]) evictLocked(now time.Time) {
	if c.maxEnt <= 0 {
		return
	}
	live := 0
	for _, e := range c.entries {
		if c.live(e, now) {
			live++
		}
	}
	excess := live - c.maxEnt
	if excess <= 0 {
		return
	}
	for e := c.lru.Back(); e != nil && excess > 0; {
		prev := e.Prev()
		ent := e.Value.(*entry[K, V])
		if c.live(ent, now) {
			c.removeLocked(ent)
			excess--
		} else {
			// Expired entries are not live and never cause a
			// live entry to be evicted; just drop them.
			c.removeLocked(ent)
		}
		e = prev
	}
}

// removeLocked removes e from the map and the LRU list. c.mu must be held.
func (c *Cache[K, V]) removeLocked(e *entry[K, V]) {
	delete(c.entries, e.key)
	c.lru.Remove(e.elem)
}
