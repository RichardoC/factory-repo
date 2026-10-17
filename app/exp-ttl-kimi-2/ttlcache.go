// Package ttlcache provides a small in-memory key/value cache with
// per-entry TTL expiry, LRU eviction bounded by a maximum size, and
// deduplicated ("singleflight") concurrent loads via GetOrLoad.
//
// A Cache is safe for concurrent use. All expiry is evaluated lazily
// through the configurable Options.Now clock; the cache starts no
// background goroutines except one per in-flight load, and each such
// goroutine ends when its load returns.
package ttlcache

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// Options configures a Cache.
type Options struct {
	// TTL > 0: an entry expires TTL after it was stored; 0: entries never expire.
	TTL time.Duration
	// MaxEntries > 0: at most this many live entries; 0: unbounded.
	MaxEntries int
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

type entry struct {
	key     any
	value   any
	exp     time.Time // zero means never expires
	lruElem *list.Element
}

// flight is one in-flight load shared by every GetOrLoad caller
// waiting on the same key.
type flight struct {
	done    chan struct{} // closed when the load function has returned
	waiters int           // callers still waiting on this load
	cancel  context.CancelFunc
	val     any // any so flight is not generic; only read after done is closed
	err     error
}

// Cache is a concurrent-safe TTL/LRU cache.
type Cache[K comparable, V any] struct {
	mu       sync.Mutex
	ttl      time.Duration
	max      int
	now      func() time.Time
	entries  map[K]*entry
	lru      *list.List // front = most recently used, live entries only are counted
	inflight map[K]*flight
}

// New returns a Cache configured by opts.
func New[K comparable, V any](opts Options) *Cache[K, V] {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Cache[K, V]{
		ttl:      opts.TTL,
		max:      opts.MaxEntries,
		now:      now,
		entries:  make(map[K]*entry),
		lru:      list.New(),
		inflight: make(map[K]*flight),
	}
}

// Get returns the value for key and whether it was present and live.
// A hit marks the entry as recently used.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		var zero V
		return zero, false
	}
	if c.expired(e) {
		c.remove(e)
		var zero V
		return zero, false
	}
	c.use(e)
	return e.value.(V), true
}

// Set stores key/value, replacing any existing entry and restarting
// its TTL, and marks the entry as most recently used. If storing
// leaves more than MaxEntries live entries, the least recently used
// live entries are evicted. Expired entries are removed and never
// count against the bound.
func (c *Cache[K, V]) Set(key K, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.storeLocked(key, value)
}

// storeLocked inserts or replaces key/value with a fresh expiry and
// enforces the size bound. Callers hold c.mu.
func (c *Cache[K, V]) storeLocked(key K, value V) {
	if e, ok := c.entries[key]; ok {
		e.value = value
		e.exp = c.expiry()
		c.use(e)
	} else {
		e := &entry{key: key, value: value, exp: c.expiry()}
		e.lruElem = c.lru.PushFront(e)
		c.entries[key] = e
	}
	if c.max > 0 {
		c.purgeExpiredLocked()
		for c.lru.Len() > c.max {
			back := c.lru.Back()
			c.remove(back.Value.(*entry))
		}
	}
}

// Delete removes key from the cache. A load in flight for key is not
// stopped: its result is still stored when it completes.
func (c *Cache[K, V]) Delete(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[key]; ok {
		c.remove(e)
	}
}

// Len returns the number of live (not expired) entries.
func (c *Cache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.purgeExpiredLocked()
	return c.lru.Len()
}

// GetOrLoad returns the live cached value for key without calling
// load. On a miss, concurrent callers for the same key share a single
// call of load: at most one load per key runs at a time, while loads
// for different keys proceed independently.
//
// If load fails, nothing is cached and every waiter returns that
// error. If a caller's ctx is done before the load returns, that
// caller returns ctx.Err() and stops waiting; the load keeps running
// for the remaining waiters. The context passed to load is cancelled
// exactly when the last waiter has given up. A value returned by load
// is stored with its TTL starting at load's return, even if every
// caller has given up.
func (c *Cache[K, V]) GetOrLoad(ctx context.Context, key K, load func(ctx context.Context) (V, error)) (V, error) {
	c.mu.Lock()
	if e, ok := c.entries[key]; ok {
		if c.expired(e) {
			c.remove(e)
		} else {
			c.use(e)
			v := e.value.(V)
			c.mu.Unlock()
			return v, nil
		}
	}

	f, ok := c.inflight[key]
	if !ok {
		// The load's context is derived from Background, not from any
		// one caller's ctx: it is cancelled only when the last waiter
		// gives up (see release), never just because of one caller.
		loadCtx, cancel := context.WithCancel(context.Background())
		f = &flight{done: make(chan struct{}), cancel: cancel}
		c.inflight[key] = f
		go func() {
			v, err := load(loadCtx)
			c.mu.Lock()
			if err == nil {
				// Rule: a successful value is stored even if every
				// caller has given up; TTL starts at load's return.
				c.storeLocked(key, v)
			}
			delete(c.inflight, key)
			f.val, f.err = v, err
			c.mu.Unlock()
			close(f.done)
			// The load context is released once the load is over and
			// no one needs it cancelled any more.
			cancel()
		}()
	}
	f.waiters++
	c.mu.Unlock()

	select {
	case <-f.done:
		return c.deliver(f)
	case <-ctx.Done():
		// If the load finished at the same time our ctx fired,
		// deliver the real result rather than ctx.Err().
		select {
		case <-f.done:
			return c.deliver(f)
		default:
		}
		c.mu.Lock()
		c.releaseLocked(f)
		c.mu.Unlock()
		var zero V
		return zero, ctx.Err()
	}
}

// deliver reads the finished flight's outcome. done is closed after
// val/err are written, so this needs no lock.
func (c *Cache[K, V]) deliver(f *flight) (V, error) {
	if f.err != nil {
		var zero V
		return zero, f.err
	}
	return f.val.(V), nil
}

// releaseLocked drops this caller's interest in flight f for key.
// When the last waiter gives up before the load has returned, the
// load's context is cancelled. Callers hold c.mu.
func (c *Cache[K, V]) releaseLocked(f *flight) {
	f.waiters--
	if f.waiters == 0 {
		// Safe because done-not-closed callers are the only ones who
		// can still release; after the load returns, f is gone from
		// inflight and cancel()ing is a no-op.
		f.cancel()
	}
}

func (c *Cache[K, V]) expiry() time.Time {
	if c.ttl <= 0 {
		return time.Time{}
	}
	return c.now().Add(c.ttl)
}

// expired reports whether e is no longer live: stored at t, an entry
// is live while now is before t+TTL.
func (c *Cache[K, V]) expired(e *entry) bool {
	if e.exp.IsZero() {
		return false
	}
	return !c.now().Before(e.exp)
}

func (c *Cache[K, V]) use(e *entry) {
	c.lru.MoveToFront(e.lruElem)
}

// removeLocked deletes e from the cache. Callers hold c.mu.
func (c *Cache[K, V]) remove(e *entry) {
	c.lru.Remove(e.lruElem)
	delete(c.entries, e.key.(K))
}

// purgeExpiredLocked drops every expired entry, so expired entries
// never cause a live entry to be evicted. Callers hold c.mu.
func (c *Cache[K, V]) purgeExpiredLocked() {
	var dead []*entry
	for elem := c.lru.Back(); elem != nil; elem = elem.Prev() {
		e := elem.Value.(*entry)
		if c.expired(e) {
			dead = append(dead, e)
		}
	}
	for _, e := range dead {
		c.remove(e)
	}
}
