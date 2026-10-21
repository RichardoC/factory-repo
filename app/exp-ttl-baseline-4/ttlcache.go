// Package ttlcache implements a generic in-memory cache with per-entry TTL
// expiry, LRU eviction under a live-entry cap, and single-flight GetOrLoad.
package ttlcache

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// Options configures a Cache.
type Options struct {
	// TTL is how long an entry stays live after it was stored.
	// > 0: an entry expires TTL after it was stored.
	// 0:   entries never expire.
	TTL time.Duration

	// MaxEntries caps the number of live entries.
	// > 0: at most this many live entries; the least recently used live
	// entry is evicted when the cap would be exceeded.
	// 0:   unbounded.
	MaxEntries int

	// Now is the clock the cache reads. nil means time.Now.
	Now func() time.Time
}

// entry is one cached value plus its bookkeeping.
type entry[K comparable, V any] struct {
	key      K
	value    V
	expireAt time.Time     // zero means never expires
	element  *list.Element // position in the LRU list; front = most recent
}

// loadCall tracks one in-flight load and the callers waiting on it.
type loadCall[K comparable, V any] struct {
	done chan struct{} // closed once when the load has finished
	val  V             // written before done is closed
	err  error         // written before done is closed

	// waiters is the number of callers still waiting for this load.
	// It is only mutated while Cache.mu is held.
	waiters int
	// cancel cancels the context that was passed to load. It is invoked
	// when the last waiter gives up, and again (harmlessly) when the
	// load finishes.
	cancel context.CancelFunc
}

// Cache is a TTL + LRU cache that is safe for concurrent use. The zero value
// is not usable; build one with New.
type Cache[K comparable, V any] struct {
	mu         sync.Mutex
	now        func() time.Time
	ttl        time.Duration
	maxEntries int
	entries    map[K]*entry[K, V]
	lru        *list.List // of K; front = most recently used
	inflight   map[K]*loadCall[K, V]
}

// New builds a Cache from opts.
func New[K comparable, V any](opts Options) *Cache[K, V] {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Cache[K, V]{
		now:        now,
		ttl:        opts.TTL,
		maxEntries: opts.MaxEntries,
		entries:    make(map[K]*entry[K, V]),
		lru:        list.New(),
		inflight:   make(map[K]*loadCall[K, V]),
	}
}

// Get returns the live value for key, if any. A hit marks the entry as
// recently used; an expired entry is removed and reported as a miss.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.getLocked(key)
}

func (c *Cache[K, V]) getLocked(key K) (V, bool) {
	e, ok := c.entries[key]
	if !ok {
		var zero V
		return zero, false
	}
	if !c.live(e) {
		c.removeLocked(e)
		var zero V
		return zero, false
	}
	c.lru.MoveToFront(e.element)
	return e.value, true
}

// Set stores value under key. If the key already exists its value is
// replaced and its TTL restarts. Otherwise the key is inserted as the most
// recently used entry; if that would leave more than MaxEntries live
// entries, expired entries are dropped first and then the least recently
// used live entry is evicted.
func (c *Cache[K, V]) Set(key K, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setLocked(key, value)
}

func (c *Cache[K, V]) setLocked(key K, value V) {
	if e, ok := c.entries[key]; ok {
		e.value = value
		e.expireAt = c.expireAt()
		c.lru.MoveToFront(e.element)
		return
	}
	if c.maxEntries > 0 {
		c.dropExpiredLocked()
		for c.lru.Len() >= c.maxEntries {
			c.evictLRULocked()
		}
	}
	c.entries[key] = &entry[K, V]{
		key:      key,
		value:    value,
		expireAt: c.expireAt(),
		element:  c.lru.PushFront(key),
	}
}

// Delete removes key if present. It does not stop an in-flight load for the
// key; that load's result is still stored when it completes.
func (c *Cache[K, V]) Delete(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[key]; ok {
		c.removeLocked(e)
	}
}

// Len returns the number of live entries. Expired entries are removed as a
// side effect, so they are never counted.
func (c *Cache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropExpiredLocked()
	return len(c.entries)
}

// GetOrLoad returns the cached value for key, loading it on a miss.
//
// On a live hit the cached value is returned and load is not called.
// On a miss, concurrent callers for the same key share one call of load:
// at most one load per key runs at a time, and different keys load
// independently. If load returns an error, nothing is cached and every
// caller waiting on that load receives the same error.
//
// A caller whose ctx is done before the load finishes returns ctx.Err()
// promptly; the load keeps running for any remaining waiters. The context
// passed to load is cancelled exactly when every caller waiting on that
// load has given up (or the load has finished). A successful value is
// always stored, even if every caller gave up before it arrived, and its
// TTL starts when load returned.
func (c *Cache[K, V]) GetOrLoad(ctx context.Context, key K, load func(ctx context.Context) (V, error)) (V, error) {
	if v, ok := c.Get(key); ok {
		return v, nil
	}

	c.mu.Lock()
	// A store may have landed between the Get above and this lock.
	if v, ok := c.getLocked(key); ok {
		c.mu.Unlock()
		return v, nil
	}
	if call, ok := c.inflight[key]; ok {
		// Join the load already running for this key. Joining rather
		// than starting a second load is what keeps "at most one load
		// per key at a time" true even after every earlier waiter has
		// given up but the load is still running.
		call.waiters++
		c.mu.Unlock()
		return c.await(ctx, call)
	}

	// First caller for this key: start the load. The load's context keeps
	// the caller's values but not its cancellation; it is cancelled only
	// by the cache, when the last waiter gives up or the load finishes.
	loadCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	call := &loadCall[K, V]{done: make(chan struct{}), waiters: 1, cancel: cancel}
	c.inflight[key] = call
	c.mu.Unlock()

	go func() {
		v, err := load(loadCtx)
		c.mu.Lock()
		if err == nil {
			// Store even if every caller has given up (rule 9);
			// the TTL starts now, when load returned.
			c.setLocked(key, v)
		}
		delete(c.inflight, key)
		call.val, call.err = v, err
		close(call.done)
		cancel()
		c.mu.Unlock()
	}()

	return c.await(ctx, call)
}

// await waits for call to finish, or for ctx to be done first.
func (c *Cache[K, V]) await(ctx context.Context, call *loadCall[K, V]) (V, error) {
	select {
	case <-call.done:
		return call.val, call.err
	case <-ctx.Done():
		// The load may have finished at the same moment the caller's
		// context fired; rule 7 only applies when the context was done
		// *before* the load finished, so prefer the result then.
		select {
		case <-call.done:
			return call.val, call.err
		default:
			c.mu.Lock()
			call.waiters--
			if call.waiters == 0 {
				// Every waiter has given up: cancel the load's
				// context. The load itself keeps running and its
				// result is still stored if it succeeds.
				call.cancel()
			}
			c.mu.Unlock()
			var zero V
			return zero, ctx.Err()
		}
	}
}

// live reports whether e has not expired at the current clock reading.
// An entry stored at t is live while Now() is before t + TTL.
func (c *Cache[K, V]) live(e *entry[K, V]) bool {
	return e.expireAt.IsZero() || c.now().Before(e.expireAt)
}

// expireAt returns the expiry time for an entry stored now.
func (c *Cache[K, V]) expireAt() time.Time {
	if c.ttl <= 0 {
		return time.Time{} // never expires
	}
	return c.now().Add(c.ttl)
}

// dropExpiredLocked removes every expired entry.
func (c *Cache[K, V]) dropExpiredLocked() {
	now := c.now()
	for el := c.lru.Front(); el != nil; {
		next := el.Next()
		e := c.entries[el.Value.(K)]
		if !e.expireAt.IsZero() && !now.Before(e.expireAt) {
			c.removeLocked(e)
		}
		el = next
	}
}

// evictLRULocked removes the least recently used entry, which is live
// because expired entries are dropped before eviction.
func (c *Cache[K, V]) evictLRULocked() {
	el := c.lru.Back()
	if el == nil {
		return
	}
	c.removeLocked(c.entries[el.Value.(K)])
}

func (c *Cache[K, V]) removeLocked(e *entry[K, V]) {
	c.lru.Remove(e.element)
	delete(c.entries, e.key)
}
