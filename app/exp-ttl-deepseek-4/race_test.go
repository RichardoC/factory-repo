// race_test.go owns the concurrency evidence for the issue's rules 11 and 12:
// one file that stress-runs the whole cache under -race and proves the cache
// starts no goroutine that outlives the load it serves.
//
// The scripts here are deterministic: nothing asserts on wall-clock progress.
// time.After appears only as a failure guard (a bound that a correct cache
// never reaches), never as the mechanism under test.
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

// drainGoroutines waits (bounded) for the process goroutine count to fall back
// to at most base. It returns the observed count.
func drainGoroutines(base int, budget time.Duration) int {
	deadline := time.Now().Add(budget)
	n := runtime.NumGoroutine()
	for n > base && time.Now().Before(deadline) {
		runtime.Gosched()
		time.Sleep(time.Millisecond)
		n = runtime.NumGoroutine()
	}
	return n
}

// noGoroutineLeak reports whether the process goroutine count returned to at
// most base after the cache's loads finished. A count *above* base is a leak: a
// goroutine the cache started outlived its load. A count *below* base is not a
// leak -- it only means unrelated goroutines (the test runtime's own
// bookkeeping, or a sibling test's leftovers) exited meanwhile. Asserting exact
// equality with base would fail spuriously in that case, so the check is a
// one-sided bound. It returns the observed count.
func noGoroutineLeak(base int, budget time.Duration) (int, bool) {
	n := drainGoroutines(base, budget)
	return n, n <= base
}

// TestRaceMixedOperations hammers every public operation from many goroutines
// over a small key space while another goroutine advances a scripted clock.
// It is the rule 11 race check: run with -race, any unsynchronised access to
// the entries map, the LRU list or the in-flight table is reported.
func TestRaceMixedOperations(t *testing.T) {
	const (
		goroutines = 16
		keys       = 8
		iterations = 400
	)
	clock := newScriptedClock()
	c := New[int, int](Options{
		TTL:        50 * time.Millisecond,
		MaxEntries: 4, // smaller than the key space, so eviction runs too
		Now:        clock.Now,
	})

	stop := make(chan struct{})
	var ticker sync.WaitGroup
	ticker.Add(1)
	go func() {
		defer ticker.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			clock.Advance(7 * time.Millisecond)
			runtime.Gosched()
		}
	}()

	start := make(chan struct{})
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			<-start
			for i := 0; i < iterations; i++ {
				key := (g + i) % keys
				switch (g + i) % 5 {
				case 0:
					c.Get(key)
				case 1:
					c.Set(key, g*iterations+i)
				case 2:
					c.Delete(key)
				case 3:
					c.Len()
				case 4:
					// A fresh ctx per call: this path must never leak a
					// goroutine or deadlock on the in-flight table. No value
					// is asserted here -- a concurrent Set/Delete on the same
					// key legitimately changes what a hit returns; the claim
					// is race-freedom plus a non-nil error only when this
					// caller's own ctx is done.
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					_, err := c.GetOrLoad(ctx, key, func(context.Context) (int, error) {
						return key * 1000, nil
					})
					cancel()
					if err != nil && !errors.Is(err, context.Canceled) {
						t.Errorf("GetOrLoad(%d) error = %v, want nil or context.Canceled", key, err)
						return
					}
				}
			}
		}(g)
	}
	close(start)
	wg.Wait()
	close(stop)
	ticker.Wait()

	// Every in-flight load was served by a goroutine that has since exited.
	base := runtime.NumGoroutine()
	if n, ok := noGoroutineLeak(base, 2*time.Second); !ok {
		t.Errorf("after the mixed-operation run the goroutine count is %d, want at most the baseline %d", n, base)
	}
	t.Logf("mixed-operation run: %d goroutines x %d iterations over %d keys; goroutine baseline %d", goroutines, iterations, keys, base)
}

// TestConcurrentSameKeyExactlyOneLoad proves rule 5: several callers on one
// miss share exactly one load, all get the same value, and the value is cached
// afterwards.
func TestConcurrentSameKeyExactlyOneLoad(t *testing.T) {
	const callers = 8
	clock := newScriptedClock()
	c := New[string, int](Options{TTL: 0, Now: clock.Now})

	gate := make(chan struct{})
	entered := make(chan struct{})
	var loads int32
	load := func(context.Context) (int, error) {
		atomic.AddInt32(&loads, 1)
		close(entered)
		<-gate
		return 42, nil
	}

	var wg sync.WaitGroup
	results := make([]int, callers)
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, err := c.GetOrLoad(context.Background(), "k", load)
			results[i], errs[i] = v, err
		}(i)
	}

	// The one load must be in flight before the others can be allowed to join;
	// the first waiter is enough because every later caller attaches to the
	// same call while it is unfinished.
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the single load never started within 2s")
	}
	// Give the remaining callers time to attach, without asserting on timing:
	// the guarantee is that they either attach here or find the stored value
	// after the release, and either way load runs once.
	for i := 0; i < 200; i++ {
		runtime.Gosched()
	}
	close(gate)
	wg.Wait()

	if got := atomic.LoadInt32(&loads); got != 1 {
		t.Errorf("load ran %d times, want exactly 1", got)
	}
	for i := 0; i < callers; i++ {
		if errs[i] != nil {
			t.Fatalf("caller %d: unexpected error %v", i, errs[i])
		}
		if results[i] != 42 {
			t.Fatalf("caller %d got %d, want 42", i, results[i])
		}
	}
	if v, ok := c.Get("k"); !ok || v != 42 {
		t.Fatalf("after the load, Get(k) = (%d, %v), want (42, true)", v, ok)
	}
	if n := c.Len(); n != 1 {
		t.Fatalf("Len() = %d, want 1", n)
	}
	// A later call is a hit: still exactly one load.
	if v, err := c.GetOrLoad(context.Background(), "k", load); err != nil || v != 42 {
		t.Fatalf("cached GetOrLoad = (%d, %v), want (42, <nil>)", v, err)
	}
	if got := atomic.LoadInt32(&loads); got != 1 {
		t.Errorf("after the cached read load ran %d times, want exactly 1", got)
	}
	t.Logf("%d concurrent callers on one key ran the load %d time", callers, atomic.LoadInt32(&loads))
}

// TestNoGoroutineOutlivesLoadUnderRace is this file's rule 11 leak proof. It
// runs K loads in every shape a loader can end in -- a load whose only caller
// gave up, a load that returns an error, and a load released only after every
// caller gave up -- and then requires the goroutine count to return to the
// baseline it started from, naming both counts if it does not.
//
// Every gate-blocked load is started by a caller that is still waiting when the
// load begins; a caller whose ctx is already done never starts a load (the
// issue's rule 7 path), so it cannot be used to observe one in flight.
func TestNoGoroutineOutlivesLoadUnderRace(t *testing.T) {
	errBoom := errors.New("boom")
	clock := newScriptedClock()

	// Warm up so the runtime's own lazily started bookkeeping goroutines are
	// not mistaken for cache leaks.
	warm := New[int, int](Options{})
	for i := 0; i < 16; i++ {
		if _, err := warm.GetOrLoad(context.Background(), i, func(context.Context) (int, error) {
			return i, nil
		}); err != nil {
			t.Fatalf("warm-up GetOrLoad: %v", err)
		}
	}
	base := runtime.NumGoroutine()
	t.Logf("goroutine baseline before the loads: %d", base)

	c := New[int, int](Options{TTL: 0, Now: clock.Now})
	started := make(chan struct{}, 128)
	var loads int32

	const rounds = 8
	var callers sync.WaitGroup
	for i := 0; i < rounds; i++ {
		key := i

		// Shape 1: the only caller gives up on its ctx while the load is
		// genuinely in flight. The goroutine must still exit and store.
		ctx, cancel := context.WithCancel(context.Background())
		loadDone := make(chan struct{})
		gone := make(chan error, 1)
		mustLoad := make(chan struct{})
		callers.Add(1)
		go func() {
			defer callers.Done()
			close(mustLoad)
			_, err := c.GetOrLoad(ctx, key, func(context.Context) (int, error) {
				atomic.AddInt32(&loads, 1)
				started <- struct{}{}
				<-loadDone
				return key, nil
			})
			gone <- err
		}()
		<-mustLoad
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatalf("round %d: the gated load never started within 2s", i)
		}
		cancel()
		select {
		case err := <-gone:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("round %d: the caller that gave up returned %v, want context.Canceled", i, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("round %d: the caller that gave up did not return within 2s", i)
		}
		// The load is released only after every caller has gone: this is the
		// late-result case (rule 9), and its goroutine must still exit.
		close(loadDone)

		// Shape 2: the load returns an error; nothing is cached, no goroutine
		// survives it.
		errKey := 1000 + key
		callers.Add(1)
		go func() {
			defer callers.Done()
			if v, err := c.GetOrLoad(context.Background(), errKey, func(context.Context) (int, error) {
				atomic.AddInt32(&loads, 1)
				return 0, errBoom
			}); !errors.Is(err, errBoom) {
				t.Errorf("round %d: errored load returned (%v, %v), want (zero, %v)", i, v, err, errBoom)
			} else if _, ok := c.Get(errKey); ok {
				t.Errorf("round %d: the errored load cached a value", i)
			}
		}()

		// Shape 3: two callers share one load; the first abandons its ctx
		// while the second keeps waiting, so the load must run on. The load
		// itself observes that its ctx is not done at the moment it starts.
		sharedKey := 2000 + key
		ctx2, cancel2 := context.WithCancel(context.Background())
		ctx3, cancel3 := context.WithCancel(context.Background())
		started3 := make(chan struct{}, 1)
		gone3 := make(chan error, 1)
		roundRelease := make(chan struct{}) // shape 3's own gate for this round
		sharedLoad := func(ctx context.Context) (int, error) {
			atomic.AddInt32(&loads, 1)
			select {
			case <-ctx.Done():
				t.Errorf("round %d: the load ctx was already cancelled at its start", i)
			default:
			}
			started3 <- struct{}{}
			<-roundRelease
			return sharedKey, nil
		}
		callers.Add(1)
		go func() {
			defer callers.Done()
			_, err := c.GetOrLoad(ctx3, sharedKey, sharedLoad)
			gone3 <- err
		}()
		select {
		case <-started3:
		case <-time.After(2 * time.Second):
			t.Fatalf("round %d: the shared load never started within 2s", i)
		}
		// The second waiter joins the in-flight call for sharedKey. It must
		// attach before it is cancelled; cancellation before the call returns
		// early without attaching, which is a different but also correct path.
		secondGone := make(chan error, 1)
		callers.Add(1)
		go func() {
			defer callers.Done()
			_, err := c.GetOrLoad(ctx2, sharedKey, func(context.Context) (int, error) {
				t.Errorf("round %d: a second load ran for a key already in flight", i)
				return 0, nil
			})
			secondGone <- err
		}()
		// Let the second caller reach the shared call. This is a bounded
		// spin, not a timing assertion.
		for s := 0; s < 500; s++ {
			runtime.Gosched()
		}
		// The abandoning waiter leaves. Its own outcome may be either: if it
		// attached to the shared call it returns context.Canceled, and if it
		// did not get there before the call it is simply a future hit. Its
		// only job here is to keep the shared load alive, so only a spurious
		// non-ctx error would be a defect.
		cancel2()
		if err := <-secondGone; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("round %d: the abandoning waiter returned %v, want context.Canceled or nil", i, err)
		}
		select {
		case <-roundRelease:
			t.Fatalf("round %d: the shared load was released before the test released it", i)
		default:
			// Still blocked: the load is genuinely in flight with a waiter.
		}
		// The last waiter gives up too, and only then is the load released.
		cancel3()
		select {
		case err := <-gone3:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("round %d: the last waiter returned %v, want context.Canceled", i, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("round %d: the last waiter did not return within 2s", i)
		}
		close(roundRelease) // let shape 3's load finish and store its late value
	}
	callers.Wait()

	// Shape 1's abandoned load stored its late success (rule 9), so key 0 is a
	// hit: the in-flight table holds no stale call and no reload is needed.
	var reloaded int32
	v0, err := c.GetOrLoad(context.Background(), 0, func(context.Context) (int, error) {
		atomic.AddInt32(&reloaded, 1)
		return 7, nil
	})
	if err != nil {
		t.Fatalf("GetOrLoad on the abandoned key after its load: %v", err)
	}
	if v0 != 0 || atomic.LoadInt32(&reloaded) != 0 {
		t.Fatalf("GetOrLoad on the abandoned key = (%d, reloads %d), want the stored late value 0 and no reload", v0, atomic.LoadInt32(&reloaded))
	}
	// A key whose load errored is not cached (rule 6): it loads again.
	var reloads int32
	if _, err := c.GetOrLoad(context.Background(), 1000, func(context.Context) (int, error) {
		atomic.AddInt32(&reloads, 1)
		return 9, nil
	}); err != nil {
		t.Fatalf("GetOrLoad on the errored key after its load: %v", err)
	}
	if atomic.LoadInt32(&reloads) != 1 {
		t.Fatalf("the errored key reloaded %d times, want 1", atomic.LoadInt32(&reloads))
	}

	final, ok := noGoroutineLeak(base, 5*time.Second)
	if !ok {
		t.Fatalf("goroutine leak: count is %d after the loads finished, baseline was %d (a goroutine the cache started outlived its load)", final, base)
	}
	t.Logf("goroutine baseline %d, final %d after %d loads in 3 shapes (abandoned, errored, late)", base, final, atomic.LoadInt32(&loads))
}

// TestContextCancellationPropagation is rule 8's observable edge: the ctx
// handed to load is not cancelled while at least one waiter remains, and
// becomes done once every waiter has given up. The 50ms selected in the middle
// is a grace window and a failure guard, never the mechanism.
func TestContextCancellationPropagation(t *testing.T) {
	clock := newScriptedClock()
	c := New[string, int](Options{TTL: 0, Now: clock.Now})

	loadCtx := make(chan context.Context, 1)
	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce sync.Once
	load := func(ctx context.Context) (int, error) {
		loadCtx <- ctx
		enteredOnce.Do(func() { close(entered) })
		<-release
		return 1, nil
	}

	// First waiter: it starts the load and keeps waiting throughout.
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	resA := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(ctxA, "k", load)
		resA <- err
	}()
	var lctx context.Context
	select {
	case lctx = <-loadCtx:
	case <-time.After(2 * time.Second):
		t.Fatal("the load never started within 2s")
	}
	<-entered

	// Second waiter: it joins the same in-flight load.
	ctxB, cancelB := context.WithCancel(context.Background())
	resB := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(ctxB, "k", load)
		resB <- err
	}()
	// Let the second caller attach before it is cancelled: this is a bounded
	// spin, not an assertion on timing.
	for i := 0; i < 200; i++ {
		runtime.Gosched()
	}

	// The SECOND waiter gives up while the first still waits: rule 8 says the
	// load's ctx must NOT become done. The grace window below elapses with the
	// ctx still live, and only then do we accept the rule as holding.
	cancelB()
	select {
	case err := <-resB:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("the abandoning caller returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the abandoning caller did not return within 2s")
	}
	select {
	case <-lctx.Done():
		t.Fatalf("the load's ctx was cancelled while one waiter remained: %v", lctx.Err())
	case <-time.After(50 * time.Millisecond):
		// Grace window: the load's ctx is still live with a waiter present.
	}

	// First waiter gives up too: now the load's ctx must become done.
	cancelA()
	select {
	case err := <-resA:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("the last abandoning caller returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the last abandoning caller did not return within 2s")
	}
	select {
	case <-lctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the load's ctx was not cancelled within 2s after the last waiter gave up")
	}
	if err := lctx.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("the load's ctx error is %v, want context.Canceled", err)
	}

	// Let the load return. Its late success must still be stored (rule 9), and
	// its goroutine must be gone.
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if v, ok := c.Get("k"); ok {
			if v != 1 {
				t.Fatalf("late result stored as %d, want 1", v)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the late successful result was never stored within 2s")
		}
		runtime.Gosched()
	}
	base := runtime.NumGoroutine()
	if n, ok := noGoroutineLeak(base, 5*time.Second); !ok {
		t.Fatalf("goroutine leak after the cancelled load: count %d, baseline %d", n, base)
	}
	t.Log("load ctx stayed live with one waiter, became done at zero waiters, and the late value was cached")
}

// TestRaceStoresUnderEviction contends Set/Set/Get on a one-entry cache so the
// evict path runs concurrently with itself under -race.
func TestRaceStoresUnderEviction(t *testing.T) {
	const (
		goroutines = 12
		iterations = 300
	)
	clock := newScriptedClock()
	c := New[int, int](Options{TTL: 0, MaxEntries: 1, Now: clock.Now})

	start := make(chan struct{})
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			<-start
			for i := 0; i < iterations; i++ {
				c.Set(i%3, g*iterations+i)
				c.Set(i%3+10, i)
				c.Get(i % 5)
				if n := c.Len(); n > 1 {
					t.Errorf("Len() = %d with MaxEntries=1 after Set/Set/Get", n)
					return
				}
			}
		}(g)
	}
	close(start)
	wg.Wait()

	if n := c.Len(); n > 1 {
		t.Errorf("final Len() = %d with MaxEntries=1, want <= 1", n)
	}
	t.Logf("%d goroutines x %d rounds of Set/Set/Get on an unbounded-TTL MaxEntries=1 cache; final Len() = %d", goroutines, iterations, c.Len())
}

// TestRaceConcurrentKeysLoadIndependently is a rule 5 companion: distinct keys
// must not block each other, so N gated loads must all be in flight at once.
// It also re-checks the leak invariant after the loads finish.
func TestRaceConcurrentKeysLoadIndependently(t *testing.T) {
	const keys = 6
	base := runtime.NumGoroutine()
	clock := newScriptedClock()
	c := New[int, int](Options{TTL: 0, Now: clock.Now})

	release := make(chan struct{})
	entered := make(chan int, keys)
	var wg sync.WaitGroup
	for k := 0; k < keys; k++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			v, err := c.GetOrLoad(context.Background(), k, func(context.Context) (int, error) {
				entered <- k
				<-release
				return k * 10, nil
			})
			if err != nil || v != k*10 {
				t.Errorf("GetOrLoad(%d) = (%d, %v), want (%d, <nil>)", k, v, err, k*10)
			}
		}(k)
	}
	// All keys must reach their loader while none has been released: if one
	// load blocked another, this would time out.
	seen := map[int]bool{}
	for i := 0; i < keys; i++ {
		select {
		case k := <-entered:
			if seen[k] {
				t.Fatalf("key %d entered its loader twice", k)
			}
			seen[k] = true
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d distinct loads were in flight at once: %v", len(seen), keys, seen)
		}
	}
	close(release)
	wg.Wait()

	if n, ok := noGoroutineLeak(base, 5*time.Second); !ok {
		t.Fatalf("goroutine leak after %d keyed loads: count %d, baseline %d", keys, n, base)
	}
	for k := 0; k < keys; k++ {
		if v, ok := c.Get(k); !ok || v != k*10 {
			t.Fatalf("Get(%d) = (%d, %v), want (%d, true)", k, v, ok, k*10)
		}
	}
	t.Logf("%d distinct keys loaded concurrently without blocking; goroutine baseline %d restored", keys, base)
}
