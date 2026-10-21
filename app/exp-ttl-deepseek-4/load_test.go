package ttlcache

// Tests for the read-through path in load.go (the issue's rules 4-11).
//
// No test sequences a correctness-critical step with time.Sleep. Loaders are
// controlled by channels: a loader signals that it has started and then blocks
// on a release channel, so a test can pin a load in flight for as long as it
// needs. Where a test has to wait for another goroutine to reach a state it
// spins with runtime.Gosched (bounded) or waits on a channel handshake. A short
// time.After guard is used only to fail the test rather than hang it, never as
// the mechanism that makes a step happen. Where time matters, the test uses a
// scripted clock passed as Options.Now.

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"
)

// guard is the timeout for a channel wait that should complete "immediately".
// It only ever turns a deadlock into a test failure; no behaviour depends on
// its duration.
const guard = 5 * time.Second

// waitFor waits on ch and fails the test if nothing arrives within guard.
func waitFor[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(guard):
		t.Fatalf("timed out after %v waiting for %s", guard, what)
		var zero T
		return zero
	}
}

// waitClosed waits for ch to be closed (or to deliver a value) or fails.
func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(guard):
		t.Fatalf("timed out after %v waiting for %s", guard, what)
	}
}

// mustNotClosed fails the test if ch is already closed.
func mustNotClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatalf("%s happened unexpectedly", what)
	default:
	}
}

// newGate returns a channel that is closed to release the loader and a func
// that closes it exactly once.
func newGate() (chan struct{}, func()) {
	release := make(chan struct{})
	var once sync.Once
	return release, func() { once.Do(func() { close(release) }) }
}

// gatedLoader returns a load function that reports every call on calls, closes
// started once (the first call only) and then blocks until release is closed.
// Every call returns value, err once released.
type gatedLoader[V any] struct {
	release  chan struct{}
	open     func()
	started  chan struct{}
	startOne sync.Once
	calls    chan struct{} // one token per call
	value    V
	err      error
}

func newGatedLoader[V any](value V, err error) *gatedLoader[V] {
	release, open := newGate()
	return &gatedLoader[V]{
		release: release,
		open:    open,
		started: make(chan struct{}),
		calls:   make(chan struct{}, 1024),
		value:   value,
		err:     err,
	}
}

func (g *gatedLoader[V]) load(ctx context.Context) (V, error) {
	g.calls <- struct{}{}
	g.startOne.Do(func() { close(g.started) })
	<-g.release
	return g.value, g.err
}

// TestGetOrLoadLiveHitDoesNotLoad covers rule 4: GetOrLoad on a live hit
// returns the cached value without calling load, and counts as a use.
func TestGetOrLoadLiveHitDoesNotLoad(t *testing.T) {
	clk := newScriptedClock()
	ttl := time.Minute
	c := New[string, int](Options{TTL: ttl, Now: clk.Now})
	c.Set("k", 41)

	load := func(context.Context) (int, error) {
		t.Fatal("load was called on a live hit")
		return 0, nil
	}
	got, err := c.GetOrLoad(context.Background(), "k", load)
	if err != nil {
		t.Fatalf("GetOrLoad() err = %v; want nil", err)
	}
	if got != 41 {
		t.Fatalf("GetOrLoad() = %d; want 41", got)
	}
	if n := c.Len(); n != 1 {
		t.Fatalf("Len() = %d; want 1", n)
	}
}

// TestGetOrLoadMissStores covers rule 9's store path on the ordinary (waited
// for) case: the value is returned, cached, and its TTL starts when Now() reads
// after load returned.
func TestGetOrLoadMissStores(t *testing.T) {
	clk := newScriptedClock()
	ttl := time.Minute
	c := New[string, int](Options{TTL: ttl, Now: clk.Now})

	var loaded bool
	load := func(context.Context) (int, error) {
		loaded = true
		// Time passes inside load: the TTL must start after it returns, not
		// when the caller entered GetOrLoad.
		clk.Advance(30 * time.Second)
		return 7, nil
	}
	got, err := c.GetOrLoad(context.Background(), "k", load)
	if err != nil {
		t.Fatalf("GetOrLoad() err = %v; want nil", err)
	}
	if !loaded {
		t.Fatal("load was not called on a miss")
	}
	if got != 7 {
		t.Fatalf("GetOrLoad() = %d; want 7", got)
	}
	if v, ok := c.Get("k"); !ok || v != 7 {
		t.Fatalf("Get(%q) = %v, %v; want 7, true", "k", v, ok)
	}
	// TTL runs from the post-load Now() (epoch+30s), so the entry is live just
	// before epoch+90s and expired at it.
	clk.Advance(ttl - time.Second)
	mustGet(t, c, "k")
	clk.Advance(time.Second)
	mustMiss(t, c, "k")
}

// TestConcurrentSameKeyOneLoad covers rule 5's common case: 50 concurrent
// callers for one key share exactly one load and all get its value.
func TestConcurrentSameKeyOneLoad(t *testing.T) {
	c := New[string, int](Options{})
	g := newGatedLoader(99, nil)

	const n = 50
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, n)
	vals := make([]int, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release all callers at once
			vals[i], errs[i] = c.GetOrLoad(context.Background(), "k", g.load)
		}()
	}
	close(start)
	// Wait for the (single) load to be in flight before letting it finish, so
	// the callers really do pile up on one call rather than racing serially.
	waitClosed(t, g.started, "the load to start")
	// Let every caller join the call: with the load blocked, a caller that has
	// not joined yet is still fine, but we want them registered as waiters.
	for range 1000 {
		c.mu.Lock()
		waiters := c.calls["k"].waiters
		c.mu.Unlock()
		if waiters == n {
			break
		}
		runtime.Gosched()
	}
	g.open() // release the load

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	waitClosed(t, done, "all 50 callers to return")

	if got := len(g.calls); got != 1 {
		t.Fatalf("load was called %d times; want exactly 1", got)
	}
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("caller %d err = %v; want nil", i, errs[i])
		}
		if vals[i] != 99 {
			t.Fatalf("caller %d value = %d; want 99", i, vals[i])
		}
	}
	if v, ok := c.Get("k"); !ok || v != 99 {
		t.Fatalf("Get(%q) = %v, %v; want 99, true", "k", v, ok)
	}
}

// TestConcurrentDifferentKeysLoadIndependently covers rule 5's second half:
// a blocked load for one key does not hold up another key.
func TestConcurrentDifferentKeysLoadIndependently(t *testing.T) {
	c := New[string, string](Options{})
	blocked := newGatedLoader("A", nil)

	// Key "A": get its load into flight and leave it blocked on the gate.
	aOut := make(chan string, 1)
	aErr := make(chan error, 1)
	go func() {
		v, err := c.GetOrLoad(context.Background(), "A", blocked.load)
		aOut <- v
		aErr <- err
	}()
	waitClosed(t, blocked.started, "key A's load to start")

	// Key "B" uses a loader that returns immediately. This call runs on the
	// test goroutine while A's load is still blocked, so it can only return
	// here if B neither waits on A's call nor on any lock A holds.
	b := make(chan struct{})
	got, err := c.GetOrLoad(context.Background(), "B", func(context.Context) (string, error) {
		close(b)
		return "B", nil
	})
	if err != nil {
		t.Fatalf("GetOrLoad(B) err = %v; want nil", err)
	}
	if got != "B" {
		t.Fatalf("GetOrLoad(B) = %q; want %q", got, "B")
	}
	waitClosed(t, b, "key B's load to run")
	if v, ok := c.Get("B"); !ok || v != "B" {
		t.Fatalf("Get(B) = %v, %v; want B, true", v, ok)
	}
	// A is still in flight and unset, proving B did not wait for it.
	if v, ok := c.Get("A"); ok {
		t.Fatalf("Get(A) = %v, true while A's load is still blocked", v)
	}
	mustNotClosed(t, blocked.release, "key A's load was released early")

	blocked.open()
	if got := waitFor(t, aOut, "key A's caller to return"); got != "A" {
		t.Fatalf("GetOrLoad(A) = %q; want %q", got, "A")
	}
	if err := waitFor(t, aErr, "key A's error"); err != nil {
		t.Fatalf("GetOrLoad(A) err = %v; want nil", err)
	}
	if v, ok := c.Get("A"); !ok || v != "A" {
		t.Fatalf("Get(A) = %v, %v; want A, true", v, ok)
	}
}

// TestLoadErrorNotCached covers rule 6: every waiter sees the same error value,
// nothing is cached, and a later call loads again.
func TestLoadErrorNotCached(t *testing.T) {
	c := New[string, int](Options{})
	sentinel := errors.New("boom")
	g := newGatedLoader(0, sentinel)

	const n = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = c.GetOrLoad(context.Background(), "k", g.load)
		}()
	}
	close(start)
	waitClosed(t, g.started, "the failing load to start")
	for range 1000 {
		c.mu.Lock()
		waiters := c.calls["k"].waiters
		c.mu.Unlock()
		if waiters == n {
			break
		}
		runtime.Gosched()
	}
	g.open()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	waitClosed(t, done, "all waiters to return")

	for i := range n {
		if errs[i] != sentinel {
			t.Fatalf("waiter %d err = %v; want the load's own error value", i, errs[i])
		}
		if !errors.Is(errs[i], sentinel) {
			t.Fatalf("waiter %d err = %v; want errors.Is(err, sentinel)", i, errs[i])
		}
	}
	if v, ok := c.Get("k"); ok {
		t.Fatalf("Get(%q) = %v, true after a failed load; want a miss", "k", v)
	}
	if n := c.Len(); n != 0 {
		t.Fatalf("Len() = %d after a failed load; want 0", n)
	}
	// The failed call left the in-flight table: a later GetOrLoad loads again.
	c.mu.Lock()
	_, stillThere := c.calls["k"]
	c.mu.Unlock()
	if stillThere {
		t.Fatal("the failed call is still in the in-flight table")
	}
	got, err := c.GetOrLoad(context.Background(), "k", func(context.Context) (int, error) {
		return 5, nil
	})
	if err != nil {
		t.Fatalf("second GetOrLoad err = %v; want nil", err)
	}
	if got != 5 {
		t.Fatalf("second GetOrLoad = %d; want 5", got)
	}
}

// TestCallerGivesUpPromptly covers rule 7: a caller whose ctx is done returns
// ctx.Err() while the load keeps running and still caches its value for the
// caller that stayed.
func TestCallerGivesUpPromptly(t *testing.T) {
	c := New[string, int](Options{})
	g := newGatedLoader(3, nil)

	// Waiter 1: a ctx that is already done.
	deadCtx, cancelDead := context.WithCancel(context.Background())
	cancelDead()
	gone := make(chan struct{})
	var goneErr error
	go func() {
		_, goneErr = c.GetOrLoad(deadCtx, "k", g.load)
		close(gone)
	}()

	// Waiter 2: a live ctx that will see the load through.
	stayed := make(chan int, 1)
	stayedErr := make(chan error, 1)
	go func() {
		v, err := c.GetOrLoad(context.Background(), "k", g.load)
		stayed <- v
		stayedErr <- err
	}()

	waitClosed(t, gone, "the cancelled caller to return promptly")
	if !errors.Is(goneErr, context.Canceled) {
		t.Fatalf("cancelled caller err = %v; want context.Canceled", goneErr)
	}
	// Waiter 1 returning must not have stopped the load for waiter 2: it is
	// still blocked and the key is still unset.
	select {
	case v := <-stayed:
		t.Fatalf("waiter 2 returned %d while the load was still blocked", v)
	default:
	}
	if v, ok := c.Get("k"); ok {
		t.Fatalf("Get(%q) = %v, true before the load returned; want a miss", "k", v)
	}
	waitClosed(t, g.started, "the load to start")

	g.open()
	if got := waitFor(t, stayed, "waiter 2 to get the value"); got != 3 {
		t.Fatalf("waiter 2 value = %d; want 3", got)
	}
	if err := waitFor(t, stayedErr, "waiter 2's error"); err != nil {
		t.Fatalf("waiter 2 err = %v; want nil", err)
	}
	if v, ok := c.Get("k"); !ok || v != 3 {
		t.Fatalf("Get(%q) = %v, %v after the load; want 3, true", "k", v, ok)
	}
}

// TestLoadContextCancelledOnlyWhenLastWaiterGivesUp covers rule 8: the context
// handed to load is cancelled exactly when the last waiter gives up.
func TestLoadContextCancelledOnlyWhenLastWaiterGivesUp(t *testing.T) {
	c := New[string, int](Options{})

	gotCtx := make(chan context.Context, 1)
	g := newGatedLoader(1, nil)
	load := func(ctx context.Context) (int, error) {
		gotCtx <- ctx
		return g.load(ctx)
	}

	ctx1, cancel1 := context.WithCancel(context.Background())
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()

	waiters := make(chan struct{}, 2)
	var wg sync.WaitGroup
	run := func(ctx context.Context) {
		defer wg.Done()
		c.GetOrLoad(ctx, "k", load)
		waiters <- struct{}{}
	}
	wg.Add(2)
	go run(ctx1)
	go run(ctx2)

	loadCtx := waitFor(t, gotCtx, "the load's context")
	if err := loadCtx.Err(); err != nil {
		t.Fatalf("load ctx was already done (%v) before anyone gave up", err)
	}
	waitClosed(t, g.started, "the load to start")

	// Cancel the first caller only: its load ctx must stay alive for the other.
	cancel1()
	waitFor(t, waiters, "caller 1 to give up")
	select {
	case <-loadCtx.Done():
		t.Fatalf("load ctx was cancelled after one caller gave up: %v", loadCtx.Err())
	default:
	}

	// The last waiter gives up: now, and only now, the load ctx is cancelled.
	cancel2()
	waitFor(t, waiters, "caller 2 to give up")
	waitClosed(t, loadCtx.Done(), "the load ctx to be cancelled when the last waiter gave up")

	g.open()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	waitClosed(t, done, "both callers to return")
}

// TestLateResultStoredAfterEveryCallerGaveUp covers rule 9: a load that
// succeeds after every caller gave up is still stored, with its TTL starting at
// the post-load Now().
func TestLateResultStoredAfterEveryCallerGaveUp(t *testing.T) {
	clk := newScriptedClock()
	ttl := time.Minute
	c := New[string, int](Options{TTL: ttl, Now: clk.Now})
	g := newGatedLoader(11, nil)

	ctx, cancel := context.WithCancel(context.Background())
	gone := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(ctx, "k", g.load)
		gone <- err
	}()
	waitClosed(t, g.started, "the load to start")
	cancel()
	if err := waitFor(t, gone, "the caller to give up"); !errors.Is(err, context.Canceled) {
		t.Fatalf("caller err = %v; want context.Canceled", err)
	}
	// Nobody is waiting; the load keeps running to completion anyway.
	if v, ok := c.Get("k"); ok {
		t.Fatalf("Get(%q) = %v, true while the load is blocked", "k", v)
	}

	clk.Advance(10 * time.Second) // time passes before the load returns
	g.open()                      // the loader returns a value to nobody

	// The value must appear without anyone asking again.
	for range 100000 {
		if v, ok := c.Get("k"); ok {
			if v != 11 {
				t.Fatalf("stored value = %d; want 11", v)
			}
			break
		}
		runtime.Gosched()
	}
	if _, ok := c.Get("k"); !ok {
		t.Fatal("a late successful load was not stored")
	}
	// TTL runs from the post-load Now() (epoch+10s), not from the caller's
	// start: live just before epoch+70s, expired at it.
	clk.Set(epoch.Add(10*time.Second + ttl - time.Second))
	mustGet(t, c, "k")
	clk.Advance(time.Second)
	mustMiss(t, c, "k")
}

// TestDeleteDoesNotStopInFlightLoad covers rule 10: Delete removes the key but
// the in-flight load's result is still stored when it completes.
func TestDeleteDoesNotStopInFlightLoad(t *testing.T) {
	c := New[string, int](Options{})
	g := newGatedLoader(21, nil)

	done := make(chan int, 1)
	go func() {
		v, _ := c.GetOrLoad(context.Background(), "k", g.load)
		done <- v
	}()
	waitClosed(t, g.started, "the load to start")

	c.Delete("k")
	if v, ok := c.Get("k"); ok {
		t.Fatalf("Get(%q) = %v, true after Delete; want a miss", "k", v)
	}

	g.open()
	if got := waitFor(t, done, "the loader's caller to return"); got != 21 {
		t.Fatalf("GetOrLoad = %d; want 21", got)
	}
	if v, ok := c.Get("k"); !ok || v != 21 {
		t.Fatalf("Get(%q) = %v, %v after the load finished post-Delete; want 21, true", "k", v, ok)
	}
}

// TestGetOrLoadCountsAsAnLRUUse covers rule 3's "used" clause for GetOrLoad:
// both a completed load and a hit count, so the entry evicted next is the one
// nothing touched.
func TestGetOrLoadCountsAsAnLRUUse(t *testing.T) {
	c := New[string, int](Options{MaxEntries: 2})
	load := func(v int) func(context.Context) (int, error) {
		return func(context.Context) (int, error) { return v, nil }
	}

	if got := mustLoad(t, c, "A", load(1)); got != 1 {
		t.Fatalf("GetOrLoad(A) = %d; want 1", got)
	}
	if got := mustLoad(t, c, "B", load(2)); got != 2 {
		t.Fatalf("GetOrLoad(B) = %d; want 2", got)
	}
	// A hit through GetOrLoad marks A as used, so B is now the least recently
	// used and must be the one evicted by the new key C.
	if got := mustLoad(t, c, "A", func(context.Context) (int, error) {
		t.Fatal("load called on a live hit through GetOrLoad")
		return 0, nil
	}); got != 1 {
		t.Fatalf("GetOrLoad(A) hit = %d; want 1", got)
	}
	if got := mustLoad(t, c, "C", load(3)); got != 3 {
		t.Fatalf("GetOrLoad(C) = %d; want 3", got)
	}
	if v, ok := c.Get("B"); ok {
		t.Fatalf("Get(B) = %v, true; want B evicted as least recently used", v)
	}
	mustGet(t, c, "A")
	mustGet(t, c, "C")
	if n := c.Len(); n != 2 {
		t.Fatalf("Len() = %d; want 2", n)
	}
}

// mustLoad asserts a successful GetOrLoad and returns the value.
func mustLoad(t *testing.T, c *Cache[string, int], key string, load func(context.Context) (int, error)) int {
	t.Helper()
	v, err := c.GetOrLoad(context.Background(), key, load)
	if err != nil {
		t.Fatalf("GetOrLoad(%q) err = %v; want nil", key, err)
	}
	return v
}

// TestNoGoroutineOutlivesItsLoad covers rule 11's leak clause: the goroutine a
// miss starts must be gone once the load has returned and its result is stored,
// including when every caller gave up first (and so never received the value).
// runtime.NumGoroutine is sampled with a bounded, Gosched-based spin rather than
// a sleep; the guard only bounds the spin.
func TestNoGoroutineOutlivesItsLoad(t *testing.T) {
	// Warm up so the runtime's own bookkeeping goroutines are not mistaken for
	// leaks from this test.
	warm := New[string, int](Options{})
	for i := range 10 {
		key := string(rune('a' + i))
		if _, err := warm.GetOrLoad(context.Background(), key, func(context.Context) (int, error) {
			return i, nil
		}); err != nil {
			t.Fatalf("warm-up GetOrLoad err = %v", err)
		}
	}
	waitGoroutines := func(want int, what string) {
		t.Helper()
		deadline := time.Now().Add(guard)
		best := runtime.NumGoroutine()
		for {
			best = runtime.NumGoroutine()
			if best <= want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("goroutine count stayed at %d (want <= %d) waiting for %s", best, want, what)
			}
			runtime.Gosched()
		}
	}
	waitGoroutines(runtime.NumGoroutine(), "the baseline to settle")
	before := runtime.NumGoroutine()

	c := New[string, int](Options{})
	for i := range 25 {
		key := string(rune('A' + i))
		g := newGatedLoader(i, nil)

		// Every caller gives up before the load returns, so no caller is left
		// to observe the result: the load goroutine must still exit and store.
		ctx, cancel := context.WithCancel(context.Background())
		gone := make(chan struct{})
		go func() {
			c.GetOrLoad(ctx, key, g.load)
			close(gone)
		}()
		waitClosed(t, g.started, "the load to start")
		cancel()
		waitClosed(t, gone, "the caller to give up")
		g.open()

		// The value lands and the load goroutine is gone: wait for the store,
		// which the load goroutine performs just before it exits.
		waitGoroutines(before, "load "+key+"'s goroutine to exit")
		if v, ok := c.Get(key); !ok || v != i {
			t.Fatalf("late result for %q = %v, %v; want %d, true", key, v, ok, i)
		}
	}
	waitGoroutines(before, "all load goroutines to exit")

	// And a plain successful load leaves nothing behind either.
	for i := range 25 {
		key := "s" + string(rune('A'+i))
		if _, err := c.GetOrLoad(context.Background(), key, func(context.Context) (int, error) {
			return i, nil
		}); err != nil {
			t.Fatalf("GetOrLoad err = %v", err)
		}
	}
	waitGoroutines(before, "all successful load goroutines to exit")
}
