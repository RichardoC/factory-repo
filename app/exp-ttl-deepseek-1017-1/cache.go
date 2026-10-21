// Package ttlcache is a small concurrency-safe cache with per-entry TTLs,
// LRU eviction bounded by MaxEntries, and single-flight loads.
package ttlcache

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// Options configures a Cache.
type Options struct {
	TTL        time.Duration    // > 0: an entry expires TTL after it was stored; 0: entries never expire
	MaxEntries int              // > 0: at most this many live entries; 0: unbounded
	Now        func() time.Time // the clock; nil means time.Now
}

// Cache is a TTL + LRU cache. It is safe for concurrent use.
//
// Expiry is lazy: an entry is treated as gone once Now() reaches its
// deadline, is not returned by Get or GetOrLoad, is not counted by Len and
// is swept (never evicting a live entry) before any LRU eviction.
type Cache[K comparable, V any] struct {
	ttl        time.Duration
	maxEntries int
	now        func() time.Time

	mu       sync.Mutex
	entries  map[K]*list.Element // key -> element whose Value is *entry[K, V]
	lru      *list.List          // front = most recently used; entries only
	inflight map[K]*call[K, V]   // key -> the single running load
}

// entry is one live (or not-yet-swept) cached value.
type entry[K comparable, V any] struct {
	key     K
	value   V
	expires time.Time // zero: never expires
}

// call is one in-flight load shared by every waiting GetOrLoad caller.
type call[K comparable, V any] struct {
	key K

	// ctx is the load's context. Its cancel func is called once the last
	// waiter gives up (or when the load finishes) and never before, so the
	// load is not cancelled while any waiter remains.
	ctx    context.Context
	cancel context.CancelFunc

	waiters int  // callers still waiting, updated under Cache.mu
	gone    bool // cancel has been called

	done chan struct{} // closed when the load has finished and stored its result
	err  error         // valid once done is closed
	val  V             // the loaded value, valid once done is closed and err is nil

	// stopped is closed when the load func has actually returned; the
	// goroutine then exits, so nothing outlives the load it serves.
	stopped chan struct{}
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
		entries:    make(map[K]*list.Element),
		lru:        list.New(),
		inflight:   make(map[K]*call[K, V]),
	}
}

// Get returns the live value for key.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[key]
	if !ok {
		var zero V
		return zero, false
	}
	e := el.Value.(*entry[K, V])
	if c.expired(e, c.now()) {
		c.removeElement(el)
		var zero V
		return zero, false
	}
	c.lru.MoveToFront(el)
	return e.value, true
}

// Set stores value for key, replacing any previous value and restarting the
// entry's TTL. An existing key counts as used.
func (c *Cache[K, V]) Set(key K, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.store(key, value, c.now())
}

// Delete removes key. An in-flight load for key keeps running, and its result
// is still stored when it completes.
func (c *Cache[K, V]) Delete(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[key]; ok {
		c.removeElement(el)
	}
}

// Len returns the number of live entries.
func (c *Cache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.sweep(now)
	return len(c.entries)
}

// GetOrLoad returns the live value for key, calling load on a miss. Concurrent
// callers for the same key share one call of load; different keys load
// independently. If load fails the error is returned to every waiter and
// nothing is cached.
func (c *Cache[K, V]) GetOrLoad(ctx context.Context, key K, load func(ctx context.Context) (V, error)) (V, error) {
	if v, ok := c.Get(key); ok {
		return v, nil
	}

	c.mu.Lock()
	if v, ok := c.getLocked(key); ok { // re-check under the same lock
		c.mu.Unlock()
		return v, nil
	}
	cl, ok := c.inflight[key]
	if !ok {
		loadCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		cl = &call[K, V]{
			key:     key,
			ctx:     loadCtx,
			cancel:  cancel,
			done:    make(chan struct{}),
			stopped: make(chan struct{}),
		}
		c.inflight[key] = cl
		go c.run(cl, load)
	}
	cl.waiters++
	c.mu.Unlock()
	return c.wait(ctx, cl)
}

// getLocked is Get's body for a caller already holding c.mu.
func (c *Cache[K, V]) getLocked(key K) (V, bool) {
	el, ok := c.entries[key]
	if !ok {
		var zero V
		return zero, false
	}
	e := el.Value.(*entry[K, V])
	if c.expired(e, c.now()) {
		c.removeElement(el)
		var zero V
		return zero, false
	}
	c.lru.MoveToFront(el)
	return e.value, true
}

// wait blocks until cl's load finishes or ctx is done. It always gives up its
// waiter slot, which may cancel the load's context when nobody is left.
func (c *Cache[K, V]) wait(ctx context.Context, cl *call[K, V]) (V, error) {
	var zero V
	select {
	case <-cl.done:
		c.release(cl)
		return c.result(cl)
	case <-ctx.Done():
		// The load may have finished at the same time as ctx; a value is
		// more useful than an error the caller no longer needs.
		select {
		case <-cl.done:
			c.release(cl)
			return c.result(cl)
		default:
		}
		c.release(cl)
		return zero, ctx.Err()
	}
}

// result returns the finished load's outcome. The caller has already released
// its waiter slot, so this never blocks on another load.
func (c *Cache[K, V]) result(cl *call[K, V]) (V, error) {
	var zero V
	if cl.err != nil {
		return zero, cl.err
	}
	// Return what the load produced even if the entry was deleted or already
	// evicted: these callers were waiting on that load, not on the cache.
	return cl.val, nil
}

// release drops one waiter; the last one to leave cancels the load's context.
func (c *Cache[K, V]) release(cl *call[K, V]) {
	c.mu.Lock()
	cl.waiters--
	if cl.waiters == 0 && !cl.gone {
		cl.gone = true
		cl.cancel()
	}
	c.mu.Unlock()
}

// run performs the single load and publishes its result.
func (c *Cache[K, V]) run(cl *call[K, V], load func(context.Context) (V, error)) {
	defer close(cl.stopped)
	v, err := load(cl.ctx)

	c.mu.Lock()
	if c.inflight[cl.key] == cl {
		delete(c.inflight, cl.key)
	}
	cl.err = err
	cl.val = v
	if err == nil {
		// Rule 9: a successful value is stored with its TTL starting now,
		// even if every caller gave up and the key was deleted meanwhile.
		c.store(cl.key, v, c.now())
	}
	if !cl.gone {
		cl.gone = true
		cl.cancel() // release the load's context; nothing waits on it now
	}
	c.mu.Unlock()
	close(cl.done)
}

// expired reports whether e is expired at now.
func (c *Cache[K, V]) expired(e *entry[K, V], now time.Time) bool {
	if c.ttl <= 0 || e.expires.IsZero() {
		return false
	}
	return !now.Before(e.expires)
}

// store inserts or replaces key, then enforces MaxEntries over live entries.
// The caller holds c.mu.
func (c *Cache[K, V]) store(key K, value V, now time.Time) {
	var expires time.Time
	if c.ttl > 0 {
		expires = now.Add(c.ttl)
	}
	if el, ok := c.entries[key]; ok {
		e := el.Value.(*entry[K, V])
		e.value = value
		e.expires = expires
		c.lru.MoveToFront(el)
	} else {
		e := &entry[K, V]{key: key, value: value, expires: expires}
		c.entries[key] = c.lru.PushFront(e)
	}
	if c.maxEntries <= 0 {
		return
	}
	// Expired entries must never cause a live one to be evicted.
	c.sweep(now)
	for len(c.entries) > c.maxEntries {
		back := c.lru.Back()
		if back == nil {
			break
		}
		c.removeElement(back)
	}
}

// sweep drops every expired entry. The caller holds c.mu.
func (c *Cache[K, V]) sweep(now time.Time) {
	for e := c.lru.Back(); e != nil; {
		prev := e.Prev()
		if c.expired(e.Value.(*entry[K, V]), now) {
			c.removeElement(e)
		}
		e = prev
	}
}

// removeElement drops el from both the list and the map. The caller holds c.mu.
func (c *Cache[K, V]) removeElement(el *list.Element) {
	e := el.Value.(*entry[K, V])
	c.lru.Remove(el)
	delete(c.entries, e.key)
}
