// Package ttlcache implements a small, concurrency-safe cache with per-entry
// TTL expiry, least-recently-used eviction and single-flight loading.
//
// The cache holds one mutex that guards the map and the LRU list; it is never
// held across a caller-supplied load function or across a wait on a caller's
// context. Each in-flight load owns a small state value (mutex + sync.Cond,
// a cancellable context derived from context.Background() and a waiter count)
// and exactly one goroutine, which exits as soon as load returns.
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

// entry is one key/value pair plus its bookkeeping. expiresAt is the zero time
// when the cache has no TTL. elem is its node in the LRU list.
type entry[K comparable, V any] struct {
	key       K
	value     V
	expiresAt time.Time // zero means "never"
	elem      *list.Element
}

// Cache is a TTL + LRU cache safe for concurrent use by multiple goroutines.
type Cache[K comparable, V any] struct {
	ttl        time.Duration
	maxEntries int
	now        func() time.Time

	mu    sync.Mutex
	items map[K]*entry[K, V]
	lru   *list.List // front = most recently used; values are *entry[K, V]
	loads map[K]*loadState[K, V]
}

// loadState is the shared state of one in-flight load for one key.
type loadState[K comparable, V any] struct {
	key K

	mu   sync.Mutex
	cond *sync.Cond

	done     bool
	value    V
	err      error
	waiters  int                // callers still waiting on this load
	cancel   context.CancelFunc // cancels the context handed to load
	canceled bool               // cancel already called (idempotence guard)

	// quit is the set of waiters that have given up, keyed by their own waker
	// channel. It only ever holds channels of callers still inside await, so
	// it cannot grow past the number of live waiters.
	quit map[chan struct{}]struct{}
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
		items:      make(map[K]*entry[K, V]),
		lru:        list.New(),
		loads:      make(map[K]*loadState[K, V]),
	}
}

// expired reports whether e is no longer live at time now. An entry with a
// zero expiresAt never expires.
func (e *entry[K, V]) expired(now time.Time) bool {
	return !e.expiresAt.IsZero() && !now.Before(e.expiresAt)
}

// removeLocked unlinks e from the LRU list and the map. The caller holds c.mu.
func (c *Cache[K, V]) removeLocked(e *entry[K, V]) {
	c.lru.Remove(e.elem)
	delete(c.items, e.key)
}

// purgeExpiredLocked drops every expired entry except keep, the key being
// stored right now. Purging first is what guarantees that an expired entry is
// never the reason a live entry is evicted. The caller holds c.mu.
func (c *Cache[K, V]) purgeExpiredLocked(now time.Time, keep K) {
	if c.ttl <= 0 {
		return
	}
	var next *list.Element
	for e := c.lru.Back(); e != nil; e = next {
		next = e.Prev()
		en := e.Value.(*entry[K, V])
		if en.key != keep && en.expired(now) {
			c.removeLocked(en)
		}
	}
}

// storeLocked inserts or replaces key, restarts its TTL and marks it most
// recently used, purging expired entries and evicting the least recently used
// live entry when MaxEntries would be exceeded. The caller holds c.mu.
func (c *Cache[K, V]) storeLocked(key K, value V, now time.Time) {
	var expiresAt time.Time
	if c.ttl > 0 {
		expiresAt = now.Add(c.ttl)
	}

	if old, ok := c.items[key]; ok {
		old.value = value
		old.expiresAt = expiresAt
		c.lru.MoveToFront(old.elem)
		return
	}

	c.purgeExpiredLocked(now, key)

	// Make room before inserting, so the list never holds more than
	// MaxEntries live entries. Only live entries can be victims: the expired
	// ones have just gone.
	if c.maxEntries > 0 {
		for c.lru.Len() >= c.maxEntries {
			victim := c.lru.Back()
			if victim == nil {
				break
			}
			c.removeLocked(victim.Value.(*entry[K, V]))
		}
	}

	en := &entry[K, V]{key: key, value: value, expiresAt: expiresAt}
	en.elem = c.lru.PushFront(en)
	c.items[key] = en
}

// lookupLocked returns a live entry for key, marking it most recently used.
// The caller holds c.mu.
func (c *Cache[K, V]) lookupLocked(key K, now time.Time) (*entry[K, V], bool) {
	en, ok := c.items[key]
	if !ok {
		return nil, false
	}
	if en.expired(now) {
		c.removeLocked(en)
		return nil, false
	}
	c.lru.MoveToFront(en.elem)
	return en, true
}

// Get returns the live value stored for key, if any.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	var zero V
	c.mu.Lock()
	defer c.mu.Unlock()
	en, ok := c.lookupLocked(key, c.now())
	if !ok {
		return zero, false
	}
	return en.value, true
}

// Set stores value under key, replacing any previous value and restarting the
// entry's TTL.
func (c *Cache[K, V]) Set(key K, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.storeLocked(key, value, c.now())
}

// Delete removes key from the cache. An in-flight load for the key is not
// stopped: that load's result is stored when it completes.
func (c *Cache[K, V]) Delete(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if en, ok := c.items[key]; ok {
		c.removeLocked(en)
	}
}

// Len returns the number of live entries.
func (c *Cache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	n := 0
	for e := c.lru.Front(); e != nil; e = e.Next() {
		if !e.Value.(*entry[K, V]).expired(now) {
			n++
		}
	}
	return n
}

// GetOrLoad returns the live value for key if there is one. Otherwise it calls
// load, at most once at a time per key, sharing its result with every other
// caller waiting on the same key. On error nothing is cached.
func (c *Cache[K, V]) GetOrLoad(ctx context.Context, key K, load func(ctx context.Context) (V, error)) (V, error) {
	var zero V
	if ctx.Err() != nil {
		return zero, ctx.Err()
	}

	c.mu.Lock()
	if en, ok := c.lookupLocked(key, c.now()); ok {
		v := en.value
		c.mu.Unlock()
		return v, nil
	}

	if st, ok := c.loads[key]; ok {
		// A load is already in flight for this key: become a waiter.
		st.addWaiter()
		c.mu.Unlock()
		return c.await(ctx, st)
	}

	// Miss with no load in flight: start the one load for this key. The
	// state is registered under c.mu, so the load goroutine's removal of it
	// is serialised against every other GetOrLoad.
	st := &loadState[K, V]{key: key}
	st.cond = sync.NewCond(&st.mu)
	loadCtx, cancel := context.WithCancel(context.Background())
	st.cancel = cancel
	st.waiters++
	c.loads[key] = st
	c.mu.Unlock()

	go c.runLoad(st, loadCtx, load)

	return c.await(ctx, st)
}

// runLoad executes the user's load function for st and publishes its result.
// The goroutine exits as soon as load returns; no per-key state or context it
// owns outlives it.
func (c *Cache[K, V]) runLoad(st *loadState[K, V], loadCtx context.Context, load func(context.Context) (V, error)) {
	value, err := load(loadCtx)
	// The load has returned; its TTL starts here.
	returnedAt := c.now()

	st.mu.Lock()
	st.value = value
	st.err = err
	st.done = true
	cancel := st.cancel
	st.canceled = true
	st.mu.Unlock()

	cancel() // the load's own goroutine is done with loadCtx
	st.cond.Broadcast()

	c.mu.Lock()
	delete(c.loads, st.key)
	if err == nil {
		// A late result is still stored, even when every caller gave up.
		c.storeLocked(st.key, value, returnedAt)
	}
	c.mu.Unlock()
}

// addWaiter registers one more waiter on st. The caller holds c.mu, which
// serialises this against the load goroutine removing the state.
func (st *loadState[K, V]) addWaiter() {
	st.mu.Lock()
	st.waiters++
	st.mu.Unlock()
}

// await waits for st to finish or for ctx to be done, whichever comes first.
// It returns the loaded value or the load's error; a caller whose ctx is done
// before the load finishes returns ctx.Err() promptly instead.
//
// When the load has already completed, await reports the load's result even if
// ctx is done: a waiter woken by ctx.Done at the same instant as the load must
// not be turned into a second load, and must never trigger a second load.
//
// Each waiter has a waker of its own, so the load's complete Broadcast wakes
// everyone, no one polls, and no callback outlives its call.
func (c *Cache[K, V]) await(ctx context.Context, st *loadState[K, V]) (V, error) {
	var zero V
	if ctx.Done() == nil {
		// A context that can never be done: a plain cond wait suffices.
		st.mu.Lock()
		defer st.mu.Unlock()
		for !st.done {
			st.cond.Wait()
		}
		return st.value, st.err
	}

	wake := make(chan struct{})
	st.mu.Lock()
	if st.quit == nil {
		st.quit = make(map[chan struct{}]struct{})
	}
	st.quit[wake] = struct{}{}
	st.mu.Unlock()

	// The callback records this waiter as gone and wakes the load's waiters,
	// so a caller in cond.Wait learns about it at once rather than by polling.
	stop := context.AfterFunc(ctx, func() {
		st.mu.Lock()
		delete(st.quit, wake)
		if !st.done {
			st.waiters--
			if st.waiters == 0 && !st.canceled {
				// This was the last waiter: cancel the load's context.
				st.canceled = true
				st.cancel()
			}
		}
		st.cond.Broadcast()
		st.mu.Unlock()
	})
	defer stop()

	st.mu.Lock()
	for !st.done {
		if _, waiting := st.quit[wake]; !waiting {
			// Our own AfterFunc has run: we have given up. It already
			// dropped our waiter, so return its error.
			st.mu.Unlock()
			stop()
			return zero, ctx.Err()
		}
		st.cond.Wait()
	}
	v, err := st.value, st.err
	st.mu.Unlock()
	stop()
	return v, err
}
