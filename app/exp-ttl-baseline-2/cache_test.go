package ttlcache

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a manually advanced clock, safe for concurrent use.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	f.mu.Unlock()
}

// waitersOf reports how many GetOrLoad callers are currently waiting on the
// in-flight load for key (0 if none is in flight).
func waitersOf(c *Cache[string, int], key string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	lc, ok := c.loads[key]
	if !ok {
		return 0
	}
	return lc.waiters
}

// waitForWaiters blocks until at least n callers have joined the in-flight
// load for key, so the test does not depend on scheduling delays.
func waitForWaiters(t *testing.T, c *Cache[string, int], key string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for waitersOf(c, key) < n {
		if time.Now().After(deadline) {
			t.Fatalf("only %d callers joined the load for %q, want %d", waitersOf(c, key), key, n)
		}
		time.Sleep(time.Millisecond)
	}
}

// Rule 1: expiry — live while Now() < stored + TTL; Get/GetOrLoad never
// return an expired entry; Len counts only live entries.
func TestExpiry(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{TTL: 5 * time.Minute, Now: clk.Now})

	c.Set("a", 1)
	if _, ok := c.Get("a"); !ok {
		t.Fatal("entry should be live right after Set")
	}
	if n := c.Len(); n != 1 {
		t.Fatalf("Len = %d, want 1", n)
	}

	clk.Advance(4*time.Minute + 59*time.Second)
	if _, ok := c.Get("a"); !ok {
		t.Fatal("entry should still be live just before TTL")
	}

	// Live while Now() < t+TTL: at exactly t+TTL it is expired.
	clk.Advance(time.Second)
	if _, ok := c.Get("a"); ok {
		t.Fatal("entry should be expired at t+TTL")
	}
	if n := c.Len(); n != 0 {
		t.Fatalf("Len = %d after expiry, want 0", n)
	}

	// GetOrLoad must not serve an expired entry either.
	calls := 0
	if _, err := c.GetOrLoad(context.Background(), "a", func(context.Context) (int, error) {
		calls++
		return 2, nil
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("GetOrLoad over expired entry called load %d times, want 1", calls)
	}
}

// Rule 1: TTL == 0 means entries never expire.
func TestNoTTLNeverExpires(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{Now: clk.Now})
	c.Set("a", 1)
	clk.Advance(100 * 365 * 24 * time.Hour)
	if v, ok := c.Get("a"); !ok || v != 1 {
		t.Fatalf("Get = (%v, %v), want (1, true) with no TTL", v, ok)
	}
	if n := c.Len(); n != 1 {
		t.Fatalf("Len = %d, want 1", n)
	}
}

// Rule 2: Set on an existing key replaces the value and restarts its TTL.
func TestSetReplacesAndRestartsTTL(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{TTL: 5 * time.Minute, Now: clk.Now})

	c.Set("a", 1)
	clk.Advance(4 * time.Minute)
	c.Set("a", 2) // restarts the TTL from here

	// The original deadline (t0+5m) has passed, but the entry was stored
	// again at t0+4m, so it lives until t0+9m.
	clk.Advance(2 * time.Minute)
	if v, ok := c.Get("a"); !ok || v != 2 {
		t.Fatalf("Get = (%v, %v), want (2, true)", v, ok)
	}
	clk.Advance(3*time.Minute + 1*time.Second)
	if _, ok := c.Get("a"); ok {
		t.Fatal("entry should expire 5m after the replacing Set")
	}
}

// Rule 3: LRU eviction of live entries when MaxEntries is exceeded.
func TestLRUEviction(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{MaxEntries: 2, Now: clk.Now})

	c.Set("a", 1)
	c.Set("b", 2)
	c.Set("c", 3) // evicts "a", the least recently used
	if _, ok := c.Get("a"); ok {
		t.Fatal("a should have been evicted")
	}
	if _, ok := c.Get("b"); !ok {
		t.Fatal("b should still be cached")
	}
	if _, ok := c.Get("c"); !ok {
		t.Fatal("c should still be cached")
	}
	if n := c.Len(); n != 2 {
		t.Fatalf("Len = %d, want 2", n)
	}

	// A Get marks the entry as used: "c" becomes LRU instead of "b".
	if _, ok := c.Get("b"); !ok {
		t.Fatal("b should still be cached")
	}
	c.Set("d", 4) // evicts "c"
	if _, ok := c.Get("c"); ok {
		t.Fatal("c should have been evicted after b was used")
	}
	if _, ok := c.Get("b"); !ok {
		t.Fatal("b should survive because it was used recently")
	}

	// A GetOrLoad hit also counts as a use.
	if _, err := c.GetOrLoad(context.Background(), "b", func(context.Context) (int, error) {
		t.Fatal("load must not run on a hit")
		return 0, nil
	}); err != nil {
		t.Fatal(err)
	}
	c.Set("e", 5) // evicts "d"
	if _, ok := c.Get("d"); ok {
		t.Fatal("d should have been evicted")
	}
	if _, ok := c.Get("b"); !ok {
		t.Fatal("b should survive a GetOrLoad hit")
	}
}

// Rule 3: an expired entry never causes a live entry to be evicted.
func TestExpiredEntriesDoNotEvictLiveOnes(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{TTL: 5 * time.Minute, MaxEntries: 2, Now: clk.Now})

	c.Set("a", 1) // stored at t0, expires t0+5m
	clk.Advance(4 * time.Minute)
	c.Set("b", 2)                            // stored at t0+4m, expires t0+9m
	clk.Advance(1*time.Minute + time.Second) // t0+5m+1s: "a" expired, "b" live

	c.Set("c", 3) // live entries: b, c -> 2 <= MaxEntries, b must survive
	if _, ok := c.Get("a"); ok {
		t.Fatal("a should be expired")
	}
	if _, ok := c.Get("b"); !ok {
		t.Fatal("b must not be evicted: only the expired a made room")
	}
	if _, ok := c.Get("c"); !ok {
		t.Fatal("c should still be cached")
	}
}

// Rule 4: a live hit returns the cached value without calling load.
func TestGetOrLoadHitDoesNotLoad(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{TTL: 5 * time.Minute, Now: clk.Now})
	c.Set("a", 7)

	var calls int
	v, err := c.GetOrLoad(context.Background(), "a", func(context.Context) (int, error) {
		calls++
		return 0, nil
	})
	if err != nil || v != 7 {
		t.Fatalf("GetOrLoad = (%v, %v), want (7, nil)", v, err)
	}
	if calls != 0 {
		t.Fatalf("load called %d times on a live hit, want 0", calls)
	}

	// After expiry the value is loaded again.
	clk.Advance(6 * time.Minute)
	v, err = c.GetOrLoad(context.Background(), "a", func(context.Context) (int, error) {
		calls++
		return 8, nil
	})
	if err != nil || v != 8 {
		t.Fatalf("GetOrLoad after expiry = (%v, %v), want (8, nil)", v, err)
	}
	if calls != 1 {
		t.Fatalf("load calls = %d, want 1", calls)
	}
}

// Rule 5: concurrent GetOrLoad callers for one key share a single load.
func TestSingleflightSameKey(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{Now: clk.Now})

	started := make(chan struct{})
	release := make(chan struct{})
	var calls int32
	load := func(context.Context) (int, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			close(started)
		}
		<-release
		return 42, nil
	}

	const n = 10
	results := make(chan int, n)
	for i := 0; i < n; i++ {
		go func() {
			v, err := c.GetOrLoad(context.Background(), "k", load)
			if err != nil {
				t.Error(err)
			}
			results <- v
		}()
	}

	<-started
	waitForWaiters(t, c, "k", n) // every caller has joined the one load
	close(release)

	for i := 0; i < n; i++ {
		if v := <-results; v != 42 {
			t.Fatalf("got %d, want 42", v)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("load ran %d times for one key, want 1", got)
	}
}

// Rule 5: different keys load independently, without blocking each other.
func TestDifferentKeysLoadIndependently(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{Now: clk.Now})

	slowStarted := make(chan struct{})
	slowRelease := make(chan struct{})
	slow := func(context.Context) (int, error) {
		close(slowStarted)
		<-slowRelease
		return 1, nil
	}
	doneA := make(chan struct{})
	go func() {
		defer close(doneA)
		if _, err := c.GetOrLoad(context.Background(), "a", slow); err != nil {
			t.Error(err)
		}
	}()

	<-slowStarted
	// "a" is stuck in its load; "b" must complete anyway.
	bDone := make(chan int)
	go func() {
		v, err := c.GetOrLoad(context.Background(), "b", func(context.Context) (int, error) {
			return 2, nil
		})
		if err != nil {
			t.Error(err)
		}
		bDone <- v
	}()
	select {
	case v := <-bDone:
		if v != 2 {
			t.Fatalf("b loaded %d, want 2", v)
		}
	case <-doneA: // "a" finished first: b was blocked — wrong
		t.Fatal("key b was blocked by key a's load")
	case <-time.After(2 * time.Second):
		t.Fatal("key b's load did not complete while a was loading")
	}
	close(slowRelease)
	<-doneA
}

// Rule 6: a failed load caches nothing and every waiter gets the same error;
// a later GetOrLoad calls load again.
func TestLoadError(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{Now: clk.Now})

	myErr := errors.New("boom")
	started := make(chan struct{})
	release := make(chan struct{})
	var calls int32
	load := func(context.Context) (int, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			close(started)
		}
		<-release
		return 0, myErr
	}

	const n = 5
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			_, err := c.GetOrLoad(context.Background(), "k", load)
			errs <- err
		}()
	}
	<-started
	waitForWaiters(t, c, "k", n) // every waiter has joined
	close(release)

	for i := 0; i < n; i++ {
		if err := <-errs; !errors.Is(err, myErr) {
			t.Fatalf("waiter got %v, want the load's error", err)
		}
	}
	if n := c.Len(); n != 0 {
		t.Fatalf("Len = %d after failed load, want 0", n)
	}
	if _, ok := c.Get("k"); ok {
		t.Fatal("failed load must not cache anything")
	}

}

// Rule 6: a later GetOrLoad after a failed load really calls load again.
func TestLoadErrorRetriedLater(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{Now: clk.Now})

	myErr := errors.New("boom")
	var calls int
	_, err := c.GetOrLoad(context.Background(), "k", func(context.Context) (int, error) {
		calls++
		return 0, myErr
	})
	if !errors.Is(err, myErr) {
		t.Fatalf("err = %v, want %v", err, myErr)
	}
	v, err := c.GetOrLoad(context.Background(), "k", func(context.Context) (int, error) {
		calls++
		return 9, nil
	})
	if err != nil || v != 9 {
		t.Fatalf("GetOrLoad = (%v, %v), want (9, nil)", v, err)
	}
	if calls != 2 {
		t.Fatalf("load called %d times, want 2 (once per attempt)", calls)
	}
}

// Rule 7: a caller whose ctx is done returns ctx.Err() promptly while the
// load keeps running for other waiters.
func TestCallerGivesUpLoadContinues(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{Now: clk.Now})

	started := make(chan struct{})
	release := make(chan struct{})
	load := func(ctx context.Context) (int, error) {
		close(started)
		select {
		case <-release:
			return 42, nil
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}

	gaveUp := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_, err := c.GetOrLoad(ctx, "k", load)
		gaveUp <- err
	}()

	<-started

	// A second caller joins the same in-flight load before the first one
	// gives up.
	vals := make(chan int, 1)
	go func() {
		v, err := c.GetOrLoad(context.Background(), "k", load)
		if err != nil {
			t.Error(err)
		}
		vals <- v
	}()
	waitForWaiters(t, c, "k", 2) // the second caller has joined the load

	cancel() // the first caller's ctx is done before the load finishes
	select {
	case err := <-gaveUp:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("giving-up caller got %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("caller did not return promptly after its ctx was cancelled")
	}

	// The load kept running for the remaining waiter and its value is
	// served from the same, single load.
	close(release)
	if v := <-vals; v != 42 {
		t.Fatalf("late waiter got %d, want 42", v)
	}
	if v, ok := c.Get("k"); !ok || v != 42 {
		t.Fatalf("Get = (%v, %v), want (42, true)", v, ok)
	}
}

// Rule 7: a caller whose ctx is already cancelled at call time gets ctx.Err
// back promptly.
func TestAlreadyCancelledContext(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{Now: clk.Now})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// The load respects its context: with the caller already cancelled, it
	// observes the cancelled load context and stores nothing. (The caller
	// returns ctx.Err regardless; the cache cancels the load context as
	// soon as its only caller gives up.)
	_, err := c.GetOrLoad(ctx, "k", func(loadCtx context.Context) (int, error) {
		time.Sleep(10 * time.Millisecond)
		if loadCtx.Err() != nil {
			return 0, loadCtx.Err()
		}
		return 42, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	time.Sleep(20 * time.Millisecond) // let the load goroutine finish
	if n := c.Len(); n != 0 {
		t.Fatalf("Len = %d, want 0", n)
	}
	if _, ok := c.Get("k"); ok {
		t.Fatal("nothing should be cached for a load that failed")
	}
}

// Rule 8: the load's context is not cancelled while at least one waiter
// remains, even if another caller gave up; it is cancelled once every
// waiter has given up.
func TestLoadContextLifetime(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{Now: clk.Now})

	type probe struct {
		mu           sync.Mutex
		cancelled    bool // loadCtx cancelled while a waiter remained?
		ctxAtLoadCtx context.Context
	}
	p := &probe{}

	started := make(chan struct{})
	release := make(chan struct{})
	load := func(ctx context.Context) (int, error) {
		p.mu.Lock()
		p.ctxAtLoadCtx = ctx
		p.mu.Unlock()
		close(started)
		<-release
		p.mu.Lock()
		select {
		case <-ctx.Done():
			p.cancelled = true
		default:
		}
		p.mu.Unlock()
		return 42, nil
	}

	ctx1, cancel1 := context.WithCancel(context.Background())
	gaveUp := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(ctx1, "k", load)
		gaveUp <- err
	}()

	vals := make(chan int, 1)
	go func() { // a waiter that never gives up
		v, err := c.GetOrLoad(context.Background(), "k", load)
		if err != nil {
			t.Error(err)
		}
		vals <- v
	}()

	<-started
	waitForWaiters(t, c, "k", 2) // the second caller has joined the load
	cancel1()
	select {
	case err := <-gaveUp:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("giving-up caller got %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("caller 1 did not return after cancellation")
	}

	// Caller 2 still waits: the load's context must be alive here.
	p.mu.Lock()
	lctx := p.ctxAtLoadCtx
	p.mu.Unlock()
	select {
	case <-lctx.Done():
		t.Fatal("load context was cancelled while a waiter remained")
	default:
	}

	close(release)
	if v := <-vals; v != 42 {
		t.Fatalf("waiter got %d, want 42", v)
	}

	// The load has been served and every remaining caller has its result;
	// the load context must now be released.
	deadline := time.Now().Add(2 * time.Second)
	for {
		select {
		case <-lctx.Done():
			return
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("load context was never cancelled after the load was served")
		}
		time.Sleep(time.Millisecond)
	}
}

// Rule 8: the load's context is cancelled when the only caller gives up, and
// a load that respects its context returns promptly.
func TestLoadContextCancelledWhenAllGiveUp(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{Now: clk.Now})

	started := make(chan struct{})
	loadCtxDone := make(chan struct{})
	release := make(chan struct{})
	load := func(ctx context.Context) (int, error) {
		close(started)
		select {
		case <-ctx.Done():
			close(loadCtxDone)
			<-release // keep the caller's waitLoad path observable
			return 0, ctx.Err()
		case <-release:
			return 42, nil
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(ctx, "k", load)
		errCh <- err
	}()

	<-started
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("caller got %v, want context.Canceled", err)
	}
	select {
	case <-loadCtxDone:
	case <-time.After(time.Second):
		t.Fatal("load context was not cancelled after the only caller gave up")
	}
	close(release)
	if n := c.Len(); n != 0 {
		t.Fatalf("Len = %d, want 0: a cancelled load stores nothing", n)
	}
}

// Rule 9: a successful late result is stored, with its TTL starting when
// load returned.
func TestLateResultStillStored(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{TTL: 5 * time.Minute, Now: clk.Now})

	started := make(chan struct{})
	release := make(chan struct{})
	load := func(context.Context) (int, error) { // ignores its context
		close(started)
		<-release
		return 42, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(ctx, "k", load)
		errCh <- err
	}()

	<-started
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("caller got %v, want context.Canceled", err)
	}

	// Time passes while the load is still running; the stored entry's TTL
	// must start when load returns, i.e. now, not at load start.
	clk.Advance(3 * time.Minute)
	close(release)
	time.Sleep(50 * time.Millisecond) // let the load goroutine store

	if v, ok := c.Get("k"); !ok || v != 42 {
		t.Fatalf("Get = (%v, %v), want (42, true): late result must be stored", v, ok)
	}

	// Stored at (t0+3m): live until t0+8m, not t0+5m.
	clk.Advance(4*time.Minute + 59*time.Second) // t0+7m59s
	if _, ok := c.Get("k"); !ok {
		t.Fatal("entry expired too early: TTL did not start at load return")
	}
	clk.Advance(2 * time.Second) // t0+8m1s
	if _, ok := c.Get("k"); ok {
		t.Fatal("entry should expire 5m after load returned")
	}
}

// Rule 10: Delete removes the key and does not stop an in-flight load, whose
// result is still stored on completion.
func TestDelete(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{TTL: 5 * time.Minute, Now: clk.Now})

	c.Set("a", 1)
	c.Delete("a")
	if _, ok := c.Get("a"); ok {
		t.Fatal("deleted key must be a miss")
	}
	if n := c.Len(); n != 0 {
		t.Fatalf("Len = %d after Delete, want 0", n)
	}
	c.Delete("missing") // no panic on a missing key

	// In-flight load survives Delete.
	started := make(chan struct{})
	release := make(chan struct{})
	load := func(context.Context) (int, error) {
		close(started)
		<-release
		return 42, nil
	}
	vals := make(chan int, 1)
	go func() {
		v, err := c.GetOrLoad(context.Background(), "k", load)
		if err != nil {
			t.Error(err)
		}
		vals <- v
	}()
	<-started
	c.Delete("k")
	close(release)
	if v := <-vals; v != 42 {
		t.Fatalf("GetOrLoad = %d, want 42", v)
	}
	if v, ok := c.Get("k"); !ok || v != 42 {
		t.Fatalf("Get = (%v, %v), want (42, true): Delete must not stop the load", v, ok)
	}
}

// Rule 11: the cache is safe for concurrent use from many goroutines doing
// mixed operations, and no goroutine it starts outlives the loads it serves.
func TestConcurrentMixedUse(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{TTL: time.Millisecond, MaxEntries: 64, Now: clk.Now})

	before := runtime.NumGoroutine()
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				key := fmt.Sprintf("k%d", i%32)
				switch i % 5 {
				case 0:
					c.Set(key, i)
				case 1:
					c.Get(key)
				case 2:
					c.Delete(key)
				case 3:
					c.Len()
				case 4:
					_, _ = c.GetOrLoad(context.Background(), key, func(context.Context) (int, error) {
						return i, nil
					})
				}
			}
		}(w)
	}
	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()

	// Every load has been served and joined; no cache goroutine may remain.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			t.Fatalf("goroutines: before=%d after=%d; a cache goroutine outlived its load", before, runtime.NumGoroutine())
		}
		time.Sleep(time.Millisecond)
	}
}

// Rule 11: concurrent GetOrLoad for many keys, some failing, with cancellable
// callers mixed in — exercised under -race.
func TestConcurrentGetOrLoadRace(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{TTL: 10 * time.Millisecond, MaxEntries: 8, Now: clk.Now})

	var wg sync.WaitGroup
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				key := fmt.Sprintf("k%d", i%8)
				ctx, cancel := context.WithTimeout(context.Background(), time.Duration(w%3)*time.Millisecond)
				_, _ = c.GetOrLoad(ctx, key, func(context.Context) (int, error) {
					if i%2 == 0 {
						return 0, errors.New("boom")
					}
					return i, nil
				})
				cancel()
			}
		}(w)
	}
	wg.Wait()
}

// Rule 2 + 3: replacing an expired entry is a fresh store — the live-entry
// budget is enforced, so a live entry is evicted if needed.
func TestSetOverExpiredEntryEvicts(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{TTL: 5 * time.Minute, MaxEntries: 1, Now: clk.Now})

	c.Set("a", 1)
	clk.Advance(6 * time.Minute) // "a" expired
	c.Set("b", 2)                // fits: no live entry besides b
	clk.Advance(time.Second)
	c.Set("a", 3) // overwriting the expired "a": live b must be evicted
	if _, ok := c.Get("b"); ok {
		t.Fatal("b should have been evicted: only one live entry fits")
	}
	if v, ok := c.Get("a"); !ok || v != 3 {
		t.Fatalf("Get a = (%v, %v), want (3, true)", v, ok)
	}
}
