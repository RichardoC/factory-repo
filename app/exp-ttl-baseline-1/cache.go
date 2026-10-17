// Package ttlcache implements a small in-memory cache with per-entry TTL,
// LRU eviction and single-flight loading.
//
// Semantics:
//
//   - An entry stored at time t (by the configured clock) is live while the
//     clock is strictly before t+TTL. Get, GetOrLoad and Len never observe
//     expired entries.
//   - At most MaxEntries live entries are kept; when a store needs room,
//     expired entries are dropped first and then the least recently used
//     live entry is evicted.
//   - Concurrent GetOrLoad calls for the same key share a single load call.
//     The context passed to load is cancelled exactly when every caller
//     waiting on that load has given up. A successful load result is stored
//     even if every caller gave up before it returned.
package ttlcache

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// Options configures a Cache.
type Options struct {
	// TTL > 0: an entry expires TTL after it was stored; 0: entries never
	// expire. A negative TTL is treated like 0.
	TTL time.Duration

	// MaxEntries > 0: at most this many live entries are kept; 0: unbounded.
	// A negative value is treated like 0.
	MaxEntries int

	// Now is the clock used for expiry; nil means time.Now.
	Now func() time.Time
}

// entry is one cached item; it lives in the LRU list and the map.
type entry[K comparable, V any] struct {
	key       K
	value     V
	expiresAt time.Time // zero means "never expires" (only used when TTL == 0)
}

// loadCall is a single in-flight load that concurrent GetOrLoad callers share.
type loadCall[K comparable, V any] struct {
	done   chan struct{} // closed once when the load has returned
	cancel context.CancelFunc

	mu       sync.Mutex
	waiters  int // callers currently blocked in wait
	finished bool
	val      V
	err      error
}

// A Cache maps keys to values with per-entry TTL and LRU eviction. It is
// safe for concurrent use.
type Cache[K comparable, V any] struct {
	mu      sync.Mutex
	ttl     time.Duration
	max     int
	now     func() time.Time
	entries map[K]*list.Element
	lru     *list.List // front = most recently used; values are *entry[K,V]
	loads   map[K]*loadCall[K, V]
}

// New returns an empty cache configured by opts.
func New[K comparable, V any](opts Options) *Cache[K, V] {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	ttl := opts.TTL
	if ttl < 0 {
		ttl = 0
	}
	max := opts.MaxEntries
	if max < 0 {
		max = 0
	}
	return &Cache[K, V]{
		ttl:     ttl,
		max:     max,
		now:     now,
		entries: make(map[K]*list.Element),
		lru:     list.New(),
		loads:   make(map[K]*loadCall[K, V]),
	}
}

// Get returns the live value for key, if any. A hit marks the entry as most
// recently used; an expired entry is removed and reported as a miss.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.getLocked(key)
}

// Set stores value under key, replacing any existing value and restarting
// its TTL. If the cache is bounded, expired entries are dropped first and
// then the least recently used live entry is evicted to make room.
func (c *Cache[K, V]) Set(key K, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.storeLocked(key, value)
}

// Delete removes key from the cache. It does not stop an in-flight load for
// key; that load's result is still stored when it completes.
func (c *Cache[K, V]) Delete(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[key]; ok {
		c.removeElementLocked(el)
	}
}

// Len returns the number of live entries.
func (c *Cache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ttl <= 0 {
		return len(c.entries)
	}
	now := c.now()
	n := 0
	for el := c.lru.Front(); el != nil; el = el.Next() {
		if e := el.Value.(*entry[K, V]); now.Before(e.expiresAt) {
			n++
		}
	}
	return n
}

// GetOrLoad returns the live value for key, calling load on a miss.
//
// On a hit the cached value is returned without calling load. On a miss,
// concurrent GetOrLoad calls for the same key share a single load call. If
// load returns an error, nothing is cached and every caller waiting on that
// load receives the same error. A caller whose ctx is done before the load
// finishes returns ctx.Err() promptly; the load keeps running for the other
// waiters, and the context passed to load is cancelled exactly when every
// caller waiting on it has given up. A value load returns successfully is
// stored even if every caller gave up, with its TTL starting when load
// returned.
func (c *Cache[K, V]) GetOrLoad(ctx context.Context, key K, load func(ctx context.Context) (V, error)) (V, error) {
	c.mu.Lock()
	if v, ok := c.getLocked(key); ok {
		c.mu.Unlock()
		return v, nil
	}
	call, joining := c.loads[key]
	if !joining {
		// First caller for this key: start the load. The load context
		// carries this caller's values but is never cancelled by any
		// caller's ctx; it is cancelled only when every waiter has
		// given up.
		loadCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		call = &loadCall[K, V]{done: make(chan struct{}), cancel: cancel}
		c.loads[key] = call
		go c.runLoad(key, call, loadCtx, load)
	}
	c.mu.Unlock()
	return call.wait(ctx)
}

// runLoad runs load once for a key, stores a successful result and publishes
// it to every waiter. It runs in its own goroutine, which ends when load
// returns.
func (c *Cache[K, V]) runLoad(key K, call *loadCall[K, V], loadCtx context.Context, load func(ctx context.Context) (V, error)) {
	v, err := load(loadCtx)

	c.mu.Lock()
	// Detach the call so the next GetOrLoad for this key starts fresh
	// (a failed load is not cached). The comparison keeps us from
	// removing a newer call's entry in the (rare) case where this call
	// was already replaced.
	if c.loads[key] == call {
		delete(c.loads, key)
	}
	if err == nil {
		c.storeLocked(key, v)
	}
	c.mu.Unlock()

	call.mu.Lock()
	call.val, call.err = v, err
	call.finished = true
	call.mu.Unlock()
	close(call.done)
}

// wait blocks the caller until the load returns or the caller's ctx is done.
func (call *loadCall[K, V]) wait(ctx context.Context) (V, error) {
	call.mu.Lock()
	call.waiters++
	call.mu.Unlock()

	var zero V
	select {
	case <-call.done:
		call.mu.Lock()
		call.waiters--
		v, err := call.val, call.err
		call.mu.Unlock()
		return v, err
	case <-ctx.Done():
		// The load may have finished at the same moment the caller's
		// context was cancelled; prefer its result.
		select {
		case <-call.done:
			call.mu.Lock()
			call.waiters--
			v, err := call.val, call.err
			call.mu.Unlock()
			return v, err
		default:
		}
		call.mu.Lock()
		call.waiters--
		if call.waiters == 0 && !call.finished {
			// Every caller waiting on this load has given up.
			call.cancel()
		}
		call.mu.Unlock()
		return zero, ctx.Err()
	}
}

// getLocked returns the live value for key and marks it most recently used.
// Caller holds c.mu.
func (c *Cache[K, V]) getLocked(key K) (V, bool) {
	el, ok := c.entries[key]
	if !ok {
		var zero V
		return zero, false
	}
	e := el.Value.(*entry[K, V])
	if c.expiredLocked(e) {
		c.removeElementLocked(el)
		var zero V
		return zero, false
	}
	c.lru.MoveToFront(el)
	return e.value, true
}

// storeLocked inserts or replaces key. Caller holds c.mu.
func (c *Cache[K, V]) storeLocked(key K, value V) {
	if el, ok := c.entries[key]; ok {
		e := el.Value.(*entry[K, V])
		e.value = value
		e.expiresAt = c.expiryLocked()
		c.lru.MoveToFront(el)
		return
	}
	c.makeRoomLocked()
	el := c.lru.PushFront(&entry[K, V]{key: key, value: value, expiresAt: c.expiryLocked()})
	c.entries[key] = el
}

// makeRoomLocked evicts so that one more entry fits within MaxEntries:
// expired entries first (they never cause a live entry to be evicted), then
// the least recently used live entry. Caller holds c.mu.
func (c *Cache[K, V]) makeRoomLocked() {
	if c.max <= 0 {
		return
	}
	if c.lru.Len() >= c.max {
		c.sweepExpiredLocked()
	}
	for c.lru.Len() >= c.max {
		c.removeElementLocked(c.lru.Back())
	}
}

// sweepExpiredLocked removes every expired entry. Caller holds c.mu.
func (c *Cache[K, V]) sweepExpiredLocked() {
	if c.ttl <= 0 {
		return
	}
	now := c.now()
	for el := c.lru.Back(); el != nil; {
		prev := el.Prev()
		if e := el.Value.(*entry[K, V]); !now.Before(e.expiresAt) {
			c.removeElementLocked(el)
		}
		el = prev
	}
}

// expiredLocked reports whether e is past its TTL. Caller holds c.mu.
func (c *Cache[K, V]) expiredLocked(e *entry[K, V]) bool {
	return c.ttl > 0 && !c.now().Before(e.expiresAt)
}

// expiryLocked returns the expiry timestamp for an entry stored now.
func (c *Cache[K, V]) expiryLocked() time.Time {
	if c.ttl <= 0 {
		return time.Time{}
	}
	return c.now().Add(c.ttl)
}

func (c *Cache[K, V]) removeElementLocked(el *list.Element) {
	e := el.Value.(*entry[K, V])
	delete(c.entries, e.key)
	c.lru.Remove(el)
}
