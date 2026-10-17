package ttlcache

import (
	"context"
	"errors"
	"math/rand/v2"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a controllable clock safe for concurrent use.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

// waitFor waits for cond to hold, up to 2s, and reports a fatal error at t
// if it never does.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// waitersFor returns how many callers are currently waiting on the
// in-flight load for key (white-box).
func waitersFor[K comparable, V any](t *testing.T, c *Cache[K, V], key K) int {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	st, ok := c.loads[key]
	if !ok {
		return 0
	}
	return st.waiters
}

// --- Rule 1: expiry ---

func TestExpiryGetAndLenCountOnlyLive(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{TTL: time.Minute, Now: clk.Now})
	c.Set("k", 5)

	if v, ok := c.Get("k"); !ok || v != 5 {
		t.Fatalf("entry live immediately after store: got %v %v", v, ok)
	}
	clk.Advance(time.Minute - time.Nanosecond) // still before t+TTL
	if _, ok := c.Get("k"); !ok {
		t.Fatal("entry must be live strictly before t+TTL")
	}
	if got := c.Len(); got != 1 {
		t.Fatalf("Len before expiry = %d, want 1", got)
	}

	clk.Advance(time.Nanosecond) // now == t+TTL: expired
	if _, ok := c.Get("k"); ok {
		t.Fatal("Get must never return an expired entry")
	}
	if got := c.Len(); got != 0 {
		t.Fatalf("Len must count only live entries: got %d, want 0", got)
	}
}

func TestExpiryGetOrLoadNeverReturnsExpired(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{TTL: time.Minute, Now: clk.Now})
	c.Set("k", 5)
	clk.Advance(time.Minute)
	var calls atomic.Int32
	v, err := c.GetOrLoad(context.Background(), "k", func(context.Context) (int, error) {
		calls.Add(1)
		return 9, nil
	})
	if err != nil {
		t.Fatalf("GetOrLoad on expired entry: %v", err)
	}
	if v != 9 {
		t.Fatalf("GetOrLoad on a miss must reload: got %d", v)
	}
	if calls.Load() != 1 {
		t.Fatalf("load called %d times, want 1", calls.Load())
	}
}

func TestTTLZeroNeverExpires(t *testing.T) {
	clk := newFakeClock()
	c := New[int, string](Options{Now: clk.Now}) // TTL 0, unbounded
	for i := 0; i < 50; i++ {
		c.Set(i, "x")
	}
	clk.Advance(1000 * time.Hour)
	if got := c.Len(); got != 50 {
		t.Fatalf("TTL 0 entries must never expire: Len = %d, want 50", got)
	}
}

// --- Rule 2: replacement restarts the TTL ---

func TestSetReplacesValueAndRestartsTTL(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{TTL: time.Minute, Now: clk.Now})
	c.Set("k", 1)
	clk.Advance(30 * time.Second)
	c.Set("k", 2) // restart: expiry is now t0+90s
	if v, ok := c.Get("k"); !ok || v != 2 {
		t.Fatalf("replaced value: got %v %v, want 2 true", v, ok)
	}
	clk.Advance(45 * time.Second) // t0+75s: old expiry passed, new not
	if _, ok := c.Get("k"); !ok {
		t.Fatal("Set must restart the TTL: entry expired too early")
	}
	clk.Advance(15 * time.Second) // t0+90s: restarted expiry reached
	if _, ok := c.Get("k"); ok {
		t.Fatal("entry must expire TTL after it was last stored")
	}
}

// --- Rule 3: LRU eviction ---

func TestLRUEvictsLeastRecentlyUsed(t *testing.T) {
	c := New[string, int](Options{MaxEntries: 2}) // TTL 0: never expires
	c.Set("a", 1)
	c.Set("b", 2)
	c.Get("a") // a is now most recently used; b is the LRU entry
	c.Set("c", 3)
	if _, ok := c.Get("b"); ok {
		t.Error("LRU entry b must be evicted by the store of c")
	}
	if v, ok := c.Get("a"); !ok || v != 1 {
		t.Errorf("recently used a must survive, got %v %v", v, ok)
	}
	if v, ok := c.Get("c"); !ok || v != 3 {
		t.Errorf("newly stored c must be present, got %v %v", v, ok)
	}
}

func TestSetExistingKeyUpdatesNotEvicts(t *testing.T) {
	c := New[string, int](Options{MaxEntries: 2})
	c.Set("a", 1)
	c.Set("b", 2)
	c.Set("a", 10) // replacement, count unchanged: no eviction
	if _, ok := c.Get("b"); !ok {
		t.Error("replacing an existing key must not evict other entries")
	}
}

func TestExpiredEntriesNeverCauseEviction(t *testing.T) {
	clk := newFakeClock()
	// Cap of 2. a expires at t0+10s, b at t0+11s.
	c := New[string, int](Options{TTL: 10 * time.Second, MaxEntries: 2, Now: clk.Now})
	c.Set("a", 1)
	clk.Advance(time.Second)
	c.Set("b", 2)
	clk.Advance(9 * time.Second) // a expired, b still live
	c.Set("c", 3)                // live count is 2 (b, c): no live entry evicted
	for k, want := range map[string]int{"b": 2, "c": 3} {
		if v, ok := c.Get(k); !ok || v != want {
			t.Errorf("live entry %q must not be evicted because an expired entry existed: got %v %v", k, v, ok)
		}
	}
}

func TestLoadCompletionCountsAsUse(t *testing.T) {
	c := New[string, int](Options{MaxEntries: 2})
	for _, k := range []string{"k1", "k2"} {
		v, err := c.GetOrLoad(context.Background(), k, func(context.Context) (int, error) {
			return 1, nil
		})
		if err != nil {
			t.Fatalf("load %s: %v", k, err)
		}
		if v != 1 {
			t.Fatalf("load %s: got %d", k, v)
		}
	}
	// k1 is now older than k2; a hit on k1 makes k2 the LRU entry.
	if v, err := c.GetOrLoad(context.Background(), "k1", func(context.Context) (int, error) {
		t.Fatal("hit must not call load")
		return 0, nil
	}); err != nil || v != 1 {
		t.Fatalf("hit on k1: %v %v", v, err)
	}
	// Storing k3 evicts k2, the least recently used.
	if _, err := c.GetOrLoad(context.Background(), "k3", func(context.Context) (int, error) {
		return 3, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Get("k2"); ok {
		t.Error("a completed load counts as a use; k2 was LRU and must be evicted")
	}
	if _, ok := c.Get("k1"); !ok {
		t.Error("k1 was used recently and must survive")
	}
}

// --- Rule 4: a hit never calls load ---

func TestHitDoesNotCallLoad(t *testing.T) {
	c := New[string, int](Options{TTL: time.Minute})
	c.Set("k", 7)
	var calls atomic.Int32
	v, err := c.GetOrLoad(context.Background(), "k", func(context.Context) (int, error) {
		calls.Add(1)
		return 0, errors.New("must not be called")
	})
	if err != nil || v != 7 {
		t.Fatalf("hit: got %v, %v; want 7, nil", v, err)
	}
	if calls.Load() != 0 {
		t.Fatal("GetOrLoad on a live hit must not call load")
	}
}

// --- Rule 5: one load per key ---

func TestConcurrentMissesShareOneLoad(t *testing.T) {
	c := New[string, int](Options{TTL: time.Minute})
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	var entered atomic.Int32
	load := func(context.Context) (int, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return 42, nil
	}
	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			entered.Add(1)
			v, err := c.GetOrLoad(context.Background(), "k", load)
			if err != nil || v != 42 {
				errs <- errors.New("waiter result invalid")
			}
		}()
	}
	<-started
	// All n callers must join the single in-flight load before it finishes.
	waitFor(t, "all callers on the in-flight load", func() bool {
		return entered.Load() == n && waitersFor(t, c, "k") == n
	})
	if calls.Load() != 1 {
		t.Fatalf("only one load may run at a time: started %d", calls.Load())
	}
	close(release)
	wg.Wait()
	select {
	case err := <-errs:
		t.Error(err)
	default:
	}
	if calls.Load() != 1 {
		t.Fatalf("concurrent GetOrLoad for the same key: %d loads, want 1", calls.Load())
	}
}

func TestDifferentKeysLoadIndependently(t *testing.T) {
	c := New[string, int](Options{TTL: time.Minute})
	startedA := make(chan struct{})
	releaseA := make(chan struct{})
	loadA := func(context.Context) (int, error) {
		startedA <- struct{}{}
		<-releaseA
		return 1, nil
	}
	go c.GetOrLoad(context.Background(), "a", loadA)
	<-startedA // a's load is now in flight and blocked

	done := make(chan int, 1)
	go func() {
		v, err := c.GetOrLoad(context.Background(), "b", func(context.Context) (int, error) {
			return 2, nil
		})
		if err != nil {
			done <- -1
			return
		}
		done <- v
	}()
	select {
	case v := <-done:
		if v != 2 {
			t.Fatalf("key b must load without waiting for key a, got %d", v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("loads for different keys must not block each other")
	}
	close(releaseA)
}

// --- Rule 6: load errors ---

func TestLoadErrorIsSharedAndNotCached(t *testing.T) {
	sentinel := errors.New("boom")
	c := New[string, int](Options{TTL: time.Minute})
	release := make(chan struct{})
	var calls atomic.Int32
	load := func(context.Context) (int, error) {
		if calls.Add(1) == 1 {
			<-release
			return 0, sentinel
		}
		return 5, nil
	}
	var entered atomic.Int32
	const n = 4
	results := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			entered.Add(1)
			_, err := c.GetOrLoad(context.Background(), "k", load)
			results <- err
		}()
	}
	waitFor(t, "all callers sharing the failing load", func() bool {
		return entered.Load() == n && waitersFor(t, c, "k") == n
	})
	close(release)
	for i := 0; i < n; i++ {
		if err := <-results; !errors.Is(err, sentinel) {
			t.Fatalf("every waiter must get the load's error: got %v", err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("shared failing load ran %d times, want 1", calls.Load())
	}
	if got := c.Len(); got != 0 {
		t.Fatalf("a failed load must cache nothing: Len = %d", got)
	}
	if _, ok := c.Get("k"); ok {
		t.Fatal("a failed load must cache nothing")
	}
	// A later GetOrLoad calls load again and stores the new result.
	v, err := c.GetOrLoad(context.Background(), "k", load)
	if err != nil || v != 5 {
		t.Fatalf("retry after error: got %v, %v; want 5, nil", v, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("load must be called again after an error: %d calls", calls.Load())
	}
}

// --- Rule 7: callers that give up ---

func TestCancelledCallerReturnsPromptly(t *testing.T) {
	c := New[string, int](Options{TTL: time.Minute})
	started := make(chan struct{})
	release := make(chan struct{})
	load := func(ctx context.Context) (int, error) {
		close(started)
		<-release
		return 9, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	var got atomic.Int32
	go func() {
		v, err := c.GetOrLoad(ctx, "k", load)
		if err != context.Canceled || v != 0 {
			got.Add(2)
		} else {
			got.Add(1)
		}
	}()
	<-started
	cancel()
	waitFor(t, "cancelled caller to return ctx.Err()", func() bool { return got.Load() >= 1 })
	if got.Load() != 1 {
		t.Fatal("cancelled caller must return ctx.Err() promptly with the zero value")
	}
	// It returned without the load having finished. Let the load finish.
	close(release)
	waitFor(t, "value to be recorded from the abandoned load", func() bool {
		return c.Len() == 1
	})
}

func TestPreCancelledCaller(t *testing.T) {
	c := New[string, int](Options{TTL: time.Minute})
	release := make(chan struct{})
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	v, err := c.GetOrLoad(ctx, "k", func(context.Context) (int, error) {
		<-release
		return 9, nil
	})
	if err != context.Canceled || v != 0 {
		t.Fatalf("caller with a done ctx must get ctx.Err(): got %v, %v", v, err)
	}
}

// --- Rule 8: the load's context ---

func TestLoadContextCancelledOnlyWhenAllWaitersLeave(t *testing.T) {
	c := New[string, int](Options{TTL: time.Minute})
	lctxCh := make(chan context.Context, 1)
	load := func(ctx context.Context) (int, error) {
		lctxCh <- ctx
		<-ctx.Done() // stay in flight until cancelled (or forever)
		return 0, ctx.Err()
	}
	ctx1, cancel1 := context.WithCancel(context.Background())
	ctx2, cancel2 := context.WithCancel(context.Background())

	go c.GetOrLoad(ctx1, "k", load)
	lctx := <-lctxCh // the first caller started the load

	go c.GetOrLoad(ctx2, "k", load) // second caller joins
	waitFor(t, "second caller to join the load", func() bool {
		return waitersFor(t, c, "k") == 2
	})

	cancel1()
	waitFor(t, "first caller to give up", func() bool { return waitersFor(t, c, "k") == 1 })
	if lctx.Err() != nil {
		t.Fatal("the load context must not be cancelled while a waiter remains")
	}

	cancel2()
	waitFor(t, "load context to be cancelled after the last waiter left", func() bool {
		return lctx.Err() != nil
	})
	if !errors.Is(lctx.Err(), context.Canceled) {
		t.Fatalf("load context: %v, want %v", lctx.Err(), context.Canceled)
	}
	// The load goroutine observes cancellation and returns nothing cached.
	waitFor(t, "cache to settle", func() bool { return c.Len() == 0 })
}

// --- Rule 9: late results ---

func TestLateResultStoredEvenIfAllGaveUp(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{TTL: time.Minute, Now: clk.Now})
	lctxCh := make(chan context.Context, 1)
	release := make(chan struct{})
	load := func(ctx context.Context) (int, error) {
		lctxCh <- ctx
		<-release // ignore ctx: keep running past every caller giving up
		return 13, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	var done atomic.Int32
	go func() {
		_, err := c.GetOrLoad(ctx, "k", load)
		if err != context.Canceled {
			done.Add(2)
		} else {
			done.Add(1)
		}
	}()
	lctx := <-lctxCh
	cancel()
	waitFor(t, "caller to give up", func() bool { return done.Load() >= 1 })
	// With every waiter gone, the load context must be cancelled (rule 8) ...
	waitFor(t, "load context cancelled after the last waiter left", func() bool {
		return lctx.Err() == context.Canceled
	})
	if c.Len() != 0 {
		t.Fatal("nothing cached before the load returns")
	}

	// The load returns at this clock time; its entry expires then+TTL.
	returnedAt := clk.Now()
	close(release)
	waitFor(t, "late result to be stored", func() bool {
		_, ok := c.Get("k")
		return ok
	})
	// Advance to just before returnedAt+TTL: still live.
	clk.Advance(returnedAt.Add(c.ttl).Sub(clk.Now()) - time.Nanosecond)
	if v, ok := c.Get("k"); !ok || v != 13 {
		t.Fatalf("late result must be stored with TTL from load return: got %v %v", v, ok)
	}
	clk.Advance(time.Nanosecond) // clk.Now() == returnedAt+TTL: expired
	if _, ok := c.Get("k"); ok {
		t.Fatal("the stored late result must expire TTL after the load returned")
	}
}

// --- Rule 10: delete ---

func TestDeleteRemovesEntry(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{TTL: time.Minute, Now: clk.Now})
	c.Set("a", 1)
	c.Set("b", 2)
	c.Delete("a")
	if _, ok := c.Get("a"); ok {
		t.Error("Delete must remove the entry")
	}
	if got := c.Len(); got != 1 {
		t.Errorf("Len after Delete = %d, want 1", got)
	}
	c.Delete("nope") // deleting a missing key is a no-op
	if v, ok := c.Get("b"); !ok || v != 2 {
		t.Error("Delete of another key must not disturb other entries")
	}
}

func TestDeleteDoesNotStopInFlightLoad(t *testing.T) {
	c := New[string, int](Options{TTL: time.Minute})
	started := make(chan struct{})
	release := make(chan struct{})
	load := func(context.Context) (int, error) {
		close(started)
		<-release
		return 21, nil
	}
	go c.GetOrLoad(context.Background(), "k", load)
	<-started
	c.Delete("k") // no entry cached anyway; must not stop or fail the load
	close(release)
	waitFor(t, "load's result still stored after Delete", func() bool {
		v, ok := c.Get("k")
		return ok && v == 21
	})
	if got := c.Len(); got != 1 {
		t.Fatalf("Len after Delete-then-load-completion = %d, want 1", got)
	}
}

// --- Rule 11: concurrency ---

func TestConcurrentAccessStress(t *testing.T) {
	c := New[int, int](Options{TTL: 5 * time.Millisecond, MaxEntries: 50})
	load := func(context.Context) (int, error) {
		return 7, nil
	}
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := range 300 {
				k := rand.IntN(40)
				switch (i + g) % 5 {
				case 0, 1:
					c.Set(k, k)
				case 2:
					c.Get(k)
				case 3:
					c.Delete(k)
				case 4:
					if _, err := c.GetOrLoad(context.Background(), k, load); err != nil {
						t.Errorf("GetOrLoad: %v", err)
					}
				}
				c.Len()
			}
		}(g)
	}
	wg.Wait()
}

func TestNoGoroutineOutlivesItsLoad(t *testing.T) {
	before := runtime.NumGoroutine()
	c := New[string, int](Options{TTL: time.Minute})
	for range 5 {
		started := make(chan struct{})
		release := make(chan struct{})
		load := func(context.Context) (int, error) {
			close(started)
			<-release
			return 1, nil
		}
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			_, _ = c.GetOrLoad(ctx, "k", load) // caller gives up
		}()
		<-started
		cancel()
		close(release)
		waitFor(t, "value stored after pre-release round", func() bool { return c.Len() == 1 })
		c.Delete("k")
		if c.Len() != 0 {
			t.Fatalf("Len = %d, want 0", c.Len())
		}
	}
	waitFor(t, "goroutines to settle back to baseline", func() bool {
		runtime.Gosched()
		return runtime.NumGoroutine() <= before
	})
}

// --- options / defaults ---

func TestDefaultClockIsRealTime(t *testing.T) {
	c := New[int, string](Options{TTL: time.Minute}) // Now nil: time.Now
	c.Set(1, "one")
	if v, ok := c.Get(1); !ok || v != "one" {
		t.Fatalf("default clock: got %v %v, want one true", v, ok)
	}
	v, err := c.GetOrLoad(context.Background(), 2, func(context.Context) (string, error) {
		return "two", nil
	})
	if err != nil || v != "two" {
		t.Fatalf("GetOrLoad with default clock: %v %v", v, err)
	}
	if got := c.Len(); got != 2 {
		t.Fatalf("Len = %d, want 2", got)
	}
}

func TestNilLoadOnMiss(t *testing.T) {
	c := New[string, int](Options{TTL: time.Minute})
	if _, err := c.GetOrLoad(context.Background(), "k", nil); err == nil {
		t.Fatal("nil load on a miss must not succeed silently")
	}
	if got := c.Len(); got != 0 {
		t.Fatalf("nil load must cache nothing: Len = %d", got)
	}
}
