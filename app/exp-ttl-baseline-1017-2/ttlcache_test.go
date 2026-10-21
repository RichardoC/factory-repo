package ttlcache

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a controllable clock for Options.Now.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Unix(1_700_000_000, 0)} }

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

// mustGet is a test helper: Get(key) must be a hit.
func mustGet[K comparable, V any](t *testing.T, c *Cache[K, V], key K) V {
	t.Helper()
	v, ok := c.Get(key)
	if !ok {
		t.Fatalf("expected hit for %v, got miss", key)
	}
	return v
}

// Rule 1: expiry — an entry stored at t is live while Now() < t+TTL.
func TestExpiry(t *testing.T) {
	fc := newFakeClock()
	c := New[int, string](Options{TTL: 5 * time.Second, Now: fc.Now})
	c.Set(1, "a")
	if _, ok := c.Get(1); !ok {
		t.Fatal("entry should be live right after Set")
	}
	fc.advance(4999 * time.Millisecond)
	if v := mustGet(t, c, 1); v != "a" {
		t.Fatal("entry should still be live just before TTL")
	}
	fc.advance(1 * time.Millisecond) // Now() == t+TTL: no longer live
	if _, ok := c.Get(1); ok {
		t.Fatal("entry should be expired at exactly t+TTL")
	}
}

// Rule 1: TTL 0 means entries never expire.
func TestNoTTLNeverExpires(t *testing.T) {
	fc := newFakeClock()
	c := New[int, string](Options{Now: fc.Now})
	c.Set(1, "a")
	fc.advance(100 * time.Hour)
	if v := mustGet(t, c, 1); v != "a" {
		t.Fatal("entry with TTL 0 must never expire")
	}
}

// Rule 1: Len counts only live entries.
func TestLenCountsOnlyLive(t *testing.T) {
	fc := newFakeClock()
	c := New[int, string](Options{TTL: 10 * time.Second, Now: fc.Now})
	c.Set(1, "a")
	c.Set(2, "b")
	if got := c.Len(); got != 2 {
		t.Fatalf("Len = %d, want 2", got)
	}
	fc.advance(10 * time.Second)
	if got := c.Len(); got != 0 {
		t.Fatalf("Len after expiry = %d, want 0", got)
	}
	if _, ok := c.Get(1); ok {
		t.Fatal("Get must never return an expired entry")
	}
}

// Rule 2: Set replaces the value and restarts the TTL.
func TestSetReplacesAndRestartsTTL(t *testing.T) {
	fc := newFakeClock()
	c := New[int, string](Options{TTL: 10 * time.Second, Now: fc.Now})
	c.Set(1, "a")
	fc.advance(8 * time.Second)
	c.Set(1, "b") // restarts the TTL from here
	fc.advance(8 * time.Second)
	if v := mustGet(t, c, 1); v != "b" {
		t.Fatalf("Get = %q, want replaced value %q", v, "b")
	}
	fc.advance(2 * time.Second) // 10s after the replacement: expired
	if _, ok := c.Get(1); ok {
		t.Fatal("TTL should have restarted at the replacement Set")
	}
}

// Rule 3: LRU eviction of the least recently used live entry.
func TestLRUEviction(t *testing.T) {
	fc := newFakeClock()
	c := New[int, string](Options{MaxEntries: 2, Now: fc.Now})
	c.Set(1, "a")
	c.Set(2, "b")
	c.Get(1)      // 1 is now most recently used
	c.Set(3, "c") // over the limit: 2 (LRU) must go
	if _, ok := c.Get(2); ok {
		t.Fatal("key 2 (least recently used) should have been evicted")
	}
	if _, ok := c.Get(1); !ok {
		t.Fatal("key 1 was recently used and must survive")
	}
	if _, ok := c.Get(3); !ok {
		t.Fatal("key 3 was just stored and must survive")
	}
	if got := c.Len(); got != 2 {
		t.Fatalf("Len = %d, want 2", got)
	}
}

// Rule 3: a stored or hit entry counts as used; a completed load too
// (covered in TestGetOrLoadStoresAfterLoad below).
func TestLoadCountsAsUse(t *testing.T) {
	fc := newFakeClock()
	c := New[int, string](Options{MaxEntries: 2, Now: fc.Now})
	ctx := context.Background()
	if _, err := c.GetOrLoad(ctx, 1, func(context.Context) (string, error) { return "a", nil }); err != nil {
		t.Fatal(err)
	}
	c.Set(2, "b")
	c.Get(1)      // refresh 1
	c.Set(3, "c") // evicts 2
	if _, ok := c.Get(2); ok {
		t.Fatal("key 2 should have been evicted as LRU")
	}
}

// Rule 3: expired entries never cause a live entry to be evicted.
func TestExpiredEntriesDoNotEvictLive(t *testing.T) {
	fc := newFakeClock()
	c := New[int, string](Options{TTL: 10 * time.Second, MaxEntries: 2, Now: fc.Now})
	c.Set(1, "a") // expires at t+10
	fc.advance(1 * time.Second)
	c.Set(2, "b")               // expires at t+11
	fc.advance(9 * time.Second) // 1 is now expired, 2 still live
	c.Set(3, "c")               // live entries: 2 and 3, within the limit
	if _, ok := c.Get(2); !ok {
		t.Fatal("live key 2 must not be evicted by an expired entry")
	}
	if _, ok := c.Get(3); !ok {
		t.Fatal("live key 3 must not be evicted by an expired entry")
	}
	if got := c.Len(); got != 2 {
		t.Fatalf("Len = %d, want 2", got)
	}
}

// Rule 4: GetOrLoad on a live hit returns the cached value without loading.
func TestGetOrLoadHitDoesNotLoad(t *testing.T) {
	c := New[int, string](Options{})
	c.Set(1, "cached")
	var loads atomic.Int32
	v, err := c.GetOrLoad(context.Background(), 1, func(context.Context) (string, error) {
		loads.Add(1)
		return "loaded", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if v != "cached" || loads.Load() != 0 {
		t.Fatalf("hit returned %q with %d loads, want cached value and 0 loads", v, loads.Load())
	}
}

// Rule 5: concurrent GetOrLoad callers for one key share a single load.
func TestSingleFlightSameKey(t *testing.T) {
	c := New[int, string](Options{})
	release := make(chan struct{})
	var loads atomic.Int32
	var wg sync.WaitGroup
	results := make([]string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, err := c.GetOrLoad(context.Background(), 1, func(context.Context) (string, error) {
				loads.Add(1)
				<-release
				return "v", nil
			})
			if err != nil {
				t.Error(err)
			}
			results[i] = v
		}(i)
	}
	// Give all callers time to pile up on the in-flight load.
	deadline := time.Now().Add(2 * time.Second)
	for c.Len() == 0 && loads.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	if got := loads.Load(); got != 1 {
		t.Fatalf("loads so far = %d, want 1 (single flight)", got)
	}
	close(release)
	wg.Wait()
	if got := loads.Load(); got != 1 {
		t.Fatalf("total loads = %d, want 1", got)
	}
	for i, v := range results {
		if v != "v" {
			t.Fatalf("caller %d got %q, want %q", i, v, "v")
		}
	}
}

// Rule 5: different keys load independently, without blocking each other.
func TestDifferentKeysLoadIndependently(t *testing.T) {
	c := New[int, string](Options{})
	var started atomic.Int32
	barrier := make(chan struct{})
	var wg sync.WaitGroup
	for key := 1; key <= 4; key++ {
		wg.Add(1)
		go func(key int) {
			defer wg.Done()
			_, err := c.GetOrLoad(context.Background(), key, func(context.Context) (string, error) {
				if started.Add(1) == 4 {
					close(barrier) // all four loads running at once
				}
				<-barrier
				return "v", nil
			})
			if err != nil {
				t.Error(err)
			}
		}(key)
	}
	wg.Wait()
	if started.Load() != 4 {
		t.Fatalf("concurrent loads started = %d, want 4", started.Load())
	}
}

// Rule 6: load errors are not cached and reach every waiter; the next
// GetOrLoad calls load again.
func TestLoadErrorNotCachedAndRetried(t *testing.T) {
	c := New[int, string](Options{})
	boom := errors.New("boom")
	var loads atomic.Int32
	release := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = c.GetOrLoad(context.Background(), 1, func(context.Context) (string, error) {
				loads.Add(1)
				<-release
				return "", boom
			})
		}(i)
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	for i, err := range errs {
		if !errors.Is(err, boom) {
			t.Fatalf("waiter %d got %v, want the load's error", i, err)
		}
	}
	if got := c.Len(); got != 0 {
		t.Fatalf("Len after failed load = %d, want 0 (nothing cached)", got)
	}
	// A later GetOrLoad calls load again.
	v, err := c.GetOrLoad(context.Background(), 1, func(context.Context) (string, error) {
		return "second", nil
	})
	if err != nil || v != "second" {
		t.Fatalf("retry got (%q, %v), want (%q, nil)", v, err, "second")
	}
}

// Rule 7: a caller whose ctx is done returns ctx.Err() promptly, and the
// load keeps running for other waiting callers.
func TestCallerGivesUp(t *testing.T) {
	c := New[int, string](Options{})
	release := make(chan struct{})
	var loads atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	gaveUp := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // the waiter that stays
		defer wg.Done()
		v, err := c.GetOrLoad(context.Background(), 1, func(context.Context) (string, error) {
			loads.Add(1)
			<-release
			return "v", nil
		})
		if err != nil || v != "v" {
			t.Errorf("remaining waiter got (%q, %v), want (%q, nil)", v, err, "v")
		}
	}()
	go func() {
		_, err := c.GetOrLoad(ctx, 1, func(context.Context) (string, error) { // shares the load above
			loads.Add(1)
			<-release
			return "v", nil
		})
		gaveUp <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-gaveUp; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled caller got %v, want context.Canceled", err)
	}
	close(release)
	wg.Wait()
	if got := loads.Load(); got != 1 {
		t.Fatalf("total loads = %d, want 1 (shared load kept running)", got)
	}
}

// Rule 8: the load's context is cancelled exactly when every waiting caller
// has given up — not while a waiter remains, not because one of several did.
func TestLoadContextCancelledOnlyWhenAllGiveUp(t *testing.T) {
	c := New[int, string](Options{})
	loadCtxDone := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup
	load := func(ctx context.Context) (string, error) {
		go func() {
			<-ctx.Done()
			close(loadCtxDone)
		}()
		<-release
		return "v", nil
	}
	ctx1, cancel1 := context.WithCancel(context.Background())
	ctx2, cancel2 := context.WithCancel(context.Background())
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = c.GetOrLoad(ctx1, 1, load) }()
	go func() { defer wg.Done(); _, _ = c.GetOrLoad(ctx2, 1, load) }()
	time.Sleep(50 * time.Millisecond)
	cancel1() // one of two callers gives up
	select {
	case <-loadCtxDone:
		t.Fatal("load context cancelled while a waiter remained")
	case <-time.After(50 * time.Millisecond):
	}
	cancel2() // the last waiter gives up
	select {
	case <-loadCtxDone:
	case <-time.After(2 * time.Second):
		t.Fatal("load context not cancelled after every caller gave up")
	}
	close(release)
	wg.Wait()
}

// Rule 9: a successful result is stored even if every caller gave up, with
// its TTL starting when load returned.
func TestLateResultStored(t *testing.T) {
	fc := newFakeClock()
	c := New[int, string](Options{TTL: 10 * time.Second, Now: fc.Now})
	release := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(ctx, 1, func(context.Context) (string, error) {
			<-release
			return "late", nil
		})
		errCh <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("caller got %v, want context.Canceled", err)
	}
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := c.Get(1); ok {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if v := mustGet(t, c, 1); v != "late" {
		t.Fatalf("late result not stored: got %q", v)
	}
	// TTL starts when load returned, i.e. after release, not at GetOrLoad.
	fc.advance(9 * time.Second)
	if _, ok := c.Get(1); !ok {
		t.Fatal("entry should still be live 9s after load returned")
	}
	fc.advance(1 * time.Second)
	if _, ok := c.Get(1); ok {
		t.Fatal("entry should expire 10s after load returned")
	}
}

// Rule 10: Delete removes the key but does not stop an in-flight load.
func TestDeleteDoesNotStopInFlightLoad(t *testing.T) {
	fc := newFakeClock()
	c := New[int, string](Options{TTL: 10 * time.Second, Now: fc.Now})
	release := make(chan struct{})
	loadStarted := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(context.Background(), 1, func(context.Context) (string, error) {
			close(loadStarted)
			<-release
			return "fresh", nil
		})
		errCh <- err
	}()
	<-loadStarted // the load for key 1 is in flight
	c.Set(1, "old")
	c.Delete(1)
	if _, ok := c.Get(1); ok {
		t.Fatal("Delete should remove the key")
	}
	close(release)
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	if v := mustGet(t, c, 1); v != "fresh" {
		t.Fatalf("after Delete + in-flight load, Get = %q, want %q (load result still stored)", v, "fresh")
	}
}

// Rule 11: stress the cache under concurrency; run with -race.
func TestConcurrentStress(t *testing.T) {
	fc := newFakeClock()
	c := New[int, int](Options{TTL: time.Millisecond, MaxEntries: 32, Now: fc.Now})
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
				key := (w*7 + i) % 64
				switch i % 4 {
				case 0:
					c.Set(key, i)
				case 1:
					_, _ = c.Get(key)
				case 2:
					_, _ = c.GetOrLoad(context.Background(), key, func(context.Context) (int, error) {
						return i, nil
					})
				case 3:
					c.Delete(key)
					_ = c.Len()
				}
				if i%16 == 0 {
					fc.advance(500 * time.Microsecond)
				}
			}
		}(w)
	}
	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// Rule 11: no goroutine the cache starts outlives the loads it serves.
func TestNoGoroutineLeak(t *testing.T) {
	defer func() {
		time.Sleep(50 * time.Millisecond)
		runtime.GC()
	}()
	before := runtime.NumGoroutine()
	for i := 0; i < 20; i++ {
		c := New[int, string](Options{})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			_, _ = c.GetOrLoad(ctx, 1, func(context.Context) (string, error) {
				return "v", nil
			})
			close(done)
		}()
		<-done
		cancel()
	}
	time.Sleep(100 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before+2 {
		t.Fatalf("goroutines before = %d, after = %d (leak)", before, after)
	}
}
