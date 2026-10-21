// Package ttlcache provides a concurrency-safe cache with per-entry TTLs,
// LRU eviction and single-flight loading.
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

type entry[K comparable, V any] struct {
	key     K
	value   V
	expires time.Time // zero means the entry never expires
}

type inflight[V any] struct {
	cancel context.CancelFunc
	done   chan struct{}
	value  V
	err    error
	// waiters counts the callers still waiting on this load. Guarded by
	// Cache.mu, which a completed load also holds while it decides whether
	// to store, so that decision cannot race a caller giving up.
	waiters int
	// claimed is set once a caller has joined this load. A load nobody ever
	// joined (its first caller was gone before it got this far) has no one
	// to report to, so its result is not stored.
	claimed bool
}

// Cache is a TTL cache with optional LRU eviction and single-flight loads.
// It is safe for concurrent use.
type Cache[K comparable, V any] struct {
	ttl        time.Duration
	maxEntries int
	now        func() time.Time

	mu       sync.Mutex
	order    *list.List          // most recently used at the front
	entries  map[K]*list.Element // value is *entry[K, V]
	inFlight map[K]*inflight[V]  // value is *inflight[V]
}

// New returns a new Cache configured by opts. A nil opts.Now means time.Now.
func New[K comparable, V any](opts Options) *Cache[K, V] {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Cache[K, V]{
		ttl:        opts.TTL,
		maxEntries: opts.MaxEntries,
		now:        now,
		order:      list.New(),
		entries:    make(map[K]*list.Element),
		inFlight:   make(map[K]*inflight[V]),
	}
}

// live reports whether e is live at the given time.
func (c *Cache[K, V]) live(e *entry[K, V], at time.Time) bool {
	return e.expires.IsZero() || at.Before(e.expires)
}

// removeElement removes el from both the map and the LRU list.
func (c *Cache[K, V]) removeElement(el *list.Element) {
	e := el.Value.(*entry[K, V])
	c.order.Remove(el)
	delete(c.entries, e.key)
}

// purge drops every expired entry. A time source that runs backwards frees
// no entry; expiry is defined only against the current clock reading.
func (c *Cache[K, V]) purgeLocked(at time.Time) {
	for el := c.order.Back(); el != nil; {
		prev := el.Prev()
		if !c.live(el.Value.(*entry[K, V]), at) {
			c.removeElement(el)
		}
		el = prev
	}
}

// storeLocked stores key with value, starting its TTL now, and refreshes its
// LRU position. The caller holds c.mu.
func (c *Cache[K, V]) storeLocked(key K, value V) {
	now := c.now()
	e := &entry[K, V]{key: key, value: value}
	if c.ttl > 0 {
		e.expires = now.Add(c.ttl)
	}
	if el, ok := c.entries[key]; ok {
		el.Value = e
		c.order.MoveToFront(el)
	} else {
		c.entries[key] = c.order.PushFront(e)
	}
	if c.maxEntries > 0 {
		c.purgeLocked(now)
		for c.order.Len() > c.maxEntries {
			c.removeElement(c.order.Back())
		}
	}
}

// lookupLocked returns key's live value, if any, marking it used on a hit.
func (c *Cache[K, V]) lookupLocked(key K) (V, bool) {
	var zero V
	el, ok := c.entries[key]
	if !ok {
		return zero, false
	}
	e := el.Value.(*entry[K, V])
	if !c.live(e, c.now()) {
		return zero, false
	}
	c.order.MoveToFront(el)
	return e.value, true
}

// Get returns the value stored under key. It reports false for a missing or
// expired entry. A hit counts as use for LRU eviction.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lookupLocked(key)
}

// Set stores value under key and restarts its TTL. Set marks the entry as
// used and evicts least recently used live entries beyond MaxEntries.
func (c *Cache[K, V]) Set(key K, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.storeLocked(key, value)
}

// Delete removes key. It does not stop an in-flight load for key: that
// load's result is stored when it completes.
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
	c.purgeLocked(c.now())
	return c.order.Len()
}

// GetOrLoad returns the live value for key, or calls load and caches its
// result. Concurrent misses for the same key share a single call of load.
func (c *Cache[K, V]) GetOrLoad(ctx context.Context, key K, load func(ctx context.Context) (V, error)) (V, error) {
	var zero V
	// A cancelled caller neither loads nor waits: it fails at once.
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	c.mu.Lock()
	if v, ok := c.lookupLocked(key); ok {
		c.mu.Unlock()
		return v, nil
	}
	l, ok := c.inFlight[key]
	if !ok {
		lctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		l = &inflight[V]{cancel: cancel, done: make(chan struct{})}
		c.inFlight[key] = l
		go c.runLoad(key, l, lctx, load)
	}
	l.waiters++
	l.claimed = true
	c.mu.Unlock()

	select {
	case <-l.done:
		if l.err != nil {
			return zero, l.err
		}
		return l.value, nil
	case <-ctx.Done():
		// This caller has given up on the load. The load itself keeps
		// running and its result is still stored when it completes; its
		// context is cancelled only once no caller waits on it any more.
		// The test is on the resulting count, not on this caller being the
		// last to leave: a caller can join between another's decrement and
		// this lock, and the load must still be cancelled.
		c.mu.Lock()
		l.waiters--
		if l.waiters == 0 {
			l.cancel()
		}
		c.mu.Unlock()
		return zero, ctx.Err()
	}
}

// runLoad runs one load for key and, on success, stores its result when load
// has returned. The goroutine ends with the load it serves.
func (c *Cache[K, V]) runLoad(key K, l *inflight[V], lctx context.Context, load func(context.Context) (V, error)) {
	value, err := load(lctx)

	c.mu.Lock()
	if c.inFlight[key] == l {
		delete(c.inFlight, key)
	}
	if err == nil && l.claimed {
		c.storeLocked(key, value)
	}
	l.value, l.err = value, err
	l.cancel()
	c.mu.Unlock()
	close(l.done)
}
