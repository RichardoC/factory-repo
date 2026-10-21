package ttlcache

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a manually advanced clock for deterministic expiry tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Unix(1_700_000_000, 0)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func mustGet[V any](t *testing.T, c *Cache[string, V], key string) V {
	t.Helper()
	v, ok := c.Get(key)
	if !ok {
		t.Fatalf("Get(%q) = miss, want hit", key)
	}
	return v
}

// Rule 1: an entry is live while Now() is before t+TTL; at t+TTL it is gone.
func TestExpiry(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{TTL: time.Minute, Now: clk.Now})
	c.Set("a", 1)
	clk.Advance(time.Minute - time.Nanosecond)
	if v := mustGet(t, c, "a"); v != 1 {
		t.Fatalf("got %d", v)
	}
	clk.Advance(time.Nanosecond) // exactly stored-at + TTL
	if _, ok := c.Get("a"); ok {
		t.Fatal("expired entry returned by Get")
	}
	if n := c.Len(); n != 0 {
		t.Fatalf("Len = %d, want 0", n)
	}

	c.Set("b", 2)
	clk.Advance(time.Minute)
	got, err := c.GetOrLoad(context.Background(), "b", func(context.Context) (int, error) {
		return 20, nil
	})
	if err != nil || got != 20 {
		t.Fatalf("GetOrLoad on expired entry = %d, %v; want fresh load", got, err)
	}
}

// Rule 1: TTL 0 means entries never expire.
func TestNoTTLNeverExpires(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{Now: clk.Now})
	c.Set("a", 1)
	clk.Advance(1000 * time.Hour)
	if v := mustGet(t, c, "a"); v != 1 {
		t.Fatalf("got %d", v)
	}
	if n := c.Len(); n != 1 {
		t.Fatalf("Len = %d", n)
	}
}

// Rule 1: Len counts only live entries.
func TestLenCountsLiveOnly(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{TTL: time.Minute, Now: clk.Now})
	c.Set("a", 1)
	clk.Advance(30 * time.Second)
	c.Set("b", 2)
	clk.Advance(30 * time.Second) // a expired, b live
	if n := c.Len(); n != 1 {
		t.Fatalf("Len = %d, want 1", n)
	}
}

// Rule 2: Set replaces the value and restarts the TTL.
func TestReplacementRestartsTTL(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{TTL: time.Minute, Now: clk.Now})
	c.Set("a", 1)
	clk.Advance(50 * time.Second)
	c.Set("a", 2)
	clk.Advance(30 * time.Second) // 80s after first set, 30s after second
	if v := mustGet(t, c, "a"); v != 2 {
		t.Fatalf("got %d, want replaced 2", v)
	}
	if n := c.Len(); n != 1 {
		t.Fatalf("Len = %d, want 1 (replacement must not duplicate)", n)
	}
	clk.Advance(30 * time.Second) // exactly 60s after the replacement
	if _, ok := c.Get("a"); ok {
		t.Fatal("replaced entry did not expire on its restarted TTL")
	}
}

// Rule 3: LRU eviction; stores and hits both count as use.
func TestLRUEviction(t *testing.T) {
	c := New[string, int](Options{MaxEntries: 2})
	c.Set("a", 1)
	c.Set("b", 2)
	mustGet(t, c, "a") // a is now most recently used, b least
	c.Set("c", 3)      // evicts b
	if _, ok := c.Get("b"); ok {
		t.Fatal("b should have been evicted")
	}
	mustGet(t, c, "a")
	mustGet(t, c, "c")
	if n := c.Len(); n != 2 {
		t.Fatalf("Len = %d, want 2", n)
	}

	// A completed load stores and counts as a use.
	c.Set("d", 4) // evicts a (a is LRU: order was c, a)
	if _, ok := c.Get("a"); ok {
		t.Fatal("a should have been evicted")
	}
	if v := mustGet(t, c, "c"); v != 3 {
		t.Fatalf("got %d", v)
	}
	c.Set("e", 5) // evicts d
	if _, ok := c.Get("d"); ok {
		t.Fatal("d should have been evicted")
	}
}

// Rule 3: unbounded when MaxEntries is 0.
func TestUnbounded(t *testing.T) {
	c := New[int, int](Options{})
	for i := 0; i < 100; i++ {
		c.Set(i, i)
	}
	if n := c.Len(); n != 100 {
		t.Fatalf("Len = %d, want 100", n)
	}
}

// Rule 3: expired entries never cause a live entry to be evicted.
func TestEvictionPrefersExpired(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{TTL: time.Minute, MaxEntries: 2, Now: clk.Now})
	c.Set("old", 1)
	clk.Advance(30 * time.Second)
	c.Set("live", 2)
	clk.Advance(31 * time.Second) // "old" expired, "live" still live
	c.Set("new", 3)
	if _, ok := c.Get("old"); ok {
		t.Fatal("expired entry still present")
	}
	if v := mustGet(t, c, "live"); v != 2 {
		t.Fatalf("live entry was evicted: got %d", v)
	}
	mustGet(t, c, "new")
}

// Rule 4: a live hit does not call load.
func TestHitSkipsLoad(t *testing.T) {
	c := New[string, int](Options{TTL: time.Minute})
	c.Set("a", 7)
	var calls int32
	got, err := c.GetOrLoad(context.Background(), "a", func(context.Context) (int, error) {
		atomic.AddInt32(&calls, 1)
		return 0, nil
	})
	if err != nil || got != 7 {
		t.Fatalf("got %d, %v", got, err)
	}
	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Fatalf("load called %d times on a hit", n)
	}
}

// gate blocks a load until released and records what it saw.
type gate struct {
	entered chan struct{}
	release chan struct{}
}

func newGate() *gate {
	return &gate{entered: make(chan struct{}, 16), release: make(chan struct{})}
}

// Rule 5: concurrent misses for one key share a single load.
func TestSingleFlight(t *testing.T) {
	c := New[string, int](Options{})
	g := newGate()
	var calls int32
	load := func(context.Context) (int, error) {
		atomic.AddInt32(&calls, 1)
		g.entered <- struct{}{}
		<-g.release
		return 42, nil
	}

	const n = 8
	var wg sync.WaitGroup
	results := make([]int, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = c.GetOrLoad(context.Background(), "k", load)
		}(i)
	}
	<-g.entered // the single load is running
	close(g.release)
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil || results[i] != 42 {
			t.Fatalf("caller %d = %d, %v", i, results[i], errs[i])
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("load called %d times, want 1", got)
	}
}

// Rule 5: different keys load independently, without blocking each other.
func TestDifferentKeysIndependent(t *testing.T) {
	c := New[string, int](Options{})
	aEntered := make(chan struct{})
	releaseA := make(chan struct{})
	bDone := make(chan struct{})

	aDone := make(chan struct{})
	go func() {
		defer close(aDone)
		c.GetOrLoad(context.Background(), "a", func(context.Context) (int, error) {
			close(aEntered)
			<-releaseA
			return 1, nil
		})
	}()
	<-aEntered

	go func() {
		defer close(bDone)
		c.GetOrLoad(context.Background(), "b", func(context.Context) (int, error) {
			return 2, nil
		})
	}()
	select {
	case <-bDone:
	case <-time.After(5 * time.Second):
		t.Fatal("load for a different key blocked on the first load")
	}
	close(releaseA)
	<-aDone
	if v := mustGet(t, c, "a"); v != 1 {
		t.Fatalf("got %d", v)
	}
}

// Rule 6: errors are shared with all waiters, nothing is cached, and a later
// call retries.
func TestLoadErrors(t *testing.T) {
	c := New[string, int](Options{})
	boom := errors.New("boom")
	var calls int32

	const n = 4
	// everyCaller is closed once all n goroutines are about to call
	// GetOrLoad; the failing load waits a settle period after that, which is
	// far longer than registering a waiter takes, so all n callers share the
	// one failing load and its error. The load never blocks on the test
	// goroutine, so a slow machine can only make it fail loudly, not hang.
	everyCaller := make(chan struct{})
	settle := 100 * time.Millisecond
	var wg sync.WaitGroup
	load := func(context.Context) (int, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			<-everyCaller
			time.Sleep(settle)
			return 0, boom
		}
		return 9, nil
	}

	errs := make([]error, n)
	var called sync.WaitGroup
	called.Add(n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			called.Done()
			_, errs[i] = c.GetOrLoad(context.Background(), "k", load)
		}(i)
	}
	called.Wait()
	close(everyCaller)
	wg.Wait()
	for i, err := range errs {
		if !errors.Is(err, boom) {
			t.Fatalf("caller %d err = %v, want boom", i, err)
		}
	}
	if n := c.Len(); n != 0 {
		t.Fatalf("Len = %d after failed load, want 0", n)
	}
	v, err := c.GetOrLoad(context.Background(), "k", load)
	if err != nil || v != 9 {
		t.Fatalf("retry = %d, %v", v, err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("load calls = %d, want 2", got)
	}
}

// Rule 7: a caller whose ctx is done returns ctx.Err() promptly while the
// load keeps running for the others.
func TestCallerGivesUp(t *testing.T) {
	c := New[string, int](Options{})
	g := newGate()
	loadDone := make(chan struct{})
	load := func(context.Context) (int, error) {
		g.entered <- struct{}{}
		<-g.release
		close(loadDone)
		return 5, nil
	}

	go c.GetOrLoad(context.Background(), "k", load) // the staying caller
	<-g.entered

	ctx, cancel := context.WithCancel(context.Background())
	goroutineDone := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(ctx, "k", load)
		goroutineDone <- err
	}()
	cancel()

	select {
	case err := <-goroutineDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled caller did not return promptly")
	}
	select {
	case <-loadDone:
		t.Fatal("load stopped because one of several callers gave up")
	default:
	}
	close(g.release)
	select {
	case <-loadDone:
	case <-time.After(5 * time.Second):
		t.Fatal("load did not finish for the remaining caller")
	}
}

// Rule 8: the load's context is cancelled exactly when every waiter is gone,
// not when one of several gives up.
func TestLoadContextCancelledOnlyWhenAllGone(t *testing.T) {
	c := New[string, int](Options{})
	entered := make(chan struct{})
	seen := make(chan error, 1)
	load := func(ctx context.Context) (int, error) {
		close(entered)
		<-ctx.Done()
		seen <- ctx.Err()
		return 0, ctx.Err()
	}

	plainCtx := context.Background()
	plainDone := make(chan struct{})
	go func() {
		defer close(plainDone)
		c.GetOrLoad(plainCtx, "k", load)
	}()
	<-entered

	// A cancelled caller joins and leaves; the load's ctx must stay live
	// while the plain caller remains.
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(ctx, "k", load)
		got <- err
	}()
	cancel()
	select {
	case err := <-got:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled caller did not return")
	}
	select {
	case err := <-seen:
		t.Fatalf("load context cancelled while a waiter remained: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	select {
	case <-plainDone:
		t.Fatal("plain caller returned before the load did")
	default:
	}
}

// Rule 8: the last waiter giving up cancels the load's context.
func TestLoadContextCancelledWhenAllGiveUp(t *testing.T) {
	c := New[string, int](Options{})
	entered := make(chan struct{})
	cancelled := make(chan struct{})
	load := func(ctx context.Context) (int, error) {
		close(entered)
		<-ctx.Done()
		close(cancelled)
		return 0, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(ctx, "k", load)
		errCh <- err
	}()
	<-entered
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("load context was not cancelled after every waiter gave up")
	}
}

// Rule 9: a successful late result is stored even when every caller gave up,
// with its TTL starting when load returned.
func TestLateResultIsStored(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{TTL: time.Minute, Now: clk.Now})
	entered := make(chan struct{})
	proceed := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(ctx, "k", func(context.Context) (int, error) {
			close(entered)
			<-proceed
			return 11, nil
		})
		errCh <- err
	}()
	<-entered
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	clk.Advance(10 * time.Minute) // long before the load returns
	close(proceed)

	deadline := time.Now().Add(5 * time.Second)
	for {
		if v, ok := c.Get("k"); ok {
			if v != 11 {
				t.Fatalf("got %d", v)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("late successful result was not stored")
		}
		time.Sleep(time.Millisecond)
	}
	clk.Advance(time.Minute - time.Nanosecond)
	mustGet(t, c, "k")
	clk.Advance(time.Nanosecond)
	if _, ok := c.Get("k"); ok {
		t.Fatal("late result TTL did not start when load returned")
	}
}

// Rule 10: Delete removes the key but does not stop an in-flight load.
func TestDeleteDuringLoadStillStores(t *testing.T) {
	c := New[string, int](Options{})
	g := newGate()
	go c.GetOrLoad(context.Background(), "k", func(context.Context) (int, error) {
		g.entered <- struct{}{}
		<-g.release
		return 3, nil
	})
	<-g.entered
	c.Delete("k")
	if n := c.Len(); n != 0 {
		t.Fatalf("Len = %d after Delete, want 0", n)
	}
	close(g.release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if v, ok := c.Get("k"); ok {
			if v != 3 {
				t.Fatalf("got %d", v)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("in-flight load's result was not stored after Delete")
		}
		time.Sleep(time.Millisecond)
	}
}

// Rule 10 (plain): Delete removes a live entry; Get misses afterwards.
func TestDelete(t *testing.T) {
	c := New[string, int](Options{})
	c.Set("k", 1)
	c.Delete("k")
	if _, ok := c.Get("k"); ok {
		t.Fatal("deleted key returned")
	}
	c.Delete("absent") // must not panic
	if n := c.Len(); n != 0 {
		t.Fatalf("Len = %d", n)
	}
}

// Rule 11: concurrent mixed use is race-free and consistent.
func TestConcurrentUse(t *testing.T) {
	c := New[int, int](Options{TTL: time.Minute, MaxEntries: 16})
	var loads int32
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				k := (g*7 + i) % 40
				v, err := c.GetOrLoad(context.Background(), k, func(context.Context) (int, error) {
					atomic.AddInt32(&loads, 1)
					return k * 10, nil
				})
				if err != nil {
					t.Errorf("GetOrLoad: %v", err)
					return
				}
				if v != k*10 {
					t.Errorf("GetOrLoad(%d) = %d", k, v)
					return
				}
				c.Set(k, k*10)
				c.Get(k)
				c.Len()
				if i%17 == 0 {
					c.Delete(k)
				}
			}
		}(g)
	}
	wg.Wait()
	if n := c.Len(); n > 16 {
		t.Fatalf("Len = %d exceeds MaxEntries", n)
	}
}

// Rule 11: no goroutine the cache starts outlives the loads it serves.
func TestNoGoroutineOutlivesLoad(t *testing.T) {
	c := New[string, int](Options{})
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c.GetOrLoad(context.Background(), fmt.Sprint(i%5), func(context.Context) (int, error) {
				return i, nil
			})
		}(i)
	}
	before := testGoroutines() // after every test goroutine exists
	wg.Wait()
	// Allow a moment for goroutines that have already returned from their
	// loads to be reaped by the runtime.
	time.Sleep(10 * time.Millisecond)
	if n := testGoroutines(); n > before {
		t.Fatalf("goroutines: %d before, %d after; cache goroutines outlived their loads", before, n)
	}
}
