// Package ttlcache is a small concurrent cache with per-entry TTL, LRU
// eviction and single-flight loading.
//
// It is an experiment module (see README.md): standard library only, one
// exported type and one constructor.
package ttlcache

import (
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

// entry is one cached value. The Cache keeps entries in a doubly linked list
// ordered most-recently used first, so the list holds only live entries at
// the moment it matters; expired entries are unlinked lazily, when something
// walks past them.
type entry[K comparable, V any] struct {
	key     K
	value   V
	expires time.Time // zero means "never expires"
	prev    *entry[K, V]
	next    *entry[K, V]
}

// expired reports whether the entry is dead at time now.
func (e *entry[K, V]) expired(now time.Time) bool {
	return !e.expires.IsZero() && !now.Before(e.expires)
}

// Cache is a cache safe for concurrent use. The zero value is not usable; call
// New.
type Cache[K comparable, V any] struct {
	ttl        time.Duration
	maxEntries int
	now        func() time.Time

	// mu guards every field below. A load's completion takes mu briefly to
	// store its result; it never holds mu while load runs.
	mu      sync.Mutex
	entries map[K]*entry[K, V]
	// head/tail bound the LRU list: head is the most recently used entry,
	// tail the least recently used live one.
	head *entry[K, V]
	tail *entry[K, V]
	// inflight holds the load in progress for each key, at most one per key.
	inflight map[K]*flight[K, V]
}

// New returns ready cache. opts.Now defaults to time.Now; a nil Now in a
// non-nil cache is never dereferenced again.
func New[K comparable, V any](opts Options) *Cache[K, V] {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Cache[K, V]{
		ttl:        opts.TTL,
		maxEntries: opts.MaxEntries,
		now:        now,
		entries:    make(map[K]*entry[K, V]),
		inflight:   make(map[K]*flight[K, V]),
	}
}

// Get returns the live value stored for key, and whether there was one.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		var zero V
		return zero, false
	}
	if e.expired(c.now()) {
		// Rule 1: an expired entry is never returned. Drop it here rather
		// than in the caller so it also stops counting towards Len and
		// towards MaxEntries.
		c.unlink(e)
		delete(c.entries, key)
		var zero V
		return zero, false
	}
	// Rule 3: a hit counts as use: move the entry to the front.
	c.moveToFront(e)
	return e.value, true
}

// Set stores value under key, replacing any previous value and restarting the
// entry's TTL (rule 2).
func (c *Cache[K, V]) Set(key K, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.store(key, value, c.now())
}

// Delete removes key. It deliberately does not disturb a load in flight for
// key: that load's result is still stored when it completes (rule 10).
func (c *Cache[K, V]) Delete(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[key]; ok {
		c.unlink(e)
		delete(c.entries, key)
	}
}

// Len returns the number of live entries, discarding any that have expired.
func (c *Cache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	// Expired entries may sit anywhere in the list, so walk it and unlink
	// every dead entry before counting.
	for e := c.head; e != nil; {
		next := e.next
		if e.expired(now) {
			c.unlink(e)
			delete(c.entries, e.key)
		}
		e = next
	}
	return len(c.entries)
}

// GetOrLoad returns the live value for key, calling load to produce it on a
// miss. Concurrent callers that miss on the same key share one call of load
// (rule 5); callers on different keys load concurrently.
func (c *Cache[K, V]) GetOrLoad(ctx context.Context, key K, load func(ctx context.Context) (V, error)) (V, error) {
	// Rule 4: a live hit is returned without calling load.
	if v, ok := c.Get(key); ok {
		return v, nil
	}
	// Rule 7: a caller whose context is already done gives up at once,
	// without joining the load for that key.
	if err := ctx.Err(); err != nil {
		var zero V
		return zero, err
	}

	c.mu.Lock()
	// Re-check under the lock: a Set, or another load, may have stored a
	// live value between the Get above and here.
	if e, ok := c.entries[key]; ok {
		if e.expired(c.now()) {
			c.unlink(e)
			delete(c.entries, key)
		} else {
			c.moveToFront(e)
			v := e.value
			c.mu.Unlock()
			return v, nil
		}
	}
	f, ok := c.inflight[key]
	if !ok {
		f = &flight[K, V]{key: key, done: make(chan struct{})}
		// Build the load's context here, under c.mu, before the load
		// goroutine can be reached by any waiter: a waiter that gives up
		// immediately calls f.cancel, which must never be nil.
		loadCtx, cancel := context.WithCancel(context.Background())
		f.ctx, f.cancel = loadCtx, cancel
		c.inflight[key] = f
		// The load runs on the caller's goroutine; the cache starts no
		// goroutine of its own beyond this one per flight, so nothing it
		// starts can outlive the load it serves (rule 11). The context
		// handed to load is the flight's own, so giving-up callers can
		// cancel it (rule 8); each caller computes with its own context.
		go c.runLoad(f, key, load)
	}
	// One more waiter: this keeps the flight alive, so a cancellation by
	// another caller cannot cancel the load out from under us.
	f.addWaiter()
	c.mu.Unlock()

	select {
	case <-f.done:
		// Rule 6: everyone waiting on a failed load gets that error and
		// nothing is cached; the next GetOrLoad starts a fresh load.
		return f.val, f.err
	case <-ctx.Done():
		// Rule 7: give up promptly and leave the load running for the
		// callers that are still waiting.
		if f.removeWaiter() {
			// Rule 8: the load's context is cancelled exactly when the
			// last waiter has given up. While a waiter remains it is not,
			// and one caller's cancellation never cancels it.
			f.cancel()
		}
		var zero V
		return zero, ctx.Err()
	}
}

// store inserts or replaces an entry and starts its TTL at now. c.mu must be
// held.
func (c *Cache[K, V]) store(key K, value V, now time.Time) {
	e, ok := c.entries[key]
	if !ok {
		e = &entry[K, V]{key: key}
		c.entries[key] = e
	} else {
		c.unlink(e)
	}
	e.value = value
	// Rule 1 with rule 9: the TTL starts when the value was stored — for a
	// load, when load returned.
	if c.ttl > 0 {
		e.expires = now.Add(c.ttl)
	} else {
		e.expires = time.Time{}
	}
	c.pushFront(e)
	c.evict()
}

// evict drops the least recently used entry while the cache is over capacity.
// c.mu must be held.
//
// The victim is whatever sits at the tail. An expired entry sitting there is
// dropped too, and the loop then looks at the entry behind it — so an expired
// entry never ends the loop in place of a live victim (rule 3). Entries are
// only ever reachable from the list while they are linked, so this also keeps
// the map and the list in step.
func (c *Cache[K, V]) evict() {
	if c.maxEntries <= 0 {
		return
	}
	for len(c.entries) > c.maxEntries && c.tail != nil {
		victim := c.tail
		c.unlink(victim)
		delete(c.entries, victim.key)
	}
}

// unlink removes e from the LRU list. c.mu must be held.
func (c *Cache[K, V]) unlink(e *entry[K, V]) {
	if e.prev != nil {
		e.prev.next = e.next
	} else if c.head == e {
		c.head = e.next
	}
	if e.next != nil {
		e.next.prev = e.prev
	} else if c.tail == e {
		c.tail = e.prev
	}
	e.prev, e.next = nil, nil
}

// pushFront makes e the most recently used entry. c.mu must be held.
func (c *Cache[K, V]) pushFront(e *entry[K, V]) {
	e.prev = nil
	e.next = c.head
	if c.head != nil {
		c.head.prev = e
	}
	c.head = e
	if c.tail == nil {
		c.tail = e
	}
}

// moveToFront marks an already linked entry as most recently used.
func (c *Cache[K, V]) moveToFront(e *entry[K, V]) {
	if c.head == e {
		return
	}
	c.unlink(e)
	c.pushFront(e)
}
