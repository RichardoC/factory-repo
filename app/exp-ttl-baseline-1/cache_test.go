package ttlcache

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a controllable clock for expiry tests.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
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

func mustGet[K comparable, V any](t *testing.T, c *Cache[K, V], key K) V {
	t.Helper()
	v, ok := c.Get(key)
	if !ok {
		t.Fatalf("Get(%v): expected hit, got miss", key)
	}
	return v
}

func mustMiss[K comparable, V any](t *testing.T, c *Cache[K, V], key K) {
	t.Helper()
	if _, ok := c.Get(key); ok {
		t.Fatalf("Get(%v): expected miss, got hit", key)
	}
}

// waitFor polls until cond is true, for results published asynchronously by
// a load goroutine.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for condition")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// Rule 1: expiry — live strictly before t+TTL; Get never returns an expired
// entry; Len counts only live entries.
func TestExpiryBoundary(t *testing.T) {
	clk := newFakeClock()
	c := New[string, string](Options{TTL: 10 * time.Second, Now: clk.Now})

	c.Set("k", "v")
	if got := mustGet(t, c, "k"); got != "v" {
		t.Fatalf("got %q, want %q", got, "v")
	}
	clk.Advance(9*time.Second + 999*time.Millisecond)
	if got := mustGet(t, c, "k"); got != "v" {
		t.Fatalf("just before expiry: got %q, want %q", got, "v")
	}
	clk.Advance(time.Millisecond) // exactly t+TTL: expired
	mustMiss(t, c, "k")
	if n := c.Len(); n != 0 {
		t.Fatalf("Len after expiry = %d, want 0", n)
	}
}

func TestLenCountsOnlyLive(t *testing.T) {
	clk := newFakeClock()
	c := New[string, string](Options{TTL: 10 * time.Second, Now: clk.Now})

	c.Set("long", "a")
	clk.Advance(5 * time.Second)
	c.Set("short", "b") // expires 5s after "long"
	if n := c.Len(); n != 2 {
		t.Fatalf("Len = %d, want 2", n)
	}
	clk.Advance(5 * time.Second) // "long" is now expired, "short" is live
	if n := c.Len(); n != 1 {
		t.Fatalf("Len after partial expiry = %d, want 1", n)
	}
	mustMiss(t, c, "long")
	mustGet(t, c, "short")
}

func TestNoTTLNeverExpires(t *testing.T) {
	clk := newFakeClock()
	c := New[string, string](Options{Now: clk.Now}) // TTL 0

	c.Set("k", "v")
	clk.Advance(1000 * time.Hour)
	if got := mustGet(t, c, "k"); got != "v" {
		t.Fatalf("got %q, want %q", got, "v")
	}
	if n := c.Len(); n != 1 {
		t.Fatalf("Len = %d, want 1", n)
	}
}

func TestNilNowUsesTimeNow(t *testing.T) {
	c := New[string, string](Options{TTL: 30 * time.Millisecond})
	c.Set("k", "v")
	mustGet(t, c, "k")
	time.Sleep(60 * time.Millisecond)
	mustMiss(t, c, "k")
}

func TestGetOrLoadMissOnExpired(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{TTL: 10 * time.Second, Now: clk.Now})

	var loads atomic.Int32
	c.Set("k", 1)
	clk.Advance(10 * time.Second)
	v, err := c.GetOrLoad(context.Background(), "k", func(ctx context.Context) (int, error) {
		return int(loads.Add(1)), nil
	})
	if err != nil {
		t.Fatalf("GetOrLoad: %v", err)
	}
	if v != 1 || loads.Load() != 1 {
		t.Fatalf("expired entry was served: v=%d loads=%d", v, loads.Load())
	}
}

// Rule 2: Set replaces the value and restarts the TTL.
func TestSetReplacesAndRestartsTTL(t *testing.T) {
	clk := newFakeClock()
	c := New[string, string](Options{TTL: 10 * time.Second, Now: clk.Now})

	c.Set("k", "v1")
	clk.Advance(6 * time.Second)
	c.Set("k", "v2")
	clk.Advance(6 * time.Second) // 12s after the first Set, 6s after the second
	if got := mustGet(t, c, "k"); got != "v2" {
		t.Fatalf("got %q, want %q", got, "v2")
	}
	clk.Advance(3*time.Second + 999*time.Millisecond) // 9.999s after the second Set
	if got := mustGet(t, c, "k"); got != "v2" {
		t.Fatalf("before restarted expiry: got %q, want %q", got, "v2")
	}
	clk.Advance(time.Millisecond)
	mustMiss(t, c, "k")
}

// Rule 1/2 corollary: hits mark the entry used but never extend its TTL.
func TestHitsDoNotExtendTTL(t *testing.T) {
	clk := newFakeClock()
	c := New[string, string](Options{TTL: 10 * time.Second, Now: clk.Now})

	c.Set("k", "v")
	for i := 0; i < 3; i++ {
		clk.Advance(3 * time.Second)
		mustGet(t, c, "k")
	}
	clk.Advance(10 * time.Second) // 19s after the only Set: hits never extended the TTL
	mustMiss(t, c, "k")
}

// Rule 3: LRU eviction of live entries; a stored entry and a hit each count
// as a use.
func TestLRUEviction(t *testing.T) {
	clk := newFakeClock()
	c := New[string, string](Options{MaxEntries: 2, Now: clk.Now})

	c.Set("a", "1")
	c.Set("b", "2")
	mustGet(t, c, "a") // order now: a (MRU), b (LRU)
	c.Set("c", "3")    // must evict b
	mustGet(t, c, "a")
	mustGet(t, c, "c")
	mustMiss(t, c, "b")
	if n := c.Len(); n != 2 {
		t.Fatalf("Len = %d, want 2", n)
	}
}

func TestLRUWithGetOrLoadHit(t *testing.T) {
	clk := newFakeClock()
	c := New[string, string](Options{MaxEntries: 2, Now: clk.Now})
	c.Set("a", "1")
	c.Set("b", "2")
	if _, err := c.GetOrLoad(context.Background(), "a", func(ctx context.Context) (string, error) {
		t.Fatal("load called on a live hit")
		return "", nil
	}); err != nil {
		t.Fatalf("GetOrLoad: %v", err)
	}
	c.Set("c", "3") // a was just used via GetOrLoad, so b is the LRU victim
	mustGet(t, c, "a")
	mustMiss(t, c, "b")
}

func TestUnboundedWhenMaxEntriesZero(t *testing.T) {
	c := New[int, int](Options{})
	for i := 0; i < 1000; i++ {
		c.Set(i, i)
	}
	if n := c.Len(); n != 1000 {
		t.Fatalf("Len = %d, want 1000", n)
	}
}

// Rule 3: expired entries never cause a live entry to be evicted.
func TestExpiredEntriesDoNotEvictLive(t *testing.T) {
	clk := newFakeClock()
	c := New[string, string](Options{MaxEntries: 2, Now: clk.Now})

	c.Set("a", "1") // TTL 10s
	clk.Advance(5 * time.Second)
	c.Set("b", "2") // expires 5s after a
	clk.Advance(6 * time.Second)
	// "a" is now expired, "b" is live. Storing "c" must evict the expired
	// "a", not the live "b".
	c.Set("c", "3")
	mustGet(t, c, "b")
	mustGet(t, c, "c")
	mustMiss(t, c, "a")
	if n := c.Len(); n != 2 {
		t.Fatalf("Len = %d, want 2", n)
	}

	// Now the cache is full of live entries ordered b (LRU), c (MRU); a
	// further store must evict b.
	c.Set("d", "4")
	mustMiss(t, c, "b")
	mustGet(t, c, "c")
	mustGet(t, c, "d")
}

func TestReplaceAtCapacityDoesNotEvict(t *testing.T) {
	clk := newFakeClock()
	c := New[string, string](Options{MaxEntries: 2, Now: clk.Now})
	c.Set("a", "1")
	c.Set("b", "2")
	c.Set("a", "3") // replacement, not an insertion
	if got := mustGet(t, c, "a"); got != "3" {
		t.Fatalf("got %q, want %q", got, "3")
	}
	mustGet(t, c, "b")
	if n := c.Len(); n != 2 {
		t.Fatalf("Len = %d, want 2", n)
	}
}

// Rule 4: a live hit returns the cached value without calling load.
func TestGetOrLoadHitDoesNotLoad(t *testing.T) {
	c := New[string, string](Options{})
	c.Set("k", "cached")
	v, err := c.GetOrLoad(context.Background(), "k", func(ctx context.Context) (string, error) {
		t.Fatal("load called on a live hit")
		return "", nil
	})
	if err != nil {
		t.Fatalf("GetOrLoad: %v", err)
	}
	if v != "cached" {
		t.Fatalf("got %q, want %q", v, "cached")
	}
}

// Rule 5: concurrent GetOrLoad callers for one key share a single load;
// different keys load independently.
func TestSingleFlightPerKey(t *testing.T) {
	clk := newFakeClock()
	c := New[string, string](Options{Now: clk.Now})

	release := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	var loads, double atomic.Int32
	load := func(ctx context.Context) (string, error) {
		if loads.Add(1) != 1 {
			double.Store(1)
		}
		once.Do(func() { close(entered) })
		<-release
		return "loaded", nil
	}

	const n = 25
	var wg sync.WaitGroup
	ready := make(chan struct{}, n)
	results := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ready <- struct{}{}
			results[i], errs[i] = c.GetOrLoad(context.Background(), "k", load)
		}(i)
	}
	for i := 0; i < n; i++ {
		<-ready // every caller is on its way into GetOrLoad
	}
	time.Sleep(100 * time.Millisecond) // let them all join the in-flight load
	<-entered
	close(release)
	wg.Wait()
	if double.Load() != 0 {
		t.Fatalf("load ran %d times, want 1", loads.Load())
	}
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if results[i] != "loaded" {
			t.Fatalf("caller %d: got %q, want %q", i, results[i], "loaded")
		}
	}
	if got := loads.Load(); got != 1 {
		t.Fatalf("load ran %d times, want 1", got)
	}
	if got := mustGet(t, c, "k"); got != "loaded" {
		t.Fatalf("cached %q, want %q", got, "loaded")
	}
}

func TestDifferentKeysLoadIndependently(t *testing.T) {
	c := New[string, string](Options{})

	blockA := make(chan struct{})
	enteredA := make(chan struct{})
	loadA := func(ctx context.Context) (string, error) {
		close(enteredA)
		<-blockA
		return "A", nil
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		v, err := c.GetOrLoad(context.Background(), "a", loadA)
		if err != nil || v != "A" {
			t.Errorf("key a: v=%q err=%v", v, err)
		}
	}()
	<-enteredA

	// Key "b" must load to completion while "a" is still blocked.
	v, err := c.GetOrLoad(context.Background(), "b", func(ctx context.Context) (string, error) {
		return "B", nil
	})
	if err != nil || v != "B" {
		t.Fatalf("key b: v=%q err=%v", v, err)
	}
	close(blockA)
	wg.Wait()
}

// Rule 6: load errors are not cached and reach every waiter; a later
// GetOrLoad calls load again.
func TestLoadErrorNotCached(t *testing.T) {
	c := New[string, string](Options{})
	boom := errors.New("boom")
	var loads atomic.Int32
	load := func(ctx context.Context) (string, error) {
		return "", fmt.Errorf("%w #%d", boom, loads.Add(1))
	}
	if _, err := c.GetOrLoad(context.Background(), "k", load); err == nil {
		t.Fatal("expected an error")
	}
	mustMiss(t, c, "k")
	if n := c.Len(); n != 0 {
		t.Fatalf("Len after failed load = %d, want 0", n)
	}
	if _, err := c.GetOrLoad(context.Background(), "k", load); err == nil {
		t.Fatal("expected the second load to run and fail again")
	}
	if got := loads.Load(); got != 2 {
		t.Fatalf("load ran %d times, want 2", got)
	}
}

func TestAllWaitersGetSameError(t *testing.T) {
	c := New[string, string](Options{})
	boom := errors.New("boom")
	release := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	load := func(ctx context.Context) (string, error) {
		once.Do(func() { close(entered) })
		<-release
		return "", boom
	}
	const n = 10
	var wg sync.WaitGroup
	ready := make(chan struct{}, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ready <- struct{}{}
			_, errs[i] = c.GetOrLoad(context.Background(), "k", load)
		}(i)
	}
	for i := 0; i < n; i++ {
		<-ready // every caller is on its way into GetOrLoad
	}
	time.Sleep(100 * time.Millisecond) // let them all join the in-flight load
	<-entered
	close(release)
	wg.Wait()
	for i, err := range errs {
		if !errors.Is(err, boom) {
			t.Fatalf("waiter %d: got %v, want %v", i, err, boom)
		}
	}
	mustMiss(t, c, "k")
}

// Rule 7: a caller whose ctx is done returns ctx.Err() promptly while the
// load keeps running for the remaining waiter.
func TestCallerGivesUpLoadContinues(t *testing.T) {
	c := New[string, string](Options{})
	release := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	load := func(ctx context.Context) (string, error) {
		once.Do(func() { close(entered) })
		<-release
		return "late", nil
	}

	gaveUp := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := c.GetOrLoad(ctx, "k", load)
		gaveUp <- err
	}()
	<-entered
	cancel()

	// The caller must return ctx.Err() while the load is still blocked.
	select {
	case err := <-gaveUp:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("giving-up caller got %v, want %v", err, context.Canceled)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("caller did not return promptly after its ctx was cancelled")
	}

	// The load keeps running for the other waiter.
	stillWaiting := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := c.GetOrLoad(context.Background(), "k", load)
		stillWaiting <- err
	}()
	// Give the second caller time to join the in-flight load, then let the
	// load finish.
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()
	if err := <-stillWaiting; err != nil {
		t.Fatalf("remaining waiter: %v", err)
	}
	if got := mustGet(t, c, "k"); got != "late" {
		t.Fatalf("cached %q, want %q", got, "late")
	}
}

// Rule 8, part 1: the load's context is NOT cancelled while at least one
// waiter remains, even though another caller was cancelled.
func TestLoadContextSurvivesWhileWaitersRemain(t *testing.T) {
	c := New[string, string](Options{})
	release := make(chan struct{})
	entered := make(chan struct{})
	load := func(ctx context.Context) (string, error) {
		close(entered)
		select {
		case <-ctx.Done():
			return "", ctx.Err() // wrong: load ctx cancelled too early
		case <-release:
			return "ok", nil
		}
	}

	// B joins first and stays: once <-entered fires, the load is running
	// and B is provably a waiter on it.
	var wg sync.WaitGroup
	wg.Add(1)
	bVal := make(chan string, 1)
	go func() {
		defer wg.Done()
		v, err := c.GetOrLoad(context.Background(), "k", load)
		if err != nil {
			t.Errorf("remaining waiter: %v", err)
			bVal <- ""
			return
		}
		bVal <- v
	}()
	<-entered

	// A joins the in-flight load and then gives up; the load must keep
	// running for B.
	ctxA, cancelA := context.WithCancel(context.Background())
	aErr := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := c.GetOrLoad(ctxA, "k", load)
		aErr <- err
	}()
	time.Sleep(50 * time.Millisecond) // let A join the in-flight load
	cancelA()
	select {
	case err := <-aErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled caller got %v, want %v", err, context.Canceled)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("caller did not return promptly after its ctx was cancelled")
	}
	// Give a buggy implementation time to cancel the load context.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if v := <-bVal; v != "ok" {
		t.Fatalf("remaining waiter got %q, want %q", v, "ok")
	}
}

// Rule 8, part 2: the load's context IS cancelled exactly when every caller
// waiting on it has given up.
func TestLoadContextCancelledWhenAllGiveUp(t *testing.T) {
	c := New[string, string](Options{})
	entered := make(chan struct{})
	loadCtxDone := make(chan struct{})
	load := func(ctx context.Context) (string, error) {
		close(entered)
		<-ctx.Done()
		close(loadCtxDone)
		return "", ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := c.GetOrLoad(ctx, "k", load)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("caller got %v, want %v", err, context.Canceled)
		}
	}()
	<-entered
	cancel()
	wg.Wait()

	select {
	case <-loadCtxDone:
		// The load observed its context being cancelled.
	case <-time.After(2 * time.Second):
		t.Fatal("load context was not cancelled after every caller gave up")
	}
	mustMiss(t, c, "k") // the load did not return a value
}

// Rule 9: a value returned by load is stored with its TTL starting when load
// returned, even if every caller gave up.
func TestLateResultStoredWithFreshTTL(t *testing.T) {
	clk := newFakeClock()
	c := New[string, string](Options{TTL: 10 * time.Second, Now: clk.Now})

	release := make(chan struct{})
	entered := make(chan struct{})
	load := func(ctx context.Context) (string, error) {
		close(entered)
		<-release // ignores ctx: returns late, successfully
		return "late", nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	gaveUp := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(ctx, "k", load)
		gaveUp <- err
	}()
	<-entered
	cancel()
	if err := <-gaveUp; !errors.Is(err, context.Canceled) {
		t.Fatalf("caller got %v, want %v", err, context.Canceled)
	}

	clk.Advance(30 * time.Second) // time passes while the load is still out
	close(release)
	waitFor(t, func() bool {
		_, ok := c.Get("k")
		return ok
	})
	// The TTL must run from the moment load returned (t0+30s), so the entry
	// is live until t0+40s.
	clk.Advance(9*time.Second + 999*time.Millisecond)
	if got := mustGet(t, c, "k"); got != "late" {
		t.Fatalf("got %q, want %q", got, "late")
	}
	clk.Advance(time.Millisecond)
	mustMiss(t, c, "k")
}

// Rule 10: Delete removes the key and does not stop an in-flight load.
func TestDelete(t *testing.T) {
	c := New[string, string](Options{})
	c.Set("k", "v")
	c.Delete("k")
	mustMiss(t, c, "k")
	c.Delete("missing") // no-op
	if n := c.Len(); n != 0 {
		t.Fatalf("Len = %d, want 0", n)
	}
}

func TestDeleteDuringLoadStillStores(t *testing.T) {
	c := New[string, string](Options{})
	release := make(chan struct{})
	entered := make(chan struct{})
	load := func(ctx context.Context) (string, error) {
		close(entered)
		<-release
		return "late", nil
	}
	res := make(chan string, 1)
	go func() {
		v, err := c.GetOrLoad(context.Background(), "k", load)
		if err != nil {
			t.Errorf("GetOrLoad: %v", err)
		}
		res <- v
	}()
	<-entered
	c.Delete("k")
	mustMiss(t, c, "k")
	close(release)
	if got := <-res; got != "late" {
		t.Fatalf("caller got %q, want %q", got, "late")
	}
	// The in-flight load's result is still stored.
	waitFor(t, func() bool {
		_, ok := c.Get("k")
		return ok
	})
	if got := mustGet(t, c, "k"); got != "late" {
		t.Fatalf("got %q, want %q", got, "late")
	}
}

// Rule 11: safe for concurrent use; run with -race.
func TestConcurrentStress(t *testing.T) {
	c := New[int, int](Options{TTL: 20 * time.Millisecond, MaxEntries: 32})

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(g)))
			for i := 0; i < 300; i++ {
				key := rng.Intn(24)
				switch rng.Intn(4) {
				case 0:
					c.Set(key, i)
				case 1:
					c.Get(key)
				case 2:
					c.Delete(key)
				default:
					_, err := c.GetOrLoad(context.Background(), key, func(ctx context.Context) (int, error) {
						time.Sleep(time.Millisecond)
						if key%5 == 0 {
							return 0, errors.New("boom")
						}
						return key, nil
					})
					_ = err
				}
				c.Len()
			}
		}(g)
	}
	wg.Wait()
}

// Rule 11: no goroutine the cache starts outlives the loads it serves.
func TestNoGoroutineLeak(t *testing.T) {
	c := New[int, int](Options{})
	var loads atomic.Int32
	load := func(ctx context.Context) (int, error) {
		return int(loads.Add(1)), nil
	}
	runtime.GC()
	time.Sleep(20 * time.Millisecond)
	base := runtime.NumGoroutine()
	for i := 0; i < 50; i++ {
		if _, err := c.GetOrLoad(context.Background(), i, load); err != nil {
			t.Fatalf("GetOrLoad: %v", err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > base {
		if time.Now().After(deadline) {
			t.Fatalf("goroutines: base=%d now=%d; a load goroutine outlived its load", base, runtime.NumGoroutine())
		}
		time.Sleep(5 * time.Millisecond)
	}
}
