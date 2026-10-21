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

// fakeClock is a controllable clock, safe for concurrent use.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{now: t} }

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

// waitCh polls cond until it holds or the timeout elapses.
func waitUntil(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// --- Rule 1: expiry ---

func TestExpiry(t *testing.T) {
	clk := newFakeClock(time.Unix(1000, 0))
	c := New[string, int](Options{TTL: 5 * time.Second, Now: clk.Now})

	c.Set("a", 1)
	if _, ok := c.Get("a"); !ok {
		t.Fatal("expected live entry to be returned")
	}
	clk.Advance(4 * time.Second) // now == store+4 < store+TTL: still live
	if v, ok := c.Get("a"); !ok || v != 1 {
		t.Fatalf("entry should still be live at store+4s, got %v %v", v, ok)
	}
	clk.Advance(1 * time.Second) // now == store+TTL: expired (live only while Now < store+TTL)
	if _, ok := c.Get("a"); ok {
		t.Fatal("entry must be expired at exactly store+TTL")
	}
	if got := c.Len(); got != 0 {
		t.Fatalf("Len = %d, want 0 after expiry", got)
	}
}

func TestLenCountsOnlyLiveEntries(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	c := New[string, int](Options{TTL: 5 * time.Second, Now: clk.Now})

	c.Set("early", 1)
	clk.Advance(3 * time.Second)
	c.Set("late", 2)
	clk.Advance(2 * time.Second) // "early" is at store+5s: expired; "late" at store+2s: live
	if got := c.Len(); got != 1 {
		t.Fatalf("Len = %d, want 1 (only the live entry)", got)
	}
	if _, ok := c.Get("early"); ok {
		t.Fatal("expired entry must not be returned")
	}
	if v, ok := c.Get("late"); !ok || v != 2 {
		t.Fatalf("live entry missing: %v %v", v, ok)
	}
}

func TestGetOrLoadNeverReturnsExpired(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	c := New[string, int](Options{TTL: 5 * time.Second, Now: clk.Now})

	c.Set("k", 1)
	clk.Advance(6 * time.Second)

	var loads int32
	v, err := c.GetOrLoad(context.Background(), "k", func(context.Context) (int, error) {
		atomic.AddInt32(&loads, 1)
		return 2, nil
	})
	if err != nil {
		t.Fatalf("GetOrLoad: %v", err)
	}
	if v != 2 || atomic.LoadInt32(&loads) != 1 {
		t.Fatalf("expired entry must not be served: got v=%d loads=%d", v, loads)
	}
}

// --- Rule 2: replacement ---

func TestSetReplacesAndRestartsTTL(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	c := New[string, int](Options{TTL: 5 * time.Second, Now: clk.Now})

	c.Set("k", 1)
	clk.Advance(4 * time.Second)
	c.Set("k", 2) // replacement restarts the TTL from t=4s
	if v, ok := c.Get("k"); !ok || v != 2 {
		t.Fatalf("Get = %v %v, want 2 true", v, ok)
	}
	clk.Advance(4 * time.Second) // t=8s: would be dead under the original TTL, live under the new one
	if v, ok := c.Get("k"); !ok || v != 2 {
		t.Fatalf("TTL was not restarted: got %v %v", v, ok)
	}
	clk.Advance(1 * time.Second) // t=9s == new store(4s)+5s
	if _, ok := c.Get("k"); ok {
		t.Fatal("entry should expire 5s after the replacing Set")
	}
}

// --- Rule 3: LRU eviction ---

func TestLRUEviction(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	c := New[string, int](Options{MaxEntries: 2, Now: clk.Now})

	c.Set("a", 1)
	c.Set("b", 2)
	c.Get("a") // "a" is now more recently used than "b"
	c.Set("c", 3)
	if _, ok := c.Get("b"); ok {
		t.Fatal("least recently used entry b should have been evicted")
	}
	if _, ok := c.Get("a"); !ok {
		t.Fatal("recently used entry a should have survived")
	}
	if _, ok := c.Get("c"); !ok {
		t.Fatal("newly stored entry c should be present")
	}
	if got := c.Len(); got != 2 {
		t.Fatalf("Len = %d, want 2", got)
	}
}

func TestSetExistingKeyDoesNotEvict(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	c := New[string, int](Options{MaxEntries: 2, Now: clk.Now})

	c.Set("a", 1)
	c.Set("b", 2)
	c.Set("a", 10) // replacement, not a new entry: nothing to evict
	if _, ok := c.Get("b"); !ok {
		t.Fatal("b should not have been evicted by a replacement Set")
	}
}

func TestExpiredEntriesNeverEvictLiveOnes(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	c := New[string, int](Options{TTL: 5 * time.Second, MaxEntries: 2, Now: clk.Now})

	c.Set("a", 1)
	clk.Advance(10 * time.Second) // "a" is expired now
	c.Set("b", 2)
	c.Set("c", 3) // over capacity only when counting the expired "a"
	if _, ok := c.Get("a"); ok {
		t.Fatal("expired entry a must be gone")
	}
	if _, ok := c.Get("b"); !ok {
		t.Fatal("live entry b must not be evicted because of an expired entry")
	}
	if _, ok := c.Get("c"); !ok {
		t.Fatal("live entry c must not be evicted because of an expired entry")
	}
	if got := c.Len(); got != 2 {
		t.Fatalf("Len = %d, want 2", got)
	}
}

// --- Rule 4: hits ---

func TestGetOrLoadHitDoesNotCallLoad(t *testing.T) {
	c := New[string, int](Options{})

	var loads int32
	load := func(context.Context) (int, error) {
		atomic.AddInt32(&loads, 1)
		return 42, nil
	}
	v, err := c.GetOrLoad(context.Background(), "k", load)
	if err != nil || v != 42 {
		t.Fatalf("first GetOrLoad = %v, %v", v, err)
	}
	v, err = c.GetOrLoad(context.Background(), "k", load)
	if err != nil || v != 42 {
		t.Fatalf("second GetOrLoad = %v, %v", v, err)
	}
	if n := atomic.LoadInt32(&loads); n != 1 {
		t.Fatalf("load called %d times, want 1 (hit must not call load)", n)
	}
}

// --- Rule 5: one load per key; different keys independent ---

func TestConcurrentGetOrLoadSingleLoadPerKey(t *testing.T) {
	c := New[string, int](Options{})

	var loads int32
	started := make(chan struct{})
	release := make(chan struct{})
	load := func(context.Context) (int, error) {
		if atomic.AddInt32(&loads, 1) == 1 {
			close(started)
		}
		<-release
		return 7, nil
	}

	const n = 16
	vals := make([]int, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			vals[i], errs[i] = c.GetOrLoad(context.Background(), "k", load)
		}(i)
	}
	<-started
	time.Sleep(50 * time.Millisecond) // let the other callers join the in-flight load
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&loads); got != 1 {
		t.Fatalf("load ran %d times, want 1", got)
	}
	for i := range vals {
		if errs[i] != nil || vals[i] != 7 {
			t.Fatalf("caller %d got %v, %v; want 7, nil", i, vals[i], errs[i])
		}
	}
}

func TestDifferentKeysLoadIndependently(t *testing.T) {
	c := New[string, int](Options{})

	releaseA := make(chan struct{})
	startedA := make(chan struct{})
	loadA := func(context.Context) (int, error) {
		close(startedA)
		<-releaseA
		return 1, nil
	}
	loadB := func(context.Context) (int, error) {
		return 2, nil
	}

	aErr := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(context.Background(), "a", loadA)
		aErr <- err
	}()
	<-startedA

	doneB := make(chan error, 1)
	go func() {
		v, err := c.GetOrLoad(context.Background(), "b", loadB)
		if err == nil && v != 2 {
			err = fmt.Errorf("got %d", v)
		}
		doneB <- err
	}()
	select {
	case err := <-doneB:
		if err != nil {
			t.Fatalf("key b blocked by in-flight load for key a: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("key b did not load independently of key a")
	}
	close(releaseA)
	if err := <-aErr; err != nil {
		t.Fatalf("key a load: %v", err)
	}
}

// --- Rule 6: load errors ---

func TestLoadErrorNotCachedAndRetried(t *testing.T) {
	c := New[string, int](Options{})

	var loads int32
	boom := errors.New("boom")
	load := func(context.Context) (int, error) {
		if atomic.AddInt32(&loads, 1) == 1 {
			return 0, boom
		}
		return 5, nil
	}
	if _, err := c.GetOrLoad(context.Background(), "k", load); !errors.Is(err, boom) {
		t.Fatalf("first GetOrLoad err = %v, want boom", err)
	}
	if got := c.Len(); got != 0 {
		t.Fatalf("Len = %d after a failed load, want 0", got)
	}
	v, err := c.GetOrLoad(context.Background(), "k", load)
	if err != nil || v != 5 {
		t.Fatalf("second GetOrLoad = %v, %v; want 5, nil (load must be retried)", v, err)
	}
}

func TestConcurrentWaitersShareLoadError(t *testing.T) {
	c := New[string, int](Options{})

	var loads int32
	boom := errors.New("boom")
	release := make(chan struct{})
	load := func(context.Context) (int, error) {
		atomic.AddInt32(&loads, 1)
		<-release
		return 0, boom
	}

	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = c.GetOrLoad(context.Background(), "k", load)
		}(i)
	}
	time.Sleep(50 * time.Millisecond) // let everyone join the single load
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&loads); got != 1 {
		t.Fatalf("load ran %d times, want 1", got)
	}
	for i := range errs {
		if !errors.Is(errs[i], boom) {
			t.Fatalf("waiter %d err = %v, want boom", i, errs[i])
		}
	}
	if got := c.Len(); got != 0 {
		t.Fatalf("Len = %d after failed load, want 0", got)
	}
}

// --- Rule 7: callers that give up ---

func TestCallerGivesUpButLoadKeepsRunningForOthers(t *testing.T) {
	c := New[string, int](Options{})

	started := make(chan struct{})
	release := make(chan struct{})
	load := func(ctx context.Context) (int, error) {
		close(started)
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-release:
			return 99, nil
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	patientErr := make(chan error, 1)
	patientVal := make(chan int, 1)
	go func() {
		v, err := c.GetOrLoad(context.Background(), "k", load)
		patientVal <- v
		patientErr <- err
	}()
	<-started
	impatientErr := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(ctx, "k", load)
		impatientErr <- err
	}()
	time.Sleep(50 * time.Millisecond) // let the second caller join
	start := time.Now()
	cancel()

	select {
	case err := <-impatientErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("impatient caller err = %v, want context.Canceled", err)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("impatient caller took %v to return", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled caller did not return promptly")
	}

	close(release) // the load kept running and must still serve the patient caller
	select {
	case v := <-patientVal:
		if v != 99 {
			t.Fatalf("patient caller got %d, want 99", v)
		}
	case err := <-patientErr:
		t.Fatalf("patient caller failed: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("patient caller never got the load result")
	}
	if v, ok := c.Get("k"); !ok || v != 99 {
		t.Fatalf("load result not cached: %v %v", v, ok)
	}
}

// --- Rule 8: the load's context ---

func TestLoadContextCancelledWhenLastWaiterGivesUp(t *testing.T) {
	c := New[string, int](Options{})

	var state struct {
		mu        sync.Mutex
		cancelled bool
	}
	load := func(ctx context.Context) (int, error) {
		<-ctx.Done()
		state.mu.Lock()
		state.cancelled = true
		state.mu.Unlock()
		return 0, ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(ctx, "k", load)
		errCh <- err
	}()
	time.Sleep(50 * time.Millisecond) // let the caller reach the load
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("caller err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("caller did not return after cancellation")
	}
	waitUntil(t, 2*time.Second, "load context cancellation", func() bool {
		state.mu.Lock()
		defer state.mu.Unlock()
		return state.cancelled
	})
}

func TestLoadContextNotCancelledWhileAWaiterRemains(t *testing.T) {
	c := New[string, int](Options{})

	var state struct {
		mu        sync.Mutex
		cancelled bool
	}
	release := make(chan struct{})
	load := func(ctx context.Context) (int, error) {
		select {
		case <-ctx.Done():
			state.mu.Lock()
			state.cancelled = true
			state.mu.Unlock()
			return 0, ctx.Err()
		case <-release:
			return 1, nil
		}
	}

	ctxA, cancelA := context.WithCancel(context.Background())
	resA := make(chan error, 1)
	resB := make(chan error, 1)
	go func() { _, err := c.GetOrLoad(ctxA, "k", load); resA <- err }()
	go func() { _, err := c.GetOrLoad(context.Background(), "k", load); resB <- err }()
	time.Sleep(50 * time.Millisecond) // let both callers join

	cancelA() // one of several callers gives up
	select {
	case err := <-resA:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("caller A err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("caller A did not return")
	}
	time.Sleep(100 * time.Millisecond) // grace period to observe a wrong cancellation
	state.mu.Lock()
	cancelled := state.cancelled
	state.mu.Unlock()
	if cancelled {
		t.Fatal("load context was cancelled while another waiter remained")
	}

	close(release) // the load must still be running for caller B
	select {
	case err := <-resB:
		if err != nil {
			t.Fatalf("caller B err = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("caller B never got the load result; load context was cancelled early")
	}
	if _, ok := c.Get("k"); !ok {
		t.Fatal("load result should have been cached")
	}
}

// --- Rule 9: late results ---

func TestLateResultStoredWithTTLOfLoadReturn(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	c := New[string, string](Options{TTL: 5 * time.Second, Now: clk.Now})

	started := make(chan struct{})
	release := make(chan struct{})
	load := func(ctx context.Context) (string, error) {
		close(started)
		select {
		case <-ctx.Done():
		case <-release:
		}
		return "late", nil // succeeds even though the caller gave up
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(ctx, "k", load)
		errCh <- err
	}()
	<-started
	clk.Advance(3 * time.Second) // time passes while the load is in flight
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("caller err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("caller did not return after cancellation")
	}
	close(release) // load returns successfully after every caller gave up

	waitUntil(t, 2*time.Second, "late result to be cached", func() bool {
		_, ok := c.Get("k")
		return ok
	})

	clk.Advance(4 * time.Second) // t=7s < loadReturn(3s)+TTL(5s)=8s
	if v, ok := c.Get("k"); !ok || v != "late" {
		t.Fatalf("late result missing at t=7s: %v %v", v, ok)
	}
	clk.Advance(1 * time.Second) // t=8s == loadReturn+TTL
	if _, ok := c.Get("k"); ok {
		t.Fatal("late result should expire 5s after load returned (t=8s), not after the caller left")
	}
}

// --- Rule 10: Delete ---

func TestDelete(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	c := New[string, int](Options{TTL: 5 * time.Second, Now: clk.Now})

	c.Set("a", 1)
	c.Set("b", 2)
	c.Delete("a")
	if _, ok := c.Get("a"); ok {
		t.Fatal("deleted entry still present")
	}
	if _, ok := c.Get("b"); !ok {
		t.Fatal("unrelated entry was removed")
	}
	if got := c.Len(); got != 1 {
		t.Fatalf("Len = %d, want 1", got)
	}
	c.Delete("missing") // must not panic
}

func TestDeleteDoesNotStopInFlightLoad(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	c := New[string, string](Options{TTL: 5 * time.Second, Now: clk.Now})

	c.Set("k", "old")
	clk.Advance(10 * time.Second) // expire it so GetOrLoad misses

	started := make(chan struct{})
	release := make(chan struct{})
	load := func(context.Context) (string, error) {
		close(started)
		<-release
		return "fresh", nil
	}
	errCh := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(context.Background(), "k", load)
		errCh <- err
	}()
	<-started

	c.Delete("k")
	if _, ok := c.Get("k"); ok {
		t.Fatal("Delete should remove the key")
	}
	close(release)

	waitUntil(t, 2*time.Second, "in-flight load result to be stored after Delete", func() bool {
		v, ok := c.Get("k")
		return ok && v == "fresh"
	})
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("GetOrLoad err = %v", err)
		}
	default:
	}
}

// --- Rule 11: concurrency and goroutine lifetime ---

func TestConcurrentMixedOperations(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	c := New[string, int](Options{TTL: 10 * time.Millisecond, MaxEntries: 8, Now: clk.Now})

	var loads int64
	load := func(ctx context.Context) (int, error) {
		n := int(atomic.AddInt64(&loads, 1))
		if n%7 == 0 {
			return 0, errors.New("boom")
		}
		return n, nil
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	moverDone := make(chan struct{})
	go func() { // a clock mover, so expiry races with everything else
		defer close(moverDone)
		for {
			select {
			case <-stop:
				return
			default:
				clk.Advance(time.Millisecond)
			}
		}
	}()
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				key := fmt.Sprintf("k%d", (g*7+i)%10)
				switch i % 5 {
				case 0:
					c.Set(key, i)
				case 1:
					c.Get(key)
				case 2:
					c.Delete(key)
				case 3:
					c.Len()
				default:
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
					c.GetOrLoad(ctx, key, load)
					cancel()
				}
			}
		}(g)
	}
	// Wait for the workers, then stop the clock mover (separate WaitGroup
	// member: the mover only stops after the workers are done).
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		close(stop)
		t.Fatal("mixed-operation test hung")
	}
	close(stop)
	<-moverDone
}

func TestNoGoroutineOutlivesLoads(t *testing.T) {
	c := New[string, int](Options{})

	load := func(ctx context.Context) (int, error) {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		default:
			return 1, nil
		}
	}
	// Settle baseline goroutines (spawned by previous tests' timers etc.).
	time.Sleep(50 * time.Millisecond)
	base := runtime.NumGoroutine()

	for i := 0; i < 100; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		if i%2 == 0 {
			cancel() // half the callers give up; loads must still terminate
		}
		c.GetOrLoad(ctx, fmt.Sprintf("k%d", i%5), load)
		cancel()
	}
	waitUntil(t, 5*time.Second, "goroutines to finish", func() bool {
		return runtime.NumGoroutine() <= base+2
	})
}

// --- Defaults and edge cases ---

func TestNoTTLNeverExpires(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	c := New[string, int](Options{Now: clk.Now}) // TTL 0: entries never expire

	c.Set("k", 1)
	clk.Advance(1000 * time.Hour)
	if v, ok := c.Get("k"); !ok || v != 1 {
		t.Fatalf("entry with TTL 0 expired: %v %v", v, ok)
	}
	if got := c.Len(); got != 1 {
		t.Fatalf("Len = %d, want 1", got)
	}
}

func TestUnboundedWhenMaxEntriesZero(t *testing.T) {
	c := New[int, int](Options{})
	for i := 0; i < 100; i++ {
		c.Set(i, i)
	}
	if got := c.Len(); got != 100 {
		t.Fatalf("Len = %d, want 100 (unbounded)", got)
	}
}

func TestNilNowUsesTimeNow(t *testing.T) {
	c := New[string, int](Options{TTL: time.Minute}) // Now nil: time.Now
	c.Set("k", 1)
	if v, ok := c.Get("k"); !ok || v != 1 {
		t.Fatalf("Get = %v %v", v, ok)
	}
	if got := c.Len(); got != 1 {
		t.Fatalf("Len = %d, want 1", got)
	}
}

func TestGetMissingKey(t *testing.T) {
	c := New[string, int](Options{})
	v, ok := c.Get("nope")
	if ok || v != 0 {
		t.Fatalf("Get on missing key = %v %v, want 0 false", v, ok)
	}
}
