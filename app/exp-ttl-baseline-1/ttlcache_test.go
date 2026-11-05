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

// fakeClock is a deterministic clock controlled by the test.
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
	if v, ok := c.Get(key); ok {
		t.Fatalf("Get(%v): expected miss, got %v", key, v)
	}
}

// --- Rule 1: expiry ---

func TestExpiryTTL(t *testing.T) {
	fc := newFakeClock()
	c := New[string, int](Options{TTL: 10 * time.Second, Now: fc.Now})
	c.Set("a", 1)
	if got := mustGet(t, c, "a"); got != 1 {
		t.Fatalf("got %d", got)
	}
	fc.Advance(10*time.Second - time.Nanosecond) // still before t+TTL
	if got := mustGet(t, c, "a"); got != 1 {
		t.Fatalf("entry expired too early: got %d", got)
	}
	fc.Advance(time.Nanosecond) // Now() == t+TTL: no longer live
	mustMiss(t, c, "a")
	if c.Len() != 0 {
		t.Fatalf("Len = %d after expiry, want 0", c.Len())
	}
}

func TestExpiryZeroTTLNeverExpires(t *testing.T) {
	fc := newFakeClock()
	c := New[string, int](Options{Now: fc.Now})
	c.Set("a", 1)
	fc.Advance(100 * 365 * 24 * time.Hour)
	if got := mustGet(t, c, "a"); got != 1 {
		t.Fatalf("entry with TTL 0 expired: got %d", got)
	}
	if c.Len() != 1 {
		t.Fatalf("Len = %d, want 1", c.Len())
	}
}

func TestLenCountsOnlyLiveEntries(t *testing.T) {
	fc := newFakeClock()
	c := New[int, string](Options{TTL: 10 * time.Second, Now: fc.Now})
	for i := 0; i < 5; i++ {
		c.Set(i, fmt.Sprint(i))
	}
	fc.Advance(11 * time.Second)
	for i := 0; i < 5; i++ {
		c.Set(100+i, "live")
	}
	if got := c.Len(); got != 5 {
		t.Fatalf("Len = %d, want 5 (only live entries)", got)
	}
}

// --- Rule 2: replacement ---

func TestSetReplacesValueAndRestartsTTL(t *testing.T) {
	fc := newFakeClock()
	c := New[string, string](Options{TTL: 10 * time.Second, Now: fc.Now})
	c.Set("k", "old")
	fc.Advance(8 * time.Second) // close to expiry
	c.Set("k", "new")
	if got := mustGet(t, c, "k"); got != "new" {
		t.Fatalf("value not replaced: got %q", got)
	}
	// The TTL restarted at the second Set (t=8s), so the entry is still
	// live at t=17s even though the first Set was at t=0.
	fc.Advance(9 * time.Second)
	if got := mustGet(t, c, "k"); got != "new" {
		t.Fatalf("TTL not restarted: entry expired at t=17s")
	}
	fc.Advance(time.Second + time.Nanosecond) // t=18s > 8s+10s
	mustMiss(t, c, "k")
}

// --- Rule 3: LRU eviction ---

func TestLRUEviction(t *testing.T) {
	fc := newFakeClock()
	c := New[string, int](Options{MaxEntries: 2, Now: fc.Now})
	c.Set("a", 1)
	c.Set("b", 2)
	mustGet(t, c, "a") // a is now more recent than b
	c.Set("c", 3)      // must evict b, the LRU live entry
	mustMiss(t, c, "b")
	if got := mustGet(t, c, "a"); got != 1 {
		t.Fatalf("a was evicted")
	}
	if got := mustGet(t, c, "c"); got != 3 {
		t.Fatalf("c was evicted")
	}
	if c.Len() != 2 {
		t.Fatalf("Len = %d, want 2", c.Len())
	}
}

func TestLRUSetCountsAsUse(t *testing.T) {
	fc := newFakeClock()
	c := New[string, int](Options{MaxEntries: 2, Now: fc.Now})
	c.Set("a", 1)
	c.Set("b", 2)
	c.Set("a", 10) // re-Storing a makes it the most recently used
	c.Set("c", 3)  // evicts b
	mustMiss(t, c, "b")
	mustGet(t, c, "a")
	mustGet(t, c, "c")
}

func TestGetOrLoadHitCountsAsUse(t *testing.T) {
	fc := newFakeClock()
	c := New[string, int](Options{MaxEntries: 2, Now: fc.Now})
	c.Set("a", 1)
	c.Set("b", 2)
	if _, err := c.GetOrLoad(context.Background(), "a", func(ctx context.Context) (int, error) {
		return 99, nil
	}); err != nil {
		t.Fatal(err)
	}
	c.Set("c", 3) // evicts b (a was just used by the GetOrLoad hit)
	mustMiss(t, c, "b")
	mustGet(t, c, "a")
	mustGet(t, c, "c")
}

func TestExpiredEntriesNeverEvictLiveOnes(t *testing.T) {
	fc := newFakeClock()
	c := New[int, int](Options{TTL: 10 * time.Second, MaxEntries: 2, Now: fc.Now})
	c.Set(1, 1) // expires at t=10
	fc.Advance(11 * time.Second)
	c.Set(2, 2) // live, expires at t=21
	c.Set(3, 3) // over cap: entry 1 is expired, so it must go, not entry 2
	mustGet(t, c, 2)
	mustGet(t, c, 3)
	mustMiss(t, c, 1)
	if c.Len() != 2 {
		t.Fatalf("Len = %d, want 2", c.Len())
	}
}

func TestExpiryAloneDoesNotEvictWhenUnderCap(t *testing.T) {
	fc := newFakeClock()
	c := New[int, int](Options{TTL: 5 * time.Second, MaxEntries: 10, Now: fc.Now})
	c.Set(1, 1)
	fc.Advance(6 * time.Second)
	c.Set(2, 2)
	mustGet(t, c, 2)
	mustMiss(t, c, 1)
	if c.Len() != 1 {
		t.Fatalf("Len = %d, want 1", c.Len())
	}
}

// --- Rule 4: hits do not load ---

func TestGetOrLoadHitDoesNotCallLoad(t *testing.T) {
	c := New[string, int](Options{})
	c.Set("k", 7)
	var loads int32
	v, err := c.GetOrLoad(context.Background(), "k", func(ctx context.Context) (int, error) {
		atomic.AddInt32(&loads, 1)
		return 42, nil
	})
	if err != nil || v != 7 {
		t.Fatalf("GetOrLoad = %v, %v; want 7, nil", v, err)
	}
	if got := atomic.LoadInt32(&loads); got != 0 {
		t.Fatalf("load called %d times on a live hit", got)
	}
}

// --- Rule 5: one load per key, independent keys ---

func TestSingleFlightSameKey(t *testing.T) {
	c := New[string, int](Options{})
	var loads int32
	release := make(chan struct{})
	loadDone := make(chan int, 64)
	for i := 0; i < 50; i++ {
		go func() {
			v, err := c.GetOrLoad(context.Background(), "k", func(ctx context.Context) (int, error) {
				atomic.AddInt32(&loads, 1)
				<-release
				return 5, nil
			})
			if err == nil {
				loadDone <- v
			} else {
				loadDone <- -1
			}
		}()
	}
	// Give every goroutine time to reach the in-flight load; the load is
	// blocked on release, so any caller arriving in this window shares it.
	time.Sleep(200 * time.Millisecond)
	close(release)
	shared, missed := 0, 0
	for i := 0; i < 50; i++ {
		if v := <-loadDone; v == 5 {
			shared++
		} else {
			missed++
		}
	}
	if missed != 0 {
		t.Fatalf("%d callers did not get the shared value", missed)
	}
	if got := atomic.LoadInt32(&loads); got != 1 {
		t.Fatalf("load ran %d times, want exactly 1", got)
	}
	_ = shared
}

func TestDifferentKeysLoadIndependently(t *testing.T) {
	c := New[string, int](Options{})
	var started int32
	bothStarted := make(chan struct{})
	start := func() {
		if atomic.AddInt32(&started, 1) == 2 {
			close(bothStarted)
		}
	}
	var wg sync.WaitGroup
	results := make([]int, 2)
	for i, key := range []string{"a", "b"} {
		wg.Add(1)
		go func(i int, key string) {
			defer wg.Done()
			v, err := c.GetOrLoad(context.Background(), key, func(ctx context.Context) (int, error) {
				start() // if loads serialized on one lock, this hangs
				time.Sleep(50 * time.Millisecond)
				return i + 10, nil
			})
			if err == nil {
				results[i] = v
			} else {
				results[i] = -1
			}
		}(i, key)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("loads for different keys blocked each other")
	}
	if !resultsEqual(results, []int{10, 11}) {
		t.Fatalf("results = %v, want [10 11]", results)
	}
}

func resultsEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- Rule 6: load errors ---

func TestLoadErrorNotCachedAndRetried(t *testing.T) {
	c := New[string, int](Options{})
	var loads int32
	_, err := c.GetOrLoad(context.Background(), "k", func(ctx context.Context) (int, error) {
		atomic.AddInt32(&loads, 1)
		return 0, errors.New("boom")
	})
	if err == nil || err.Error() != "boom" {
		t.Fatalf("err = %v, want boom", err)
	}
	mustMiss(t, c, "k")
	if c.Len() != 0 {
		t.Fatalf("Len = %d after a failed load, want 0", c.Len())
	}
	// A later GetOrLoad calls load again.
	v, err := c.GetOrLoad(context.Background(), "k", func(ctx context.Context) (int, error) {
		atomic.AddInt32(&loads, 1)
		return 9, nil
	})
	if err != nil || v != 9 {
		t.Fatalf("retry: GetOrLoad = %v, %v; want 9, nil", v, err)
	}
	if got := atomic.LoadInt32(&loads); got != 2 {
		t.Fatalf("loads = %d, want 2", got)
	}
}

func TestAllWaitersGetSameLoadError(t *testing.T) {
	c := New[string, int](Options{})
	release := make(chan struct{})
	boom := errors.New("boom")
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = c.GetOrLoad(context.Background(), "k", func(ctx context.Context) (int, error) {
				<-release
				return 0, boom
			})
		}(i)
	}
	time.Sleep(200 * time.Millisecond) // let all callers join the load
	close(release)
	wg.Wait()
	for i, err := range errs {
		if !errors.Is(err, boom) {
			t.Fatalf("caller %d: err = %v, want the shared boom error", i, err)
		}
	}
}

// --- Rule 7: callers that give up ---

func TestCallerGivesUpButLoadServesOtherWaiters(t *testing.T) {
	c := New[string, int](Options{})
	started := make(chan struct{})
	var startedOnce sync.Once
	release := make(chan struct{})
	var loadCancelled atomic.Bool

	loadFn := func(ctx context.Context) (int, error) {
		startedOnce.Do(func() { close(started) })
		select {
		case <-ctx.Done():
			loadCancelled.Store(true)
			return 0, ctx.Err()
		case <-release:
			return 7, nil
		}
	}

	// Caller A is the initiator: it is registered as a waiter before the
	// load even starts, so a waiter provably remains throughout.
	var wgA sync.WaitGroup
	wgA.Add(1)
	var vA int
	var errA error
	go func() {
		defer wgA.Done()
		vA, errA = c.GetOrLoad(context.Background(), "k", loadFn)
	}()
	<-started

	// Caller B joins the running load, then gives up.
	ctxB, cancelB := context.WithCancel(context.Background())
	var wgB sync.WaitGroup
	wgB.Add(1)
	var errB error
	go func() {
		defer wgB.Done()
		_, errB = c.GetOrLoad(ctxB, "k", loadFn)
	}()
	time.Sleep(100 * time.Millisecond) // let B join (if it has not, its cancel is still harmless)
	cancelB()
	wgB.Wait()
	if errB != nil && !errors.Is(errB, context.Canceled) {
		t.Fatalf("giving-up caller err = %v, want context.Canceled or nil", errB)
	}

	// The load keeps running for A and is not cancelled.
	close(release)
	wgA.Wait()
	if errA != nil || vA != 7 {
		t.Fatalf("remaining waiter got %d, %v; want 7, nil", vA, errA)
	}
	if loadCancelled.Load() {
		t.Fatal("load's context was cancelled while waiter A remained")
	}
	if got := mustGet(t, c, "k"); got != 7 {
		t.Fatalf("successful load was not cached: got %d", got)
	}
}

// --- Rule 8: the load's context ---

func TestLoadContextCancelledWhenEveryCallerGivesUp(t *testing.T) {
	c := New[string, int](Options{})
	started := make(chan struct{})
	var loadCancelled atomic.Bool
	var loadReturned sync.WaitGroup
	loadReturned.Add(1)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	var err1 error
	go func() {
		defer wg.Done()
		_, err1 = c.GetOrLoad(ctx, "k", func(loadCtx context.Context) (int, error) {
			close(started)
			<-loadCtx.Done()
			loadCancelled.Store(true)
			loadReturned.Done()
			return 0, loadCtx.Err()
		})
	}()
	<-started
	cancel()
	wg.Wait()
	if !errors.Is(err1, context.Canceled) {
		t.Fatalf("caller err = %v, want context.Canceled", err1)
	}
	loadReturned.Wait() // the load observed its context being cancelled
	if !loadCancelled.Load() {
		t.Fatal("load's context was not cancelled after every caller gave up")
	}
	// The load returned an error, so nothing was cached and a later
	// GetOrLoad starts a fresh load.
	mustMiss(t, c, "k")
	v, err := c.GetOrLoad(context.Background(), "k", func(ctx context.Context) (int, error) {
		return 3, nil
	})
	if err != nil || v != 3 {
		t.Fatalf("fresh load after cancellation: %d, %v", v, err)
	}
}

func TestCancelledCallerDoesNotStartLoad(t *testing.T) {
	c := New[string, int](Options{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var loads int32
	_, err := c.GetOrLoad(ctx, "k", func(ctx context.Context) (int, error) {
		atomic.AddInt32(&loads, 1)
		return 1, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got := atomic.LoadInt32(&loads); got != 0 {
		t.Fatalf("load started for an already-cancelled caller")
	}
}

// --- Rule 9: late results ---

func TestLateResultStoredAfterAllCallersGiveUp(t *testing.T) {
	fc := newFakeClock()
	c := New[string, int](Options{TTL: 10 * time.Second, Now: fc.Now})
	fc.Advance(100 * time.Second) // t = 100s

	started := make(chan struct{})
	release := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	var err1 error
	go func() {
		defer wg.Done()
		_, err1 = c.GetOrLoad(ctx, "k", func(loadCtx context.Context) (int, error) {
			close(started)
			<-release // ignores loadCtx: a load that outlives its callers
			return 42, nil
		})
	}()
	<-started
	cancel()
	wg.Wait()
	if !errors.Is(err1, context.Canceled) {
		t.Fatalf("caller err = %v, want context.Canceled", err1)
	}

	// The load has not returned yet: nothing cached.
	mustMiss(t, c, "k")

	// The load returns late and successfully: the value is stored, with
	// its TTL starting when load returned (t = 100s, so expiry at 110s).
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for c.Len() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("late successful load result was never stored")
		}
		time.Sleep(time.Millisecond)
	}
	if got := mustGet(t, c, "k"); got != 42 {
		t.Fatalf("cached late result = %d, want 42", got)
	}
	fc.Advance(9 * time.Second)
	if got := mustGet(t, c, "k"); got != 42 {
		t.Fatalf("late result expired too early")
	}
	fc.Advance(time.Second + time.Nanosecond) // past 100s + 10s
	mustMiss(t, c, "k")
}

// --- Rule 10: Delete ---

func TestDeleteRemovesKey(t *testing.T) {
	c := New[string, int](Options{})
	c.Set("a", 1)
	c.Set("b", 2)
	c.Delete("a")
	mustMiss(t, c, "a")
	if got := mustGet(t, c, "b"); got != 2 {
		t.Fatalf("b affected by Delete(a)")
	}
	if c.Len() != 1 {
		t.Fatalf("Len = %d, want 1", c.Len())
	}
	c.Delete("missing") // no panic, no effect
}

func TestDeleteDoesNotStopInflightLoad(t *testing.T) {
	c := New[string, int](Options{})
	started := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		v, err := c.GetOrLoad(context.Background(), "k", func(ctx context.Context) (int, error) {
			close(started)
			<-release
			return 11, nil
		})
		if err != nil || v != 11 {
			t.Errorf("GetOrLoad after Delete = %d, %v; want 11, nil", v, err)
		}
	}()
	<-started
	c.Delete("k") // must not stop the load
	mustMiss(t, c, "k")
	close(release)
	wg.Wait()
	if got := mustGet(t, c, "k"); got != 11 {
		t.Fatalf("in-flight load result was not stored after Delete: got %d", got)
	}
}

// --- Rule 11: concurrency and goroutine lifetime ---

func TestConcurrentStress(t *testing.T) {
	fc := newFakeClock()
	c := New[int, int](Options{TTL: time.Millisecond, MaxEntries: 64, Now: fc.Now})
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 400; i++ {
				key := (g*7 + i) % 97
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
					// A keyspace Set never touches, so a hit or a
					// fresh load must both yield the key itself.
					loadKey := 1000 + key
					ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
					v, err := c.GetOrLoad(ctx, loadKey, func(ctx context.Context) (int, error) {
						time.Sleep(time.Millisecond)
						return loadKey, nil
					})
					if err == nil && v != loadKey {
						t.Errorf("GetOrLoad = %d, want %d", v, loadKey)
					}
					cancel()
				}
				fc.Advance(100 * time.Microsecond)
			}
		}(g)
	}
	wg.Wait()
}

func TestNoGoroutineOutlivesLoads(t *testing.T) {
	c := New[string, int](Options{})
	baseline := runtime.NumGoroutine()

	for round := 0; round < 10; round++ {
		// Loads whose callers all give up: the load goroutine must end
		// once the (ctx-respecting) load returns.
		var wg sync.WaitGroup
		for i := 0; i < 5; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithCancel(context.Background())
				go func() {
					time.Sleep(10 * time.Millisecond)
					cancel()
				}()
				c.GetOrLoad(ctx, fmt.Sprint(round), func(ctx context.Context) (int, error) {
					<-ctx.Done()
					return 0, ctx.Err()
				})
			}()
		}
		wg.Wait()
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+1 {
		if time.Now().After(deadline) {
			t.Fatalf("goroutines did not settle: baseline %d, now %d", baseline, runtime.NumGoroutine())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// --- boundary: MaxEntries 1 and generic types ---

func TestMaxEntriesOne(t *testing.T) {
	fc := newFakeClock()
	c := New[int, string](Options{MaxEntries: 1, Now: fc.Now})
	c.Set(1, "a")
	c.Set(2, "b")
	mustMiss(t, c, 1)
	if got := mustGet(t, c, 2); got != "b" {
		t.Fatalf("got %q", got)
	}
}

func TestStructKeysAndValues(t *testing.T) {
	type key struct {
		ns string
		id int
	}
	type val struct{ name string }
	c := New[key, val](Options{})
	k := key{ns: "default", id: 3}
	c.Set(k, val{name: "pod"})
	if got := mustGet(t, c, k); got.name != "pod" {
		t.Fatalf("got %+v", got)
	}
}
