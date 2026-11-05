// Package ttlcache implements an in-memory cache with a per-entry TTL,
// optional least-recently-used size bounding, and single-flight loading.
//
// All exported methods are safe for concurrent use by multiple goroutines.
// The cache starts one goroutine per in-flight load, which ends when the
// load returns.
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
	// TTL is how long an entry stays live. An entry stored at time t
	// (as read by Now) is live while Now() is before t+TTL; at t+TTL
	// it is expired. Zero means entries never expire. Negative values
	// are treated as zero.
	TTL time.Duration

	// MaxEntries bounds the number of live entries: storing an entry
	// that would leave more than MaxEntries live entries first evicts
	// the least recently used live entry. Expired entries are dropped
	// and never cause a live entry to be evicted. Zero means unbounded.
	// Negative values are treated as zero.
	MaxEntries int

	// Now is the cache's clock. A nil Now means time.Now.
	Now func() time.Time
}

// A Cache is a TTL + LRU cache with single-flight loading. The zero
// value is not usable; construct one with New.
//
// A Cache is safe for concurrent use. Get, Set, Delete, Len and
// GetOrLoad may be called from any goroutine.
type Cache[K comparable, V any] struct {
	ttl        time.Duration
	maxEntries int
	now        func() time.Time

	mu    sync.Mutex
	items map[K]*entry[K, V]
	lru   *list.List // of *entry[K, V]; the front is the most recently used

	loads map[K]*loadCall[K, V]
}

// entry is one cached key/value pair.
type entry[K comparable, V any] struct {
	key      K
	value    V
	expireAt time.Time // zero means never
	elem     *list.Element
}

// live reports whether the entry is still live at the given time: it is
// live while now is before expireAt (rule 1).
func (e *entry[K, V]) live(now time.Time) bool {
	return e.expireAt.IsZero() || now.Before(e.expireAt)
}

// loadCall tracks a single in-flight load for one key: the generation of
// GetOrLoad callers waiting on it, the context handed to load, and the
// result once it finishes.
type loadCall[K comparable, V any] struct {
	key     K
	done    chan struct{}      // closed once, when the load has finished
	val     V                  // written before done is closed
	err     error              // written before done is closed
	loadCtx context.Context    // passed to load
	cancel  context.CancelFunc // cancels loadCtx
	waiters int                // callers still waiting on this load, guarded by Cache.mu
}

var errNilLoad = errors.New("ttlcache: load must not be nil")

// New returns a Cache with the given options. A nil Now means time.Now;
// TTL <= 0 and MaxEntries <= 0 mean never-expiring and unbounded.
func New[K comparable, V any](opts Options) *Cache[K, V] {
	ttl := opts.TTL
	if ttl < 0 {
		ttl = 0
	}
	max := opts.MaxEntries
	if max < 0 {
		max = 0
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Cache[K, V]{
		ttl:        ttl,
		maxEntries: max,
		now:        now,
		items:      make(map[K]*entry[K, V]),
		lru:        list.New(),
		loads:      make(map[K]*loadCall[K, V]),
	}
}

// Get returns the cached value for key, if there is one and it has not
// expired (rule 1). A hit marks the entry as used (rule 3).
func (c *Cache[K, V]) Get(key K) (V, bool) {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[key]
	if !ok {
		var zero V
		return zero, false
	}
	if !e.live(now) {
		c.removeEntryLocked(e)
		var zero V
		return zero, false
	}
	c.lru.MoveToFront(e.elem)
	return e.value, true
}

// Set stores the value for key, replacing any previous value and
// restarting its TTL (rule 2). Storing marks the entry as used (rule 3)
// and may evict live entries per the LRU rule, after expired entries are
// dropped.
func (c *Cache[K, V]) Set(key K, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.storeLocked(key, value, c.now())
}

// Delete removes key from the cache. It does not stop an in-flight load
// for the key: that load's result is still stored when it completes
// (rule 10).
func (c *Cache[K, V]) Delete(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		c.removeEntryLocked(e)
	}
}

// Len returns the number of live entries; expired entries are removed
// and not counted (rule 1).
func (c *Cache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneExpiredLocked(c.now())
	return len(c.items)
}

// GetOrLoad returns the cached value for key if there is one and it has
// not expired (rule 1, rule 4: load is not called on a hit). On a miss
// it calls load, starting at most one load at a time for the key: all
// concurrent callers for the same key share that one call (rule 5).
// Loads for different keys run independently (rule 5).
//
// If load returns an error, nothing is cached and every waiting caller
// gets that same error; a later GetOrLoad calls load again (rule 6). A
// caller whose ctx is done before the load finishes returns ctx.Err()
// promptly (rule 7), while the load keeps running for its remaining
// callers. The context passed to load is cancelled exactly when every
// caller waiting on that load has stopped waiting (rule 8), and a
// successful result is stored with its TTL measured from when load
// returned, even if every caller has since given up (rule 9).
//
// A nil load or nil ctx is an error; the caller's ctx carries no values
// into load besides those inheritable from context.WithoutCancel of the
// first caller to start the load.
func (c *Cache[K, V]) GetOrLoad(ctx context.Context, key K, load func(context.Context) (V, error)) (V, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	now := c.now()
	var zero V

	c.mu.Lock()
	if e, ok := c.items[key]; ok {
		if e.live(now) {
			c.lru.MoveToFront(e.elem)
			v := e.value
			c.mu.Unlock()
			return v, nil
		}
		c.removeEntryLocked(e)
	}
	if load == nil {
		c.mu.Unlock()
		return zero, errNilLoad
	}
	if call, ok := c.loads[key]; ok {
		// A load for this key is already running: join it (rule 5).
		call.waiters++
		c.mu.Unlock()
		return c.waitLoad(ctx, call)
	}
	// Start the load. It runs on a goroutine the cache starts, so that
	// a caller whose ctx is done can return promptly while the load
	// keeps running for the others (rule 7).
	call := &loadCall[K, V]{
		key:  key,
		done: make(chan struct{}),
	}
	call.loadCtx, call.cancel = context.WithCancel(context.WithoutCancel(ctx))
	call.waiters = 1
	c.loads[key] = call
	c.mu.Unlock()
	go c.runLoad(key, call, load)
	return c.waitLoad(ctx, call)
}

// waitLoad blocks until the load finishes or ctx is done. Either way the
// caller stops waiting: the last one to leave cancels the load's context
// and forgets the call (rule 8), whether it left with a result or by
// giving up.
func (c *Cache[K, V]) waitLoad(ctx context.Context, call *loadCall[K, V]) (V, error) {
	var zero V
	select {
	case <-call.done:
		c.mu.Lock()
		call.waiters--
		if call.waiters == 0 {
			c.releaseLoadLocked(call)
		}
		c.mu.Unlock()
		return call.val, call.err
	case <-ctx.Done():
		c.mu.Lock()
		call.waiters--
		if call.waiters == 0 {
			c.releaseLoadLocked(call)
		}
		c.mu.Unlock()
		return zero, ctx.Err()
	}
}

// releaseLoadLocked cancels the context passed to the load of call --
// which can no longer affect any waiter's result -- and forgets call, so
// that a later GetOrLoad for the key starts a fresh load (rule 6).
func (c *Cache[K, V]) releaseLoadLocked(call *loadCall[K, V]) {
	call.cancel()
	if cur, ok := c.loads[call.key]; ok && cur == call {
		delete(c.loads, call.key)
	}
}

// runLoad is the goroutine the cache starts for one load (rule 11): it
// ends when the load returns. It stores a successful result even if no
// caller is still waiting, with its TTL measured from when load returned
// (rule 9), and wakes every waiter.
func (c *Cache[K, V]) runLoad(key K, call *loadCall[K, V], load func(context.Context) (V, error)) {
	v, err := load(call.loadCtx)
	finished := c.now()
	call.val = v
	call.err = err
	c.mu.Lock()
	if err == nil {
		c.storeLocked(key, v, finished)
	}
	if cur, ok := c.loads[key]; ok && cur == call {
		delete(c.loads, key)
	}
	c.mu.Unlock()
	close(call.done)
}

// storeLocked inserts or replaces key with value. The entry's TTL and
// its recency are measured at t. Expired entries are dropped first, so
// they never cause a live entry to be evicted (rule 3). c.mu must be
// held.
func (c *Cache[K, V]) storeLocked(key K, value V, t time.Time) {
	if e, ok := c.items[key]; ok {
		// Replacement: update in place and restart the TTL (rule 2).
		e.value = value
		e.expireAt = c.expiry(t)
		c.lru.MoveToFront(e.elem)
		return
	}
	c.pruneExpiredLocked(t)
	for c.maxEntries > 0 && c.lru.Len() >= c.maxEntries {
		c.removeEntryLocked(c.lru.Back().Value.(*entry[K, V]))
	}
	e := &entry[K, V]{key: key, value: value, expireAt: c.expiry(t)}
	e.elem = c.lru.PushFront(e)
	c.items[key] = e
}

// expiry returns the time at which an entry stored at t expires, or the
// zero time if entries never expire.
func (c *Cache[K, V]) expiry(t time.Time) time.Time {
	if c.ttl <= 0 {
		return time.Time{}
	}
	return t.Add(c.ttl)
}

// pruneExpiredLocked removes every entry that is expired at now. c.mu
// must be held.
func (c *Cache[K, V]) pruneExpiredLocked(now time.Time) {
	for _, e := range c.items {
		if !e.live(now) {
			c.removeEntryLocked(e)
		}
	}
}

// removeEntryLocked drops an entry from the map and the LRU list. c.mu
// must be held.
func (c *Cache[K, V]) removeEntryLocked(e *entry[K, V]) {
	delete(c.items, e.key)
	c.lru.Remove(e.elem)
}
