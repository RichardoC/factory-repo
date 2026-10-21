package ttlcache

// load.go owns the read-through path: GetOrLoad, the single-flight call[V]
// record, the waiter bookkeeping and the one goroutine that runs a load.
// cache.go owns the entry/LRU engine and declares the frozen Cache fields
// (mu, now, entries and the calls table) that this file uses.

import "context"

// call is one in-flight load for one key. Every field is read or written under
// c.mu except done, which is only ever closed (never sent on) after the result
// is decided and published.
type call[V any] struct {
	done     chan struct{} // closed exactly once, when the result is decided
	value    V             // valid only after done is closed
	err      error         // valid only after done is closed
	waiters  int           // callers still waiting (c.mu-held count)
	cancel   context.CancelFunc
	finished bool // load returned; its result has been stored/decided
}

// GetOrLoad returns the live value stored for key. On a hit it is exactly Get:
// load is not called and the hit counts as a use for LRU purposes.
//
// On a miss the first caller creates a call and starts exactly one goroutine
// running load; concurrent callers for the same key join that call and wait on
// its done channel, so at most one load per key is in flight. load runs with no
// lock held, so keys never block each other.
//
// ctx only governs the caller: if it is done before the load finishes, this
// caller returns ctx.Err() promptly and the load keeps running for the others.
// The context handed to load is derived from context.Background() and is
// cancelled when the last waiter gives up.
func (c *Cache[K, V]) GetOrLoad(ctx context.Context, key K, load func(ctx context.Context) (V, error)) (V, error) {
	c.mu.Lock()
	now := c.now()
	if e, ok := c.entries[key]; ok && c.liveLocked(e, now) {
		// Live hit: never call load, and count the hit as a use.
		c.touchLocked(key)
		value := e.value
		c.mu.Unlock()
		return value, nil
	}
	state := ctx.Err()
	if state != nil {
		// The caller is already done. Miss or not, report that at once and
		// start no load on its behalf.
		c.mu.Unlock()
		var zero V
		return zero, state
	}
	cl := c.loadLocked(key, load)
	c.mu.Unlock()

	select {
	case <-cl.done:
		// Wait for the goroutine to publish, then read the result. Both are
		// safe: it writes value/err before close(done), and close is a happens
		// before edge for this receive.
		c.mu.Lock()
		if c.calls[key] == cl {
			c.waiterGoneLocked(cl)
		}
		value, err := cl.value, cl.err
		c.mu.Unlock()
		return value, err
	case <-ctx.Done():
		// Giving up: drop this caller's waiter slot. The load itself is not
		// stopped, and the last waiter to leave cancels the load's context.
		c.mu.Lock()
		if c.calls[key] == cl {
			c.waiterGoneLocked(cl)
		}
		c.mu.Unlock()
		var zero V
		return zero, ctx.Err()
	}
}

// loadLocked returns the in-flight call for key, creating it and starting its
// load goroutine on a miss. c.mu must be held.
func (c *Cache[K, V]) loadLocked(key K, load func(context.Context) (V, error)) *call[V] {
	if cl, ok := c.calls[key]; ok {
		cl.waiters++
		return cl
	}
	// The call's context is built and published by runLoad (it is derived from
	// context.Background(), never from a caller's ctx) so that a waiter which
	// gives up before the goroutine is scheduled still finds its cancel.
	cl := &call[V]{done: make(chan struct{}), waiters: 1}
	c.calls[key] = cl
	go c.runLoad(key, load, cl)
	return cl
}

// waiterGoneLocked records that one waiter has stopped waiting, either because
// the load finished (the returning path) or because it gave up on its own ctx.
// When the last waiter leaves, the load's context is cancelled. c.mu must be
// held.
func (c *Cache[K, V]) waiterGoneLocked(cl *call[V]) {
	if cl.waiters == 0 {
		return
	}
	cl.waiters--
	if cl.waiters == 0 && cl.cancel != nil {
		cl.cancel()
	}
}

// runLoad is the single goroutine a miss starts. It runs load with the call's
// own context, then publishes the result: on success the value is stored with a
// TTL starting after load returned, then done is closed to release every
// waiter. Closing done is the last thing that happens to the call, so no waiter
// can observe a half-published result, and this goroutine outlives nothing it
// serves: it exits as soon as the result is decided and stored.
func (c *Cache[K, V]) runLoad(key K, load func(context.Context) (V, error), cl *call[V]) {
	// The load context is derived from context.Background(), never from a
	// caller's ctx, so no single caller's cancellation can reach it. It is
	// cancelled by the waiter bookkeeping when the last waiter gives up, or
	// here once the result is decided. It is created here and published on the
	// call under c.mu before load starts, so a waiter that gives up first still
	// finds the cancel function.
	ctx, cancel := context.WithCancel(context.Background())
	c.mu.Lock()
	released := cl.waiters == 0 // every waiter gave up before we got here
	cl.cancel = cancel
	c.mu.Unlock()
	if released {
		// Nobody is left to keep the load alive.
		cancel()
	}

	value, err := load(ctx)

	// Success stores even if every caller has given up (a late result) and even
	// if the key was deleted while the load was in flight: the store path
	// deliberately does not ask whether anyone is still waiting or whether the
	// key is still present.
	now := c.now()
	c.mu.Lock()
	if err == nil {
		c.storeLocked(key, value, now)
	}
	if c.calls[key] == cl {
		delete(c.calls, key)
	}
	cl.value = value
	cl.err = err
	cl.finished = true
	// Everything else in this function is done: release the context and let
	// waiters read the result.
	cancel()
	c.mu.Unlock()
	close(cl.done)
}
