// Package ttlcache is a small in-memory cache with per-entry TTLs, LRU
// eviction and single-flight loading. It is an experiment, not production
// code.
//
// # Design notes
//
// Waiter counting and load-context cancellation (rule 8). Each key maps to a
// *call object built by the first caller to miss; later callers for the same
// key find the call under the cache mutex and increment its waiter count.
// The context handed to load is derived once, at call creation, from
// context.WithoutCancel(context.Background()), so it inherits no caller's
// deadline or values; the call owns its cancel func. A caller that gives up
// (its own ctx is done, or it is woken after the call finished) decrements
// the waiter count, and the cancel func is invoked exactly at the transition
// to zero. Cancelling one of several callers therefore never reaches load's
// context, and the context stays live for as long as any waiter remains.
//
// A single goroutine per call runs load, publishes the result and then
// removes itself and the call from the map under the mutex, so no goroutine
// started here outlives the load it serves. Callers wait on a per-call done
// channel, which is closed exactly once by that goroutine.
//
// LRU order. Entries live in a container/list in least-recently-used-first
// order, with an index map from key to element and a key map from key to
// entry. Every mutation ("use") - Set, a completed load, or a hit in Get or
// GetOrLoad - moves the key's element to the back of the list. Get, Set,
// Delete and Len each take the mutex, drop expired entries they touch (or all
// of them, for Len), and never hold the mutex across load: an in-flight load
// is only reachable through the calls map.
package ttlcache

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// Options configures a Cache.
type Options struct {
	// TTL is how long an entry stays live after it was stored. Zero means
	// entries never expire. A negative TTL behaves like zero.
	TTL time.Duration
	// MaxEntries is the maximum number of live entries. Zero (or negative)
	// means unbounded.
	MaxEntries int
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
}

// entry is one stored value together with the time it was stored.
type entry[V any] struct {
	value   V
	expires time.Time // zero means "never expires"
}

// call is a single in-flight load for one key. It is created by the first
// caller to miss and shared by every caller that misses while it runs.
type call[V any] struct {
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	mu       sync.Mutex // guards waiters; held only for short transitions
	waiters  int        // callers still waiting on this load
	finished bool       // the load returned and done was closed
	value    V          // valid once finished
	err      error      // valid once finished
}

// startCall registers a caller as a waiter.
func (c *call[V]) startCall() {
	c.mu.Lock()
	c.waiters++
	c.mu.Unlock()
}

// releaseCall removes a caller and cancels the load context if it was the
// last one waiting. It reports whether this caller was the last waiter.
func (c *call[V]) releaseCall() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.waiters--
	if c.waiters == 0 {
		c.cancel()
		return true
	}
	return false
}

// finish publishes the load result to the waiting callers and wakes them.
func (c *call[V]) finish(value V, err error) {
	c.mu.Lock()
	c.value = value
	c.err = err
	c.finished = true
	c.mu.Unlock()
	close(c.done)
}

// result returns the load outcome. It is only called after done is closed.
func (c *call[V]) result() (V, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.value, c.err
}

// Cache is a TTL cache with LRU eviction and single-flight loading.
type Cache[K comparable, V any] struct {
	mu         sync.Mutex
	ttl        time.Duration
	maxEntries int
	now        func() time.Time

	entries map[K]*entry[V]
	lru     *list.List          // front = least recently used
	elems   map[K]*list.Element // element value is the key
	calls   map[K]*call[V]      // in-flight loads, never held across load
}

// New returns an empty cache configured by opts.
func New[K comparable, V any](opts Options) *Cache[K, V] {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Cache[K, V]{
		ttl:        opts.TTL,
		maxEntries: opts.MaxEntries,
		now:        now,
		entries:    make(map[K]*entry[V]),
		lru:        list.New(),
		elems:      make(map[K]*list.Element),
		calls:      make(map[K]*call[V]),
	}
}

// live reports whether e stored at the given instant is live at now.
func (c *Cache[K, V]) live(e *entry[V], now time.Time) bool {
	if c.ttl <= 0 || e.expires.IsZero() {
		return true
	}
	return now.Before(e.expires)
}

// getLocked reports whether key holds a live entry at now, dropping the
// entry if it is expired. c.mu must be held.
func (c *Cache[K, V]) getLocked(key K, now time.Time) (*entry[V], bool) {
	e, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if !c.live(e, now) {
		c.removeLocked(key)
		return nil, false
	}
	return e, true
}

// removeLocked drops key from the index and the LRU list. c.mu must be held.
func (c *Cache[K, V]) removeLocked(key K) {
	delete(c.entries, key)
	if el, ok := c.elems[key]; ok {
		c.lru.Remove(el)
		delete(c.elems, key)
	}
}

// touchLocked moves key to the back of the LRU list, the most recently used
// end. c.mu must be held.
func (c *Cache[K, V]) touchLocked(key K) {
	if el, ok := c.elems[key]; ok {
		c.lru.MoveToBack(el)
	}
}

// storeLocked stores value under key, replacing any existing entry, dropping
// expired entries and evicting the least recently used live entry while the
// cache is over MaxEntries. The TTL starts at now. c.mu must be held.
func (c *Cache[K, V]) storeLocked(key K, value V, now time.Time) {
	delete(c.entries, key)
	if el, ok := c.elems[key]; ok {
		c.lru.Remove(el)
		delete(c.elems, key)
	}

	var expires time.Time
	if c.ttl > 0 {
		expires = now.Add(c.ttl)
	}
	c.entries[key] = &entry[V]{value: value, expires: expires}
	c.elems[key] = c.lru.PushBack(key)

	if c.maxEntries <= 0 {
		return
	}
	// Drop expired entries first: they never cause a live eviction.
	for el := c.lru.Front(); el != nil; {
		next := el.Next()
		k := el.Value.(K)
		if e, ok := c.entries[k]; ok && !c.live(e, now) {
			c.removeLocked(k)
		}
		el = next
	}
	for c.lru.Len() > c.maxEntries {
		el := c.lru.Front()
		if el == nil {
			break
		}
		c.removeLocked(el.Value.(K))
	}
}

// Get returns the live value stored under key.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	var zero V
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.getLocked(key, c.now())
	if !ok {
		return zero, false
	}
	c.touchLocked(key)
	return e.value, true
}

// Set stores value under key, replacing any existing entry and restarting
// its TTL.
func (c *Cache[K, V]) Set(key K, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.storeLocked(key, value, c.now())
}

// Delete removes key from the cache. It does not stop an in-flight load for
// key; that load's result is stored when it completes.
func (c *Cache[K, V]) Delete(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.removeLocked(key)
}

// Len returns the number of live entries.
func (c *Cache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for key, e := range c.entries {
		if !c.live(e, now) {
			c.removeLocked(key)
		}
	}
	return len(c.entries)
}

// GetOrLoad returns the live value stored under key, or calls load to produce
// it. Concurrent misses for the same key share a single load call.
func (c *Cache[K, V]) GetOrLoad(ctx context.Context, key K, load func(ctx context.Context) (V, error)) (V, error) {
	var zero V
	if err := ctx.Err(); err != nil {
		return zero, err
	}

	c.mu.Lock()
	if e, ok := c.getLocked(key, c.now()); ok {
		c.touchLocked(key)
		v := e.value
		c.mu.Unlock()
		return v, nil
	}
	cl, ok := c.calls[key]
	if !ok {
		loadCtx, cancel := context.WithCancel(context.WithoutCancel(context.Background()))
		cl = &call[V]{
			ctx:    loadCtx,
			cancel: cancel,
			done:   make(chan struct{}),
		}
		c.calls[key] = cl
		go c.run(key, cl, load)
	}
	cl.startCall()
	c.mu.Unlock()

	defer cl.releaseCall()

	select {
	case <-cl.done:
		return cl.result()
	case <-ctx.Done():
		return zero, ctx.Err()
	}
}

// run executes one load for key and publishes its result. It is the only
// goroutine the cache starts, and it removes the call from the map before it
// returns, so it never outlives the load it serves.
func (c *Cache[K, V]) run(key K, cl *call[V], load func(context.Context) (V, error)) {
	value, err := load(cl.ctx)

	c.mu.Lock()
	cl.finish(value, err)
	delete(c.calls, key)
	if err == nil {
		c.storeLocked(key, value, c.now())
	}
	c.mu.Unlock()
	cl.cancel()
}
