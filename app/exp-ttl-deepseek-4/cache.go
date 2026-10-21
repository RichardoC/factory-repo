// Package ttlcache implements a small, concurrency-safe in-memory cache with
// per-entry time-to-live expiry and least-recently-used eviction.
//
// This file owns the expiry/LRU engine: Options, Cache, New, Get, Set, Delete
// and Len, plus the lock-held internals that load.go also uses. The load path
// (GetOrLoad, call[V] and the in-flight table) lives in load.go; the two files
// share the frozen unexported layout declared here, so neither defines a symbol
// the other owns.
package ttlcache

import (
	"container/list"
	"sync"
	"time"
)

// Options configures a Cache.
type Options struct {
	// TTL is how long an entry stays live. TTL > 0 means an entry expires TTL
	// after it was stored; TTL == 0 means entries never expire. A negative TTL
	// is treated exactly like 0: entries never expire.
	TTL time.Duration
	// MaxEntries bounds the number of live entries. MaxEntries > 0 means at
	// most this many live entries are kept, evicting the least recently used
	// live entry as needed; MaxEntries == 0 means unbounded. A negative value
	// is treated exactly like 0.
	MaxEntries int
	// Now is the clock the cache reads to stamp stores and to decide expiry. A
	// nil Now means time.Now.
	Now func() time.Time
}

// entryV is one cache slot. The zero value means "absent".
type entryV[V any] struct {
	value   V
	stored  time.Time // Now() at store time; the TTL window starts here
	used    uint64    // last-use stamp, strictly increasing
	present bool
}

// Cache is a TTL + LRU cache. The zero Cache is not usable; call New. A Cache
// is safe for concurrent use by multiple goroutines.
type Cache[K comparable, V any] struct {
	now        func() time.Time // NEVER nil: New fills in time.Now
	ttl        time.Duration    // 0 = never expires
	maxEntries int              // 0 = unbounded

	mu      sync.Mutex          // guards everything below
	entries map[K]*entryV[V]    // key -> pointer, value mutated in place
	lru     list.List           // front = most recently used; Value = K
	refs    map[K]*list.Element // key -> its lru element
	stamp   uint64              // last handed-out use stamp

	calls map[K]*call[V] // load.go: in-flight loads on this cache
}

// New returns an empty Cache configured by opts.
func New[K comparable, V any](opts Options) *Cache[K, V] {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	ttl := opts.TTL
	if ttl < 0 {
		// Documented choice: only TTL > 0 expires entries, so TTL == 0 and any
		// negative TTL mean "never expires".
		ttl = 0
	}
	maxEntries := opts.MaxEntries
	if maxEntries < 0 {
		// A negative limit is meaningless; treat it as unbounded, like 0.
		maxEntries = 0
	}
	return &Cache[K, V]{
		now:        now,
		ttl:        ttl,
		maxEntries: maxEntries,
		entries:    make(map[K]*entryV[V]),
		refs:       make(map[K]*list.Element),
		calls:      make(map[K]*call[V]),
	}
}

// Get returns the live value stored for key. It reports false for an absent,
// deleted or expired key; an entry found expired is removed and never returned.
// A hit counts as a use for LRU purposes.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	e, ok := c.entries[key]
	if !ok || !c.liveLocked(e, now) {
		if ok {
			c.dropKeyLocked(key)
		}
		var zero V
		return zero, false
	}
	c.touchLocked(key)
	return e.value, true
}

// Set stores value for key. Storing an existing key replaces the value and
// restarts its TTL from now. A store counts as a use for LRU purposes.
func (c *Cache[K, V]) Set(key K, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.storeLocked(key, value, c.now())
}

// Delete removes key from the cache. Deleting an absent key is a no-op. It does
// not stop an in-flight load for the key: that load's result is still stored
// when it completes.
func (c *Cache[K, V]) Delete(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropKeyLocked(key)
}

// Len returns the number of live entries. Expired entries are reaped, not
// counted. On a small map the O(n) sweep is fine.
func (c *Cache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropExpiredLocked(c.now())
	return len(c.entries)
}

// liveLocked reports whether e is present and unexpired at now. An entry stored
// at t is live while now < t+ttl; with ttl == 0 it never expires. c.mu must be
// held.
func (c *Cache[K, V]) liveLocked(e *entryV[V], now time.Time) bool {
	if e == nil || !e.present {
		return false
	}
	if c.ttl <= 0 {
		return true
	}
	return now.Before(e.stored.Add(c.ttl))
}

// touchLocked marks key as most recently used and moves it to the front of the
// LRU list. c.mu must be held.
func (c *Cache[K, V]) touchLocked(key K) {
	e := c.entries[key]
	if e == nil {
		return
	}
	c.stamp++
	e.used = c.stamp
	if el, ok := c.refs[key]; ok {
		c.lru.MoveToFront(el)
		return
	}
	c.refs[key] = c.lru.PushFront(key)
}

// storeLocked inserts or replaces key with value, restarts its TTL from
// stored, counts the store as a use, then trims the cache to maxEntries.
// c.mu must be held.
func (c *Cache[K, V]) storeLocked(key K, value V, stored time.Time) {
	e, ok := c.entries[key]
	if !ok {
		e = &entryV[V]{}
		c.entries[key] = e
	}
	e.value = value
	e.stored = stored
	e.present = true
	c.touchLocked(key)
	c.evictLocked(stored)
}

// evictLocked trims the cache down to maxEntries live entries, oldest use
// stamp first. Expired entries are dropped before anything else, so an expired
// entry is always freed before a live one: no live entry is evicted while an
// expired entry could go instead. c.mu must be held.
func (c *Cache[K, V]) evictLocked(now time.Time) {
	if c.maxEntries <= 0 {
		return
	}
	// Reap first: this is what keeps an expired entry from costing a live one.
	c.dropExpiredLocked(now)
	// Trim only while the live count exceeds the limit. Everything left is live
	// (dropExpiredLocked just ran), so len(entries) is the live count here.
	for len(c.entries) > c.maxEntries {
		back := c.lru.Back()
		if back == nil {
			return
		}
		key, _ := back.Value.(K)
		c.dropKeyLocked(key)
	}
}

// dropExpiredLocked removes every entry that has already expired at now.
// c.mu must be held.
func (c *Cache[K, V]) dropExpiredLocked(now time.Time) {
	if c.ttl <= 0 {
		return
	}
	for key, e := range c.entries {
		if !c.liveLocked(e, now) {
			c.dropKeyLocked(key)
		}
	}
}

// dropKeyLocked removes key from the entries map and the LRU index. Removing an
// absent key is a no-op. c.mu must be held.
func (c *Cache[K, V]) dropKeyLocked(key K) {
	if _, ok := c.entries[key]; !ok {
		return
	}
	delete(c.entries, key)
	if el, ok := c.refs[key]; ok {
		c.lru.Remove(el)
		delete(c.refs, key)
	}
}
