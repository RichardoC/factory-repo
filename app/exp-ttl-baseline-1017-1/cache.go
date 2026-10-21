// Package ttlcache implements a generic in-memory cache with per-entry TTL
// expiry, LRU eviction and single-flight loading.
//
// An entry is live while the configured clock is strictly before the entry's
// expiry time (store time plus TTL). Get and GetOrLoad never return an expired
// entry, and Len counts only live entries. When a bounded cache is full, the
// least recently used live entry is evicted; expired entries are removed
// instead and never cause a live entry to be evicted.
//
// GetOrLoad collapses concurrent misses for the same key into a single call
// of the load function. The context passed to load is cancelled exactly when
// every caller waiting on that load has given up, and a value returned by a
// successful load is always cached, with its TTL starting when load returned,
// even if every caller had already given up.
package ttlcache

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// Options configures a Cache.
type Options struct {
	// TTL is how long an entry stays live after it is stored.
	// > 0: an entry expires TTL after it was stored. 0: entries never expire.
	TTL time.Duration

	// MaxEntries is the maximum number of live entries.
	// > 0: at most this many live entries. 0: unbounded.
	MaxEntries int

	// Now is the clock used for expiry. nil means time.Now.
	Now func() time.Time
}

type entry[K comparable, V any] struct {
	key  K
	val  V
	exp  time.Time // zero: never expires
	elem *list.Element
}

// call tracks one in-flight load and the callers waiting on it.
type call[K comparable, V any] struct {
	done chan struct{}
	val  V
	err  error

	mu       sync.Mutex
	waiters  int
	finished bool
	cancel   context.CancelFunc // cancels the context passed to load
}

// join records one more waiting caller.
func (ca *call[K, V]) join() {
	ca.mu.Lock()
	ca.waiters++
	ca.mu.Unlock()
}

// leave records a waiting caller that is returning. If it was the last one
// and the load has not finished yet, the load's context is cancelled.
func (ca *call[K, V]) leave() {
	ca.mu.Lock()
	ca.waiters--
	last := ca.waiters == 0 && !ca.finished
	ca.mu.Unlock()
	if last {
		ca.cancel()
	}
}

// finish records the load result and wakes every waiter.
func (ca *call[K, V]) finish(val V, err error) {
	ca.mu.Lock()
	ca.val, ca.err = val, err
	ca.finished = true
	ca.mu.Unlock()
	close(ca.done)
}

// Cache is an in-memory TTL + LRU cache with single-flight loading.
// It is safe for concurrent use.
type Cache[K comparable, V any] struct {
	opts Options
	now  func() time.Time

	mu       sync.Mutex
	items    map[K]*entry[K, V]
	lru      *list.List // front = least recently used, back = most recently used
	inflight map[K]*call[K, V]
}

// New returns an empty cache configured by opts.
func New[K comparable, V any](opts Options) *Cache[K, V] {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Cache[K, V]{
		opts:     opts,
		now:      now,
		items:    make(map[K]*entry[K, V]),
		lru:      list.New(),
		inflight: make(map[K]*call[K, V]),
	}
}

// Get returns the live value for key, if any, and marks it as used.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.getLocked(key)
}

// Set stores value under key, replacing any existing value and restarting its
// TTL. It may evict the least recently used live entry to respect MaxEntries.
func (c *Cache[K, V]) Set(key K, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setLocked(key, value)
}

// Delete removes key. It does not stop an in-flight load for key: that load's
// result is still stored when it completes.
func (c *Cache[K, V]) Delete(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		c.removeLocked(e)
	}
}

// Len returns the number of live entries.
func (c *Cache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.purgeLocked()
	return len(c.items)
}

// GetOrLoad returns the cached value for key. On a live hit it returns the
// cached value without calling load. On a miss it participates in a single
// call of load for the key: at most one load per key runs at a time, and
// concurrent callers for the same key share one load and its result or error.
//
// If the caller's ctx is done before the load finishes, the caller returns
// ctx.Err() promptly while the load keeps running for the remaining waiters.
// The context passed to load is cancelled exactly when every waiting caller
// has given up. A value returned by a successful load is always cached, with
// its TTL starting when load returned, even if every caller had given up.
func (c *Cache[K, V]) GetOrLoad(ctx context.Context, key K, load func(ctx context.Context) (V, error)) (V, error) {
	c.mu.Lock()
	if v, ok := c.getLocked(key); ok {
		c.mu.Unlock()
		return v, nil
	}
	// Miss. If this caller is already done, do not start or join a load.
	if err := ctx.Err(); err != nil {
		c.mu.Unlock()
		var zero V
		return zero, err
	}
	ca, ok := c.inflight[key]
	if ok {
		ca.join()
	} else {
		ca = &call[K, V]{done: make(chan struct{}), waiters: 1}
		// The load outlives this caller's ctx in general (rule 8), so it is
		// not derived from it; it is cancelled exactly when the last waiting
		// caller gives up.
		loadCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		ca.cancel = cancel
		c.inflight[key] = ca
		go c.doLoad(key, ca, load, loadCtx)
	}
	c.mu.Unlock()

	select {
	case <-ca.done: // load already finished; prefer its result
		ca.leave()
		return ca.val, ca.err
	default:
	}
	select {
	case <-ctx.Done():
		ca.leave()
		var zero V
		return zero, ctx.Err()
	case <-ca.done:
		ca.leave()
		return ca.val, ca.err
	}
}

// doLoad runs load and publishes its result. It runs in its own goroutine,
// which ends when load returns.
func (c *Cache[K, V]) doLoad(key K, ca *call[K, V], load func(ctx context.Context) (V, error), loadCtx context.Context) {
	v, err := load(loadCtx)

	c.mu.Lock()
	if err == nil {
		c.setLocked(key, v)
	}
	if c.inflight[key] == ca {
		delete(c.inflight, key)
	}
	c.mu.Unlock()

	ca.finish(v, err)
	// The load is over; release the context's resources. Cancel is idempotent,
	// and a final waiter's leave may have cancelled it already.
	ca.cancel()
}

// getLocked returns the live entry for key, marking it as used, and removes
// the entry if it has expired. c.mu must be held.
func (c *Cache[K, V]) getLocked(key K) (V, bool) {
	var zero V
	e, ok := c.items[key]
	if !ok {
		return zero, false
	}
	if c.expired(e) {
		c.removeLocked(e)
		return zero, false
	}
	c.lru.MoveToBack(e.elem)
	return e.val, true
}

// setLocked stores key/value, restarting its TTL, and evicts if needed.
// c.mu must be held.
func (c *Cache[K, V]) setLocked(key K, value V) {
	c.purgeLocked()
	e, ok := c.items[key]
	if ok {
		e.val = value
		e.exp = c.expiry()
		c.lru.MoveToBack(e.elem)
	} else {
		e = &entry[K, V]{key: key, val: value, exp: c.expiry()}
		e.elem = c.lru.PushBack(e)
		c.items[key] = e
	}
	if max := c.opts.MaxEntries; max > 0 {
		for len(c.items) > max {
			front := c.lru.Front()
			if front == nil {
				break
			}
			c.removeLocked(front.Value.(*entry[K, V]))
		}
	}
}

// expiry returns the expiry time for an entry stored now.
func (c *Cache[K, V]) expiry() time.Time {
	if c.opts.TTL <= 0 {
		return time.Time{} // never expires
	}
	return c.now().Add(c.opts.TTL)
}

// expired reports whether e is no longer live: live while Now() < store+TTL.
func (c *Cache[K, V]) expired(e *entry[K, V]) bool {
	return !e.exp.IsZero() && !c.now().Before(e.exp)
}

// purgeLocked removes every expired entry so that eviction decisions and Len
// only consider live entries. c.mu must be held.
func (c *Cache[K, V]) purgeLocked() {
	for el := c.lru.Front(); el != nil; {
		next := el.Next()
		if e := el.Value.(*entry[K, V]); c.expired(e) {
			c.removeLocked(e)
		}
		el = next
	}
}

// removeLocked unlinks e from the list and the map. c.mu must be held.
func (c *Cache[K, V]) removeLocked(e *entry[K, V]) {
	c.lru.Remove(e.elem)
	delete(c.items, e.key)
}
