// Package ttlcache implements an in-memory cache with per-entry TTL,
// LRU eviction, and single-flight GetOrLoad.
package ttlcache

import (
	"container/heap"
	"context"
	"sync"
	"time"
)

// Options configures a Cache.
type Options struct {
	// TTL > 0: an entry expires TTL after it was stored.
	// TTL == 0: entries never expire.
	TTL time.Duration

	// MaxEntries > 0: at most this many live entries; the least recently
	// used live entry is evicted when the limit would be exceeded.
	// MaxEntries == 0: unbounded.
	MaxEntries int

	// Now is the clock the cache reads; nil means time.Now.
	Now func() time.Time
}

type entry[K comparable, V any] struct {
	key     K
	value   V
	expires time.Time // zero: never expires
	seq     uint64    // last-use stamp; higher is more recent
	index   int       // position in the LRU heap
}

// lruHeap orders entries by last use, least recently used first.
type lruHeap[K comparable, V any] []*entry[K, V]

func (h lruHeap[K, V]) Len() int           { return len(h) }
func (h lruHeap[K, V]) Less(i, j int) bool { return h[i].seq < h[j].seq }
func (h lruHeap[K, V]) Swap(i, j int)      { h[i], h[j] = h[j], h[i]; h[i].index, h[j].index = i, j }
func (h *lruHeap[K, V]) Push(x any)        { e := x.(*entry[K, V]); e.index = len(*h); *h = append(*h, e) }
func (h *lruHeap[K, V]) Pop() any {
	old := *h
	n := len(old)
	e := old[n-1]
	old[n-1] = nil
	e.index = -1
	*h = old[:n-1]
	return e
}

// call tracks one in-flight load and the callers waiting on it.
type call[K comparable, V any] struct {
	done    chan struct{} // closed when load has returned and the result is recorded
	cancel  context.CancelFunc
	waiters int // callers still waiting; guarded by Cache.mu
	val     V
	err     error
}

// Cache is an in-memory TTL + LRU cache. It is safe for concurrent use.
type Cache[K comparable, V any] struct {
	mu      sync.Mutex
	now     func() time.Time
	ttl     time.Duration
	max     int
	seq     uint64
	entries map[K]*entry[K, V]
	lru     lruHeap[K, V]
	loads   map[K]*call[K, V]
}

// New returns an empty Cache configured by opts.
func New[K comparable, V any](opts Options) *Cache[K, V] {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	max := opts.MaxEntries
	if max < 0 {
		max = 0
	}
	return &Cache[K, V]{
		now:     now,
		ttl:     opts.TTL,
		max:     max,
		entries: make(map[K]*entry[K, V]),
		loads:   make(map[K]*call[K, V]),
	}
}

// Get returns the live value for key, if any. A hit refreshes the entry's
// LRU recency; an expired entry is removed and reported as a miss.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.getLocked(key)
}

// Set stores value under key, replacing any existing entry and restarting
// its TTL and recency. It evicts expired entries and, if MaxEntries is set
// and live entries would exceed it, the least recently used live entry.
func (c *Cache[K, V]) Set(key K, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setLocked(key, value)
}

// Delete removes key. It does not stop an in-flight load for key: that
// load's result is still stored when it completes.
func (c *Cache[K, V]) Delete(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[key]; ok {
		c.removeLocked(e)
	}
}

// Len returns the number of live entries, removing expired ones first.
func (c *Cache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.purgeLocked()
	return len(c.entries)
}

// GetOrLoad returns the live cached value for key, or on a miss calls load
// exactly once per in-flight miss and caches a successful result.
//
// Concurrent callers for the same key share one load. If load returns an
// error, nothing is cached and every waiter gets that same error. A caller
// whose ctx is done before the load finishes returns ctx.Err() promptly;
// the load keeps running for the remaining waiters, and the context passed
// to load is cancelled only once every waiter has given up. A successful
// result is stored even if every caller gave up before it arrived, with its
// TTL starting when load returned.
func (c *Cache[K, V]) GetOrLoad(ctx context.Context, key K, load func(ctx context.Context) (V, error)) (V, error) {
	c.mu.Lock()
	if v, ok := c.getLocked(key); ok {
		c.mu.Unlock()
		return v, nil
	}
	if err := ctx.Err(); err != nil {
		c.mu.Unlock()
		var zero V
		return zero, err
	}
	if call, ok := c.loads[key]; ok {
		call.waiters++
		c.mu.Unlock()
		return c.waitFor(ctx, call)
	}
	// The load's context carries the initiating caller's values but not its
	// cancellation: it is cancelled when the last waiter gives up.
	lctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	call := &call[K, V]{done: make(chan struct{}), cancel: cancel, waiters: 1}
	c.loads[key] = call
	c.mu.Unlock()

	go func() {
		v, err := load(lctx)
		c.mu.Lock()
		call.val, call.err = v, err
		delete(c.loads, key)
		if err == nil {
			// Late results are stored too; the TTL starts now (rule 9).
			c.setLocked(key, v)
		}
		c.mu.Unlock()
		close(call.done)
	}()
	return c.waitFor(ctx, call)
}

// waitFor waits for call to finish, or for ctx to be done first.
func (c *Cache[K, V]) waitFor(ctx context.Context, call *call[K, V]) (V, error) {
	select {
	case <-call.done:
		return call.val, call.err
	default:
	}
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
		if call.waiters == 0 {
			call.cancel()
		}
		c.mu.Unlock()
		var zero V
		return zero, ctx.Err()
	}
}

func (c *Cache[K, V]) getLocked(key K) (V, bool) {
	e, ok := c.entries[key]
	if !ok {
		var zero V
		return zero, false
	}
	if c.expired(e) {
		c.removeLocked(e)
		var zero V
		return zero, false
	}
	c.touchLocked(e)
	return e.value, true
}

func (c *Cache[K, V]) setLocked(key K, value V) {
	c.purgeLocked()
	if e, ok := c.entries[key]; ok {
		e.value = value
		e.expires = c.expiry()
		c.touchLocked(e)
		return
	}
	c.seq++
	e := &entry[K, V]{key: key, value: value, expires: c.expiry(), seq: c.seq}
	c.entries[key] = e
	heap.Push(&c.lru, e)
	for c.max > 0 && len(c.entries) > c.max {
		// purgeLocked removed every expired entry, so the LRU end is live.
		c.removeLocked(heap.Pop(&c.lru).(*entry[K, V]))
	}
}

// touchLocked marks e as the most recently used entry.
func (c *Cache[K, V]) touchLocked(e *entry[K, V]) {
	c.seq++
	e.seq = c.seq
	heap.Fix(&c.lru, e.index)
}

func (c *Cache[K, V]) removeLocked(e *entry[K, V]) {
	delete(c.entries, e.key)
	if e.index >= 0 && e.index < len(c.lru) {
		heap.Remove(&c.lru, e.index)
	}
}

// purgeLocked removes every expired entry, so len(c.entries) equals the
// number of live entries afterwards.
func (c *Cache[K, V]) purgeLocked() {
	for _, e := range c.entries {
		if c.expired(e) {
			c.removeLocked(e)
		}
	}
}

func (c *Cache[K, V]) expiry() time.Time {
	if c.ttl <= 0 {
		return time.Time{}
	}
	return c.now().Add(c.ttl)
}

func (c *Cache[K, V]) expired(e *entry[K, V]) bool {
	return !e.expires.IsZero() && !c.now().Before(e.expires)
}
