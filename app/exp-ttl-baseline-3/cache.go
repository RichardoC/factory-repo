// Package ttlcache implements an in-memory cache with per-entry TTL expiry,
// LRU eviction of live entries, and single-flight loading.
//
// It is a model experiment (issue #269): standard library only, no
// dependencies, one small file.
package ttlcache

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// Options configures a Cache.
type Options struct {
	// TTL is the time an entry stays live after it was stored.
	// > 0: an entry expires TTL after it was stored; 0: entries never expire.
	TTL time.Duration

	// MaxEntries bounds the number of live entries.
	// > 0: at most this many live entries; 0: unbounded.
	MaxEntries int

	// Now is the clock. nil means time.Now.
	Now func() time.Time
}

// entry is one cached key/value pair, with its expiry deadline.
type entry[K comparable, V any] struct {
	key      K
	value    V
	expireAt time.Time // zero when entries never expire
}

// call tracks one in-flight load for a key. All GetOrLoad callers that
// arrive while the load runs share it.
type call[K comparable, V any] struct {
	done chan struct{} // closed with Cache.mu held, after the result is final

	val V
	err error

	cancel   func() // cancels the context passed to load
	waiters  int    // callers still waiting, guarded by Cache.mu
	finished bool   // load has returned, guarded by Cache.mu
}

// Cache is a TTL + LRU cache with single-flight loading.
// It is safe for concurrent use.
type Cache[K comparable, V any] struct {
	mu    sync.Mutex
	ttl   time.Duration
	max   int
	now   func() time.Time
	order *list.List          // LRU order, front = most recently used; only live entries
	items map[K]*list.Element // key -> element whose Value is *entry[K, V]
	infl  map[K]*call[K, V]   // in-flight loads, key -> call
}

// New returns a Cache configured by opts.
func New[K comparable, V any](opts Options) *Cache[K, V] {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Cache[K, V]{
		ttl:   opts.TTL,
		max:   opts.MaxEntries,
		now:   now,
		order: list.New(),
		items: make(map[K]*list.Element),
		infl:  make(map[K]*call[K, V]),
	}
}

// Get returns the live value for key. A hit marks the entry as most
// recently used. An expired entry is dropped and reported as a miss.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		var zero V
		return zero, false
	}
	e := el.Value.(*entry[K, V])
	if c.expired(e) {
		c.removeElement(el)
		var zero V
		return zero, false
	}
	c.order.MoveToFront(el)
	return e.value, true
}

// Set stores value for key, restarting its TTL and marking it most recently
// used. If storing it would leave more than MaxEntries live entries, the
// least recently used live entry is evicted first.
func (c *Cache[K, V]) Set(key K, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setLocked(key, value)
}

// Delete removes key. It does not stop an in-flight load for the key: that
// load's successful result is still stored when it completes.
func (c *Cache[K, V]) Delete(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.removeElement(el)
	}
}

// Len returns the number of live entries, dropping expired ones first.
func (c *Cache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.purgeLocked()
	return c.order.Len()
}

// GetOrLoad returns the live value for key, calling load on a miss.
//
// While a load for a key is in flight, concurrent GetOrLoad callers for the
// same key share it: at most one load per key runs at a time, and different
// keys load independently. A live hit returns the cached value without
// calling load.
//
// If load returns an error, nothing is cached and every waiter receives that
// same error. A caller whose ctx is done before the load finishes returns
// ctx.Err() promptly; the load keeps running for remaining waiters, and its
// context is cancelled exactly when every waiter has given up. A successful
// result is stored even if every caller gave up, with its TTL starting when
// load returned.
func (c *Cache[K, V]) GetOrLoad(ctx context.Context, key K, load func(ctx context.Context) (V, error)) (V, error) {
	c.mu.Lock()
	c.purgeLocked()
	if el, ok := c.items[key]; ok {
		e := el.Value.(*entry[K, V])
		c.order.MoveToFront(el)
		v := e.value
		c.mu.Unlock()
		return v, nil
	}
	if call, ok := c.infl[key]; ok {
		call.waiters++
		c.mu.Unlock()
		return c.wait(ctx, call)
	}
	loadCtx, cancel := context.WithCancel(context.Background())
	call := &call[K, V]{done: make(chan struct{}), cancel: cancel, waiters: 1}
	c.infl[key] = call
	c.mu.Unlock()

	go c.doLoad(key, call, loadCtx, load)
	return c.wait(ctx, call)
}

// wait blocks until the load finishes or ctx is done.
func (c *Cache[K, V]) wait(ctx context.Context, call *call[K, V]) (V, error) {
	select {
	case <-call.done:
		return call.val, call.err
	case <-ctx.Done():
		// The load may have finished at the same moment; prefer its result.
		select {
		case <-call.done:
			return call.val, call.err
		default:
		}
		c.mu.Lock()
		call.waiters--
		if call.waiters == 0 && !call.finished {
			// Every waiter has given up: cancel the load's context.
			call.cancel()
		}
		c.mu.Unlock()
		var zero V
		return zero, ctx.Err()
	}
}

// doLoad runs load and publishes its result to every waiter.
func (c *Cache[K, V]) doLoad(key K, call *call[K, V], loadCtx context.Context, load func(ctx context.Context) (V, error)) {
	v, err := load(loadCtx)
	c.mu.Lock()
	call.val, call.err = v, err
	call.finished = true
	if err == nil {
		// Store the result even if every caller gave up (late results);
		// its TTL starts now, when load returned.
		c.setLocked(key, v)
	}
	delete(c.infl, key)
	close(call.done)
	c.mu.Unlock()
	call.cancel() // release the load context's resources
}

// setLocked stores key/value with a fresh TTL, marks it most recently used
// and enforces MaxEntries. Caller must hold c.mu.
func (c *Cache[K, V]) setLocked(key K, value V) {
	c.purgeLocked()
	var expireAt time.Time
	if c.ttl > 0 {
		expireAt = c.now().Add(c.ttl)
	}
	if el, ok := c.items[key]; ok {
		e := el.Value.(*entry[K, V])
		e.value = value
		e.expireAt = expireAt
		c.order.MoveToFront(el)
		return
	}
	e := &entry[K, V]{key: key, value: value, expireAt: expireAt}
	c.items[key] = c.order.PushFront(e)
	if c.max > 0 {
		for c.order.Len() > c.max {
			c.removeElement(c.order.Back())
		}
	}
}

// purgeLocked drops every expired entry. Caller must hold c.mu.
func (c *Cache[K, V]) purgeLocked() {
	if c.ttl <= 0 {
		return
	}
	now := c.now()
	for el := c.order.Front(); el != nil; {
		next := el.Next()
		e := el.Value.(*entry[K, V])
		if !now.Before(e.expireAt) {
			c.removeElement(el)
		}
		el = next
	}
}

// removeElement drops one entry from both the list and the map.
func (c *Cache[K, V]) removeElement(el *list.Element) {
	e := el.Value.(*entry[K, V])
	c.order.Remove(el)
	delete(c.items, e.key)
}

// expired reports whether e is no longer live. An entry stored at time t is
// live while Now() is before t+TTL.
func (c *Cache[K, V]) expired(e *entry[K, V]) bool {
	return c.ttl > 0 && !c.now().Before(e.expireAt)
}
