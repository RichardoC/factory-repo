package ttlcache

import (
	"context"
	"sync"
)

// flight[K, V] is one load in progress for one key.
//
// At most one flight exists per key at a time (rule 5): the cache creates it
// under c.mu on the first miss and removes it when the load returns. The load
// runs on the goroutine of the caller that started the flight, so the cache
// itself starts no goroutine (rule 11).
//
// val and err are written once by runLoad and read only after done is closed,
// so the channel close is the happens-before edge and neither needs a lock.
// waiters is read and written by callers that give up and by nobody holding
// c.mu at the same time, so it carries its own mutex.
type flight[K comparable, V any] struct {
	key    K
	ctx    context.Context
	cancel context.CancelFunc

	done chan struct{} // closed by runLoad after val and err are set
	val  V
	err  error

	mu      sync.Mutex
	waiters int // callers still waiting on this load
}

// addWaiter records one more caller waiting on the load.
func (f *flight[K, V]) addWaiter() {
	f.mu.Lock()
	f.waiters++
	f.mu.Unlock()
}

// removeWaiter records that one caller has stopped waiting and reports whether
// it was the last one. c.mu must be held.
func (f *flight[K, V]) removeWaiter() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.waiters--
	return f.waiters == 0
}

// runLoad performs the load for f and publishes its result. It runs on the
// goroutine of the caller that started the flight, and no other goroutine
// waits on it beyond the callers registered as waiters.
//
// The load is handed the flight's context, which is cancelled only when the
// last waiter gives up (rules 7 and 8) — never because the caller that
// happened to start the flight gave up first.
func (c *Cache[K, V]) runLoad(f *flight[K, V], key K, load func(ctx context.Context) (V, error)) {
	// f.ctx and f.cancel were built under c.mu, so a waiter that gives up
	// before this goroutine is scheduled can cancel them safely. Cancelling
	// here as well releases the context after a load that succeeded; it is
	// idempotent.
	val, err := load(f.ctx)
	f.cancel()

	if err == nil {
		// Rules 9 and 10: a successful result is stored with its TTL
		// starting when load returned, even if every caller has given up,
		// and even if the key was deleted while the load ran.
		c.mu.Lock()
		c.store(key, val, c.now())
		c.mu.Unlock()
	}
	// Rule 6: on error nothing is cached.

	c.mu.Lock()
	// Only clear the flight if it is still ours: a Delete does not remove
	// it, but guarding here keeps a future replacement honest.
	if c.inflight[key] == f {
		delete(c.inflight, key)
	}
	f.val, f.err = val, err
	c.mu.Unlock()
	close(f.done)
}
