package ttlcache

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a controllable clock for Options.Now.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(1_700_000_000, 0)} }

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

// Rule 1: expiry.
func TestExpiry(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{TTL: 10 * time.Second, Now: clk.Now})

	c.Set("a", 1)
	if v, ok := c.Get("a"); !ok || v != 1 {
		t.Fatalf("before TTL: got (%v, %v), want (1, true)", v, ok)
	}
	// An entry is live while Now() is strictly before t+TTL.
	clk.Advance(10*time.Second - time.Nanosecond)
	if v, ok := c.Get("a"); !ok || v != 1 {
		t.Fatalf("just before expiry: got (%v, %v), want (1, true)", v, ok)
	}
	clk.Advance(time.Nanosecond)
	if _, ok := c.Get("a"); ok {
		t.Fatal("entry live at exactly t+TTL; want expired")
	}

	// Get never returns an expired entry...
	c.Set("b", 2)
	clk.Advance(20 * time.Second)
	if _, ok := c.Get("b"); ok {
		t.Fatal("Get returned expired entry")
	}
	// ...and Len counts only live entries.
	c.Set("c", 3)
	if got := c.Len(); got != 1 {
		t.Fatalf("Len = %d, want 1 (only live entries)", got)
	}

	// TTL 0: entries never expire.
	c0 := New[string, int](Options{Now: clk.Now})
	c0.Set("x", 9)
	clk.Advance(1000 * time.Hour)
	if v, ok := c0.Get("x"); !ok || v != 9 {
		t.Fatalf("TTL 0 should never expire: got (%v, %v)", v, ok)
	}
	if got := c0.Len(); got != 1 {
		t.Fatalf("TTL 0 Len = %d, want 1", got)
	}
}

// Rule 2: Set on an existing key replaces the value and restarts its TTL.
func TestSetReplaces(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{TTL: 10 * time.Second, Now: clk.Now})

	c.Set("a", 1)
	clk.Advance(6 * time.Second)
	c.Set("a", 2)
	if v, ok := c.Get("a"); !ok || v != 2 {
		t.Fatalf("replaced value: got (%v, %v), want (2, true)", v, ok)
	}
	// The TTL restarted at the second Set: 6+10 = 16s is live, 16.1s is not.
	clk.Advance(9 * time.Second)
	if v, ok := c.Get("a"); !ok || v != 2 {
		t.Fatalf("TTL should restart on replace (15s in): got (%v, %v)", v, ok)
	}
	clk.Advance(10 * time.Second)
	if _, ok := c.Get("a"); ok {
		t.Fatal("TTL should restart on replace: entry should expire 10s after the second Set")
	}
	// Len reflects the single entry, not two.
	c.Set("a", 3)
	if got := c.Len(); got != 1 {
		t.Fatalf("Len = %d after replace, want 1", got)
	}
}

// Rule 3: LRU eviction, and expired entries never cause a live eviction.
func TestLRUEviction(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{MaxEntries: 2, Now: clk.Now})

	c.Set("a", 1)
	c.Set("b", 2)
	// Touch "a": it becomes most recently used, so "b" is evicted.
	if _, ok := c.Get("a"); !ok {
		t.Fatal("expected hit on a")
	}
	c.Set("c", 3)
	if _, ok := c.Get("b"); ok {
		t.Fatal("b should have been evicted as least recently used")
	}
	if v, ok := c.Get("a"); !ok || v != 1 {
		t.Fatalf("a should survive eviction: got (%v, %v)", v, ok)
	}
	if got := c.Len(); got != 2 {
		t.Fatalf("Len = %d, want 2", got)
	}

	// A load completing stores its value and evicts too (storage is storage).
	c2 := New[string, int](Options{MaxEntries: 1, Now: clk.Now})
	c2.Set("x", 1)
	if _, err := c2.GetOrLoad(context.Background(), "y", func(context.Context) (int, error) {
		return 2, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := c2.Get("x"); ok {
		t.Fatal("x should have been evicted when y was stored")
	}

	// Expired entries never cause a live entry to be evicted.
	clk2 := newFakeClock()
	c3 := New[string, int](Options{TTL: 5 * time.Second, MaxEntries: 1, Now: clk2.Now})
	c3.Set("old", 1) // will be expired
	clk2.Advance(10 * time.Second)
	c3.Set("live", 2) // "old" is expired; storing "live" must not evict... itself
	if v, ok := c3.Get("live"); !ok || v != 2 {
		t.Fatalf("live entry must survive: got (%v, %v)", v, ok)
	}
	// A fuller check: two entries, one expired, MaxEntries 1.
	c4 := New[string, int](Options{TTL: 5 * time.Second, MaxEntries: 1, Now: clk2.Now})
	c4.Set("e1", 1)
	clk2.Advance(10 * time.Second) // e1 expired
	c4.Set("live", 2)
	if got := c4.Len(); got != 1 {
		t.Fatalf("Len = %d, want 1", got)
	}
	if v, ok := c4.Get("live"); !ok || v != 2 {
		t.Fatalf("storing over an expired entry must not evict the live entry: got (%v, %v)", v, ok)
	}
}

// Rule 4: a live hit returns the cached value without calling load.
func TestGetOrLoadHit(t *testing.T) {
	c := New[string, int](Options{})
	c.Set("k", 7)
	calls := 0
	v, err := c.GetOrLoad(context.Background(), "k", func(context.Context) (int, error) {
		calls++
		return 0, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if v != 7 {
		t.Fatalf("v = %d, want 7", v)
	}
	if calls != 0 {
		t.Fatalf("load called %d times on a hit, want 0", calls)
	}
	// A hit through GetOrLoad also marks the entry as used (rule 3).
	clk := newFakeClock()
	c2 := New[string, int](Options{MaxEntries: 2, Now: clk.Now})
	c2.Set("a", 1)
	c2.Set("b", 2)
	if _, err := c2.GetOrLoad(context.Background(), "a", func(context.Context) (int, error) {
		t.Error("load must not be called on a hit")
		return 0, nil
	}); err != nil {
		t.Fatal(err)
	}
	c2.Set("c", 3) // evicts "b", the least recently used
	if _, ok := c2.Get("a"); !ok {
		t.Fatal("a should survive: GetOrLoad hit counts as use")
	}
}

// Rule 5: one load per key; different keys load independently.
func TestSingleFlightAndIndependentKeys(t *testing.T) {
	c := New[string, int](Options{})
	var loads int32
	release := make(chan struct{})
	var wg sync.WaitGroup
	const n = 32
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := c.GetOrLoad(context.Background(), "k", func(context.Context) (int, error) {
				atomic.AddInt32(&loads, 1)
				<-release
				return 42, nil
			})
			if err != nil || v != 42 {
				t.Errorf("GetOrLoad = (%d, %v), want (42, nil)", v, err)
			}
		}()
	}
	// Give all callers time to arrive at the shared load, then release.
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt32(&loads) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	close(release)
	wg.Wait()
	if got := atomic.LoadInt32(&loads); got != 1 {
		t.Fatalf("load ran %d times for one key, want 1", got)
	}

	// Different keys load independently, without blocking each other.
	c2 := New[string, int](Options{})
	var startedA, startedB, doneA, doneB bool
	var mu sync.Mutex
	releaseA := make(chan struct{})
	releaseB := make(chan struct{})
	go func() {
		_, _ = c2.GetOrLoad(context.Background(), "a", func(context.Context) (int, error) {
			mu.Lock()
			startedA = true
			mu.Unlock()
			<-releaseA
			return 1, nil
		})
		mu.Lock()
		doneA = true
		mu.Unlock()
	}()
	go func() {
		_, _ = c2.GetOrLoad(context.Background(), "b", func(context.Context) (int, error) {
			mu.Lock()
			startedB = true
			mu.Unlock()
			<-releaseB
			return 2, nil
		})
		mu.Lock()
		doneB = true
		mu.Unlock()
	}()
	// Both loads must start even though "a" has not finished.
	deadline = time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		s := startedA && startedB
		mu.Unlock()
		if s || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	both := startedA && startedB
	mu.Unlock()
	if !both {
		t.Fatal("a blocked load must not block a different key's load")
	}
	close(releaseB)
	close(releaseA)
	deadline = time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		d := doneA && doneB
		mu.Unlock()
		if d || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	d := doneA && doneB
	mu.Unlock()
	if !d {
		t.Fatal("both loads should complete after release")
	}
}

// Rule 6: load errors are not cached and reach every waiter.
func TestLoadError(t *testing.T) {
	c := New[string, int](Options{})
	var calls int32
	want := fmt.Errorf("boom")
	release := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := c.GetOrLoad(context.Background(), "k", func(context.Context) (int, error) {
				atomic.AddInt32(&calls, 1)
				<-release // every load invocation returns the error
				return 0, want
			})
			if v != 0 {
				t.Errorf("v = %d on error, want 0", v)
			}
			if err != want {
				t.Errorf("err = %v, want %v", err, want)
			}
		}()
	}
	// Wait until at least one load is running, then let it fail. A caller
	// that arrives after the failed load finished starts a new load, which
	// also fails: every caller must still end up with the same error.
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt32(&calls) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	close(release)
	wg.Wait()
	if atomic.LoadInt32(&calls) < 1 {
		t.Fatal("load never ran")
	}
	if _, ok := c.Get("k"); ok {
		t.Fatal("failed load must not be cached")
	}
	// A later GetOrLoad calls load again.
	v, err := c.GetOrLoad(context.Background(), "k", func(context.Context) (int, error) {
		calls++
		return 5, nil
	})
	if err != nil || v != 5 {
		t.Fatalf("retry after error: got (%d, %v), want (5, nil)", v, err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

// Rule 7: a caller whose ctx is done returns ctx.Err() promptly and the load
// keeps running for other waiters. Rule 8: the load's context is cancelled
// exactly when every waiter has given up.
func TestCallerGiveUpAndLoadContext(t *testing.T) {
	c := New[string, int](Options{})
	release := make(chan struct{})
	loadCtxCh := make(chan context.Context, 1)
	var loadReturned atomic.Bool

	ctx1, cancel1 := context.WithCancel(context.Background())
	done1 := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(ctx1, "k", func(ctx context.Context) (int, error) {
			loadCtxCh <- ctx
			<-release
			loadReturned.Store(true)
			return 42, nil
		})
		done1 <- err
	}()
	loadCtx := <-loadCtxCh

	// A second caller joins the same load, then gives up.
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(ctx2, "k", func(context.Context) (int, error) {
			t.Error("load must run once per key")
			return 0, nil
		})
		done2 <- err
	}()
	// Wait until caller 2 is registered as a waiter of the in-flight load:
	// observable through GetOrLoad of a fresh key sharing nothing is not, so
	// poll the load's context indirectly below with generous retries.
	time.Sleep(50 * time.Millisecond)

	cancel2()
	select {
	case err := <-done2:
		if err != context.Canceled {
			t.Fatalf("giving-up caller err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("giving-up caller did not return promptly")
	}

	// Rule 8: one waiter (caller 1) remains, so the load context is NOT cancelled.
	select {
	case <-loadCtx.Done():
		t.Fatal("load context cancelled while a waiter remains")
	case <-time.After(100 * time.Millisecond):
	}

	// Rule 7: caller 1 gives up too; now every waiter is gone, so the load
	// context is cancelled exactly then.
	cancel1()
	select {
	case err := <-done1:
		if err != context.Canceled {
			t.Fatalf("giving-up caller err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("giving-up caller did not return promptly")
	}
	select {
	case <-loadCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("load context not cancelled after every waiter gave up")
	}

	// The load keeps running for itself and returns (rule 9) even though
	// every caller gave up.
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for !loadReturned.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !loadReturned.Load() {
		t.Fatal("load did not keep running after all callers gave up")
	}
	// The value is stored (rule 9 tested in detail in TestLateResult).
	if v, ok := c.Get("k"); !ok || v != 42 {
		t.Fatalf("late result not stored: got (%v, %v)", v, ok)
	}
}

// Rule 9: a successful late result is stored, with its TTL starting when
// load returned.
func TestLateResult(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{TTL: 10 * time.Second, Now: clk.Now})
	release := make(chan struct{})
	var started atomic.Bool

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(ctx, "k", func(context.Context) (int, error) {
			started.Store(true)
			<-release
			return 42, nil
		})
		done <- err
	}()
	for !started.Load() {
		time.Sleep(time.Millisecond)
	}
	// Time passes while the load is in flight.
	clk.Advance(7 * time.Second)
	// The only caller gives up.
	cancel()
	<-done
	// The load returns now; its TTL must start now (t+7s), not at load start.
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := c.Get("k"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("late successful result was not stored")
		}
		time.Sleep(time.Millisecond)
	}
	// t+7s + 9s = 16s in: live (would be dead if TTL started at load start).
	clk.Advance(9 * time.Second)
	if _, ok := c.Get("k"); !ok {
		t.Fatal("TTL must start when load returned: entry should still be live")
	}
	clk.Advance(1 * time.Second) // 17s after load start, 10s after load return
	if _, ok := c.Get("k"); ok {
		t.Fatal("TTL must start when load returned: entry should be expired now")
	}
}

// Rule 10: Delete removes the key; an in-flight load's result is still
// stored when it completes.
func TestDelete(t *testing.T) {
	c := New[string, int](Options{})
	c.Set("a", 1)
	c.Delete("a")
	if _, ok := c.Get("a"); ok {
		t.Fatal("Delete must remove the key")
	}
	if got := c.Len(); got != 0 {
		t.Fatalf("Len = %d after Delete, want 0", got)
	}
	c.Delete("missing") // must not panic

	// Delete does not stop an in-flight load.
	release := make(chan struct{})
	var started atomic.Bool
	done := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(context.Background(), "k", func(context.Context) (int, error) {
			started.Store(true)
			<-release
			return 42, nil
		})
		done <- err
	}()
	for !started.Load() {
		time.Sleep(time.Millisecond)
	}
	c.Delete("k")
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("GetOrLoad after Delete of in-flight key: %v", err)
	}
	if v, ok := c.Get("k"); !ok || v != 42 {
		t.Fatalf("in-flight load result must be stored after Delete: got (%v, %v)", v, ok)
	}
}

// Rule 11: concurrency safety and no leaked goroutines.
func TestConcurrency(t *testing.T) {
	c := New[string, int](Options{TTL: time.Millisecond, MaxEntries: 64})
	base := runtime.NumGoroutine()
	var wg sync.WaitGroup
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			ctx := context.Background()
			for i := 0; i < 300; i++ {
				k := fmt.Sprintf("k%d", (w*i)%32)
				switch i % 5 {
				case 0:
					c.Set(k, w)
				case 1:
					c.Get(k)
				case 2:
					c.Delete(k)
				case 3:
					c.Len()
				default:
					_, _ = c.GetOrLoad(ctx, k, func(context.Context) (int, error) {
						return w, nil
					})
				}
			}
		}(w)
	}
	wg.Wait()
	// No goroutine the cache starts may outlive the loads it serves.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > base+2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > base+2 {
		t.Fatalf("goroutines: base %d, now %d; cache leaked goroutines", base, got)
	}
}
