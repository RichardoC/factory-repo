// Package ttlcache implements a concurrency-safe, generic in-memory cache
// with per-entry TTL, LRU eviction and single-flight loading.
//
// A value stored at time t (as reported by Options.Now) is live while
// Now() is before t + Options.TTL. Expired entries are never returned and
// never cause a live entry to be evicted.
package ttlcache

import (
	"context"
	"sync"
	"time"
)

// Options configures a Cache.
type Options struct {
	// TTL is how long an entry stays live after it was stored.
	// A value greater than zero makes entries expire TTL after they were
	// stored; zero means entries never expire.
	TTL time.Duration
	// MaxEntries, when greater than zero, is the maximum number of live
	// entries the cache holds. Zero means unbounded.
	MaxEntries int
	// Now is the clock the cache consults for expiry. A nil Now means
	// time.Now.
	Now func() time.Time
}

// now returns the current time according to the configured clock.
func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// expiresAt returns the expiry time for a value stored at t, or the zero
// time.Time when entries never expire.
func (o Options) expiresAt(t time.Time) time.Time {
	if o.TTL <= 0 {
		return time.Time{}
	}
	return t.Add(o.TTL)
}

// node is an intrusive doubly-linked-list node keyed by K, ordering cache
// entries from most to least recently used.
type node[K comparable] struct {
	key  K
	prev *node[K]
	next *node[K]
}

// lruList holds the entries in use order. head is the most recently used
// entry, tail the least recently used one.
type lruList[K comparable] struct {
	head *node[K]
	tail *node[K]
}

// remove unlinks n from the list.
func (l *lruList[K]) remove(n *node[K]) {
	if n.prev != nil {
		n.prev.next = n.next
	} else {
		l.head = n.next
	}
	if n.next != nil {
		n.next.prev = n.prev
	} else {
		l.tail = n.prev
	}
	n.prev, n.next = nil, nil
}

// pushFront moves n to the head of the list.
func (l *lruList[K]) pushFront(n *node[K]) {
	n.prev = nil
	n.next = l.head
	if l.head != nil {
		l.head.prev = n
	} else {
		l.tail = n
	}
	l.head = n
}

// entry is one cached value. expiresAt is the zero time.Time when the
// entry never expires; lruNode is its position in the cache's LRU list.
type entry[K comparable, V any] struct {
	value     V
	expiresAt time.Time
	lruNode   *node[K]
}

// Cache is a TTL + LRU cache. The zero value is not usable; build one with
// New. It is safe for concurrent use.
type Cache[K comparable, V any] struct {
	opts Options
	mu   sync.Mutex
	// items and lru always hold exactly the same set of keys: an entry is
	// inserted into and removed from both together, under mu.
	items map[K]*entry[K, V]
	lru   lruList[K]
	// loads holds the in-flight GetOrLoad call, if any, per key.
	loads map[K]*loadCall[K, V]
}

// New returns a cache with the given options. A nil Options.Now means
// time.Now.
func New[K comparable, V any](opts Options) *Cache[K, V] {
	return &Cache[K, V]{
		opts:  opts,
		items: make(map[K]*entry[K, V]),
		loads: make(map[K]*loadCall[K, V]),
	}
}

// Get returns the live value stored under key, if any. A hit counts as a
// use of the entry for LRU purposes.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.getLocked(key)
	if e == nil {
		var zero V
		return zero, false
	}
	c.lru.remove(e.lruNode)
	c.lru.pushFront(e.lruNode)
	return e.value, true
}

// Set stores value under key. An existing key has its value replaced and
// its TTL restarted. Storing counts as a use.
func (c *Cache[K, V]) Set(key K, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.storeLocked(key, value)
}

// Delete removes key from the cache. It does not stop an in-flight load
// for key; that load's result is still stored when it completes.
func (c *Cache[K, V]) Delete(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		c.removeLocked(key, e)
	}
}

// Len returns the number of live entries.
func (c *Cache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.liveCountLocked()
}

// GetOrLoad returns the live value stored under key, loading it with load
// on a miss. Concurrent callers for the same key share a single load;
// callers for different keys never block each other. If the caller's ctx
// is done before the load finishes, it returns ctx.Err() while the load
// keeps running for the remaining callers.
func (c *Cache[K, V]) GetOrLoad(ctx context.Context, key K, load func(ctx context.Context) (V, error)) (V, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	if e := c.getLocked(key); e != nil {
		// A hit counts as a use.
		c.lru.remove(e.lruNode)
		c.lru.pushFront(e.lruNode)
		v := e.value
		c.mu.Unlock()
		return v, nil
	}
	call, joined := c.loads[key]
	if !joined {
		call = &loadCall[K, V]{done: make(chan struct{})}
		call.loadCtx, call.cancel = context.WithCancel(context.Background())
		c.loads[key] = call
	}
	call.waiters++
	c.mu.Unlock()

	if !joined {
		go c.runLoad(key, call, load)
	}
	return c.wait(ctx, call)
}

// loadCall is one in-flight load shared by all waiters of a key.
type loadCall[K comparable, V any] struct {
	// done is closed once load has returned, its result stored (if any)
	// and the call unregistered.
	done chan struct{}
	// mu (the cache's lock) guards waiters.
	waiters int
	// loadCtx is the context passed to load. It is cancelled when every
	// waiter has given up on this call.
	loadCtx context.Context
	cancel  context.CancelFunc
	// val and err are the load's result; they are written before done is
	// closed and read after, so the channel close orders them.
	val V
	err error
}

// runLoad calls load once, stores its result (a late success is stored
// even if every caller gave up; an error stores nothing) and unregisters
// the call. It is the only goroutine the cache starts per load and it ends
// when the load ends.
func (c *Cache[K, V]) runLoad(key K, call *loadCall[K, V], load func(context.Context) (V, error)) {
	v, err := load(call.loadCtx)
	c.mu.Lock()
	if err == nil {
		c.storeLocked(key, v)
	}
	// A failed load is not remembered: a later GetOrLoad calls load again.
	delete(c.loads, key)
	c.mu.Unlock()
	call.val, call.err = v, err
	close(call.done)
}

// wait blocks until the shared load finishes or the caller's ctx is done.
// Every exit path unregisters the caller; when the last waiter of a
// running load leaves, that load's context is cancelled.
func (c *Cache[K, V]) wait(ctx context.Context, call *loadCall[K, V]) (V, error) {
	select {
	case <-call.done:
		c.leave(call)
		return call.val, call.err
	case <-ctx.Done():
		// The result may have landed in the same instant; prefer it.
		select {
		case <-call.done:
			c.leave(call)
			return call.val, call.err
		default:
		}
		c.leave(call)
		var zero V
		return zero, ctx.Err()
	}
}

// leave unregisters one waiter, cancelling the load's context if it was
// the last one.
func (c *Cache[K, V]) leave(call *loadCall[K, V]) {
	c.mu.Lock()
	call.waiters--
	if call.waiters == 0 {
		call.cancel()
	}
	c.mu.Unlock()
}

// getLocked returns the live entry for key, purging it if it expired.
// c.mu must be held.
func (c *Cache[K, V]) getLocked(key K) *entry[K, V] {
	e, ok := c.items[key]
	if !ok {
		return nil
	}
	if e.expired(c.opts.now()) {
		c.removeLocked(key, e)
		return nil
	}
	return e
}

// expired reports whether the entry is no longer live at now.
func (e *entry[K, V]) expired(now time.Time) bool {
	return !e.expiresAt.IsZero() && !now.Before(e.expiresAt)
}

// removeLocked removes key's entry from the map and the LRU list.
// c.mu must be held.
func (c *Cache[K, V]) removeLocked(key K, e *entry[K, V]) {
	c.lru.remove(e.lruNode)
	delete(c.items, key)
	e.lruNode = nil
}

// storeLocked inserts or replaces key, restarts its TTL and marks it most
// recently used, evicting LRU live entries if that would exceed
// MaxEntries. c.mu must be held.
func (c *Cache[K, V]) storeLocked(key K, value V) {
	expiresAt := c.opts.expiresAt(c.opts.now())
	if e, ok := c.items[key]; ok {
		e.value = value
		e.expiresAt = expiresAt
		c.lru.remove(e.lruNode)
		c.lru.pushFront(e.lruNode)
	} else {
		e := &entry[K, V]{value: value, expiresAt: expiresAt}
		e.lruNode = &node[K]{key: key}
		c.items[key] = e
		c.lru.pushFront(e.lruNode)
	}
	c.evictLRULocked(key)
}

// evictLRULocked evicts least-recently-used live entries until at most
// Options.MaxEntries live entries remain. protect (the key just stored) is
// never a victim; it is the most recent use by construction. c.mu must be
// held.
func (c *Cache[K, V]) evictLRULocked(protect K) {
	max := c.opts.MaxEntries
	if max <= 0 {
		return
	}
	// liveCountLocked also purges expired entries, so no expired entry can
	// be picked as a victim below.
	for c.liveCountLocked() > max {
		n := c.lru.tail
		for n != nil && n.key == protect {
			n = n.prev
		}
		if n == nil {
			return // defensive: nothing evictable
		}
		c.removeLocked(n.key, c.items[n.key])
	}
}

// liveCountLocked returns the number of live entries, purging expired
// entries it finds on the way. c.mu must be held.
func (c *Cache[K, V]) liveCountLocked() int {
	now := c.opts.now()
	live := 0
	n := c.lru.head
	for n != nil {
		next := n.next
		if e := c.items[n.key]; e.expired(now) {
			c.removeLocked(n.key, e)
		} else {
			live++
		}
		n = next
	}
	return live
}
