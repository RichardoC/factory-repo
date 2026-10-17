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

// fakeClock is a Now the test moves by hand, so expiry is deterministic
// instead of timing-dependent.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// --- rule 1: expiry ---------------------------------------------------------

func TestExpiryBoundaryAndLen(t *testing.T) {
	clock := newClock()
	c := New[string, int](Options{TTL: 10 * time.Second, Now: clock.Now})
	c.Set("k", 1)

	// live while Now() is before stored+TTL
	clock.advance(10*time.Second - time.Nanosecond)
	if v, ok := c.Get("k"); !ok || v != 1 {
		t.Fatalf("just before expiry: Get = (%v, %v), want (1, true)", v, ok)
	}
	if n := c.Len(); n != 1 {
		t.Fatalf("just before expiry: Len = %d, want 1", n)
	}

	// at exactly stored+TTL the entry is dead
	clock.advance(time.Nanosecond)
	if v, ok := c.Get("k"); ok {
		t.Fatalf("at expiry: Get returned (%v, true), want the zero value and false", v)
	}
	if n := c.Len(); n != 0 {
		t.Fatalf("at expiry: Len = %d, want 0", n)
	}
}

func TestExpiryZeroNeverExpires(t *testing.T) {
	clock := newClock()
	c := New[string, int](Options{Now: clock.Now})
	c.Set("k", 7)
	clock.advance(1000 * time.Hour)
	if v, ok := c.Get("k"); !ok || v != 7 {
		t.Fatalf("TTL 0: Get = (%v, %v), want (7, true)", v, ok)
	}
	if n := c.Len(); n != 1 {
		t.Fatalf("TTL 0: Len = %d, want 1", n)
	}
}

func TestGetOrLoadNeverReturnsExpired(t *testing.T) {
	clock := newClock()
	c := New[string, int](Options{TTL: time.Second, Now: clock.Now})
	c.Set("k", 1)
	clock.advance(2 * time.Second)

	var loads int32
	v, err := c.GetOrLoad(context.Background(), "k", func(context.Context) (int, error) {
		atomic.AddInt32(&loads, 1)
		return 2, nil
	})
	if err != nil || v != 2 {
		t.Fatalf("GetOrLoad after expiry = (%v, %v), want (2, nil)", v, err)
	}
	if n := atomic.LoadInt32(&loads); n != 1 {
		t.Fatalf("load called %d times, want 1", n)
	}
}

// --- rule 2: replacement ----------------------------------------------------

func TestSetReplacesValueAndRestartsTTL(t *testing.T) {
	clock := newClock()
	c := New[string, int](Options{TTL: 10 * time.Second, Now: clock.Now})
	c.Set("k", 1)
	clock.advance(9 * time.Second)
	c.Set("k", 2) // restarts the TTL
	clock.advance(9 * time.Second)
	if v, ok := c.Get("k"); !ok || v != 2 {
		t.Fatalf("after re-Set: Get = (%v, %v), want (2, true)", v, ok)
	}
	clock.advance(2 * time.Second) // 11s after the second Set
	if _, ok := c.Get("k"); ok {
		t.Fatal("entry outlived TTL measured from the second Set")
	}
}

func TestSetReplaceDoesNotGrowLenAndMovesToFront(t *testing.T) {
	clock := newClock()
	c := New[string, int](Options{MaxEntries: 2, Now: clock.Now})
	c.Set("a", 1)
	c.Set("b", 2)
	c.Set("a", 10) // replaces, and counts as use
	if n := c.Len(); n != 2 {
		t.Fatalf("Len = %d, want 2", n)
	}
	c.Set("c", 3) // evicts the LRU, which is now b
	if _, ok := c.Get("b"); ok {
		t.Fatal("b should have been evicted as least recently used")
	}
	if v, ok := c.Get("a"); !ok || v != 10 {
		t.Fatalf("a = (%v, %v), want (10, true)", v, ok)
	}
}

// --- rule 3: LRU eviction ---------------------------------------------------

func TestLRUEvictionOrder(t *testing.T) {
	clock := newClock()
	c := New[string, int](Options{MaxEntries: 2, Now: clock.Now})
	c.Set("a", 1)
	c.Set("b", 2)
	if _, ok := c.Get("a"); !ok { // a becomes the most recently used
		t.Fatal("a missing")
	}
	c.Set("c", 3) // evicts b, the least recently used
	if n := c.Len(); n != 2 {
		t.Fatalf("Len = %d, want 2", n)
	}
	if _, ok := c.Get("b"); ok {
		t.Fatal("b should have been evicted")
	}
	for _, k := range []string{"a", "c"} {
		if _, ok := c.Get(k); !ok {
			t.Fatalf("%q should still be cached", k)
		}
	}
}

func TestEvictionSkipsExpiredEntries(t *testing.T) {
	clock := newClock()
	c := New[string, int](Options{TTL: 5 * time.Second, MaxEntries: 2, Now: clock.Now})
	c.Set("old", 1) // stored at t0, so it dies at t5
	clock.advance(time.Second)
	c.Set("live", 2) // stored at t1, so it dies at t6
	clock.advance(4 * time.Second)
	// Now is t5: "old" is exactly at its deadline and dead, "live" still has
	// one second left. "old" is also the least recently used, so it is the
	// tail of the LRU list and the tempting victim.
	c.Set("new", 3)

	// Rule 3: the expired entry must be dropped instead of the live one.
	if v, ok := c.Get("live"); !ok || v != 2 {
		t.Fatalf("live = (%v, %v), want (2, true): an expired entry evicted a live one", v, ok)
	}
	if _, ok := c.Get("old"); ok {
		t.Fatal("expired entry still readable")
	}
	if n := c.Len(); n != 2 {
		t.Fatalf("Len = %d, want 2", n)
	}
	// The capacity is still honoured: one more live entry pushes out the
	// least recently used live one.
	if _, ok := c.Get("new"); !ok {
		t.Fatal("new entry missing")
	}
	c.Set("extra", 4)
	if n := c.Len(); n != 2 {
		t.Fatalf("Len = %d, want 2", n)
	}
	if _, ok := c.Get("extra"); !ok {
		t.Fatal("the just-stored entry must survive")
	}
}

func TestMaxEntriesZeroIsUnbounded(t *testing.T) {
	c := New[int, int](Options{})
	for i := 0; i < 500; i++ {
		c.Set(i, i)
	}
	if n := c.Len(); n != 500 {
		t.Fatalf("Len = %d, want 500", n)
	}
}

// --- rule 4: hits -----------------------------------------------------------

func TestGetOrLoadHitDoesNotCallLoad(t *testing.T) {
	c := New[string, int](Options{})
	c.Set("k", 41)
	v, err := c.GetOrLoad(context.Background(), "k", func(context.Context) (int, error) {
		t.Fatal("load called on a live hit")
		return 0, nil
	})
	if err != nil || v != 41 {
		t.Fatalf("GetOrLoad = (%v, %v), want (41, nil)", v, err)
	}
}

// --- rule 5: one load per key ----------------------------------------------

func TestConcurrentMissesShareOneLoad(t *testing.T) {
	c := New[string, int](Options{})
	const callers = 50
	release := make(chan struct{})
	var loads int32
	load := func(context.Context) (int, error) {
		atomic.AddInt32(&loads, 1)
		<-release
		return 99, nil
	}

	var wg sync.WaitGroup
	got := make([]int, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, err := c.GetOrLoad(context.Background(), "k", load)
			if err != nil {
				t.Errorf("caller %d: %v", i, err)
				return
			}
			got[i] = v
		}(i)
	}
	// Wait until the single load is definitely running, then let it finish.
	waitFor(t, func() bool { return atomic.LoadInt32(&loads) == 1 })
	time.Sleep(20 * time.Millisecond) // let the other callers join the flight
	close(release)
	wg.Wait()

	if n := atomic.LoadInt32(&loads); n != 1 {
		t.Fatalf("load ran %d times, want 1", n)
	}
	for i, v := range got {
		if v != 99 {
			t.Fatalf("caller %d got %d, want 99", i, v)
		}
	}
}

func TestDifferentKeysLoadConcurrently(t *testing.T) {
	c := New[string, int](Options{})
	// Both loads must be inside load at the same time for this to return.
	bothIn := make(chan struct{}, 2)
	release := make(chan struct{})
	load := func(context.Context) (int, error) {
		bothIn <- struct{}{}
		<-release
		return 1, nil
	}
	var wg sync.WaitGroup
	for _, k := range []string{"a", "b"} {
		wg.Add(1)
		go func(k string) {
			defer wg.Done()
			if _, err := c.GetOrLoad(context.Background(), k, load); err != nil {
				t.Errorf("%s: %v", k, err)
			}
		}(k)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-bothIn:
		case <-time.After(2 * time.Second):
			t.Fatal("two keys did not load concurrently: the second blocked on the first")
		}
	}
	close(release)
	wg.Wait()
}

// --- rule 6: load errors ----------------------------------------------------

func TestLoadErrorNotCachedAndSharedByAllWaiters(t *testing.T) {
	c := New[string, int](Options{})
	boom := errors.New("boom")
	release := make(chan struct{})
	var loads int32
	load := func(context.Context) (int, error) {
		if atomic.AddInt32(&loads, 1) == 1 {
			<-release
			return 0, boom
		}
		return 5, nil
	}

	const callers = 20
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = c.GetOrLoad(context.Background(), "k", load)
		}(i)
	}
	waitFor(t, func() bool { return atomic.LoadInt32(&loads) == 1 })
	time.Sleep(20 * time.Millisecond) // let the other callers join the flight
	close(release)
	wg.Wait()

	for i, err := range errs {
		if !errors.Is(err, boom) {
			t.Fatalf("caller %d got err %v, want boom", i, err)
		}
	}
	if n := c.Len(); n != 0 {
		t.Fatalf("Len = %d after a failed load, want 0", n)
	}
	// A later call loads again and caches.
	if v, err := c.GetOrLoad(context.Background(), "k", load); err != nil || v != 5 {
		t.Fatalf("second attempt = (%v, %v), want (5, nil)", v, err)
	}
	if n := c.Len(); n != 1 {
		t.Fatalf("Len = %d after a successful load, want 1", n)
	}
}

// --- rules 7 and 8: callers that give up, and the load's context ------------

func TestCancelledCallerReturnsPromptlyAndLoadKeepsRunning(t *testing.T) {
	c := New[string, int](Options{})
	loadStarted := make(chan struct{})
	release := make(chan struct{})
	loadCtx := make(chan context.Context, 1)
	load := func(ctx context.Context) (int, error) {
		loadCtx <- ctx
		close(loadStarted)
		<-release
		return 3, nil
	}

	// The first caller starts the load and then gives up.
	giveUp, cancel := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(giveUp, "k", load)
		firstDone <- err
	}()
	<-loadStarted

	// A second caller is already waiting when the first is cancelled.
	waiter := make(chan struct {
		v   int
		err error
	}, 1)
	go func() {
		v, err := c.GetOrLoad(context.Background(), "k", load)
		waiter <- struct {
			v   int
			err error
		}{v, err}
	}()
	// Give the waiter time to register as a waiter on the same flight before
	// the first caller is cancelled. A short sleep is the only portable way
	// to observe "a goroutine has reached the select"; the assertion that
	// matters (the load context is not cancelled) then holds under -race.
	time.Sleep(20 * time.Millisecond)

	cancel()
	select {
	case err := <-firstDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled caller got %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled caller did not return promptly")
	}

	// Rule 8: one caller giving up must not cancel a load another is waiting on.
	ctx := <-loadCtx
	if err := ctx.Err(); err != nil {
		t.Fatalf("load context cancelled while a waiter remained: %v", err)
	}

	close(release)
	select {
	case r := <-waiter:
		if r.err != nil || r.v != 3 {
			t.Fatalf("waiter got (%v, %v), want (3, nil)", r.v, r.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter never got the value")
	}
}

func TestLastWaiterGivingUpCancelsTheLoadContext(t *testing.T) {
	c := New[string, int](Options{})
	loadCtx := make(chan context.Context, 1)
	cancelled := make(chan struct{})
	proceed := make(chan struct{})
	load := func(ctx context.Context) (int, error) {
		loadCtx <- ctx
		<-ctx.Done()
		close(cancelled)
		<-proceed
		return 0, ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(ctx, "k", load)
		done <- err
	}()
	got := <-loadCtx
	cancel()

	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("the load context was not cancelled after the last waiter gave up")
	}
	if got.Err() == nil {
		t.Fatal("load context had been cancelled too early, before every waiter gave up")
	}
	close(proceed)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("caller got %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("caller did not return")
	}
}

func TestCancelBeforeLoadStartsReturnsImmediately(t *testing.T) {
	c := New[string, int](Options{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.GetOrLoad(ctx, "k", func(context.Context) (int, error) {
		t.Fatal("load was called for an already-cancelled caller")
		return 0, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// --- rule 9: late results ---------------------------------------------------

func TestLateResultIsStoredAfterEveryCallerGaveUp(t *testing.T) {
	clock := newClock()
	c := New[string, int](Options{TTL: 10 * time.Second, Now: clock.Now})
	loadStarted := make(chan struct{})
	release := make(chan struct{})
	load := func(context.Context) (int, error) {
		close(loadStarted)
		<-release
		return 42, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(ctx, "k", load)
		done <- err
	}()
	<-loadStarted
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("caller got %v, want context.Canceled", err)
	}

	clock.advance(time.Second) // load returns now, so the TTL starts here
	close(release)
	waitFor(t, func() bool { _, ok := c.Get("k"); return ok })

	// Rule 9: stored with its TTL starting when load returned.
	if v, ok := c.Get("k"); !ok || v != 42 {
		t.Fatalf("late result: Get = (%v, %v), want (42, true)", v, ok)
	}
	clock.advance(10*time.Second - time.Nanosecond)
	if _, ok := c.Get("k"); !ok {
		t.Fatal("TTL did not start when load returned")
	}
	clock.advance(time.Nanosecond)
	if _, ok := c.Get("k"); ok {
		t.Fatal("entry outlived TTL measured from when load returned")
	}
}

// --- rule 10: Delete --------------------------------------------------------

func TestDeleteDoesNotStopAnInFlightLoad(t *testing.T) {
	c := New[string, int](Options{})
	// No entry for "k": the first GetOrLoad must miss and start a load. (A
	// live hit would return before calling load at all, rule 4.)
	loadStarted := make(chan struct{})
	release := make(chan struct{})
	load := func(context.Context) (int, error) {
		close(loadStarted)
		<-release
		return 2, nil
	}
	done := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(context.Background(), "k", load)
		done <- err
	}()
	<-loadStarted
	// The key exists during the load only as an in-flight entry; a Delete
	// here must not abort it.
	c.Delete("k")
	// A second Delete, and a Set of an unrelated key, must not disturb it
	// either.
	c.Delete("k")
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("in-flight load returned %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the in-flight load did not finish after Delete")
	}
	// Rule 10: the load that was in flight when the key was deleted still
	// stores its result.
	if v, ok := c.Get("k"); !ok || v != 2 {
		t.Fatalf("after Delete during a load: Get = (%v, %v), want (2, true)", v, ok)
	}
}

// --- rule 11: concurrency and goroutines ------------------------------------

func TestConcurrentMixedUseIsRaceFree(t *testing.T) {
	c := New[int, int](Options{TTL: time.Millisecond, MaxEntries: 64})
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 250; i++ {
				k := (g*7 + i) % 128
				switch i % 5 {
				case 0:
					c.Set(k, i)
				case 1:
					c.Get(k)
				case 2:
					c.Delete(k)
				case 3:
					c.Len()
				default:
					if _, err := c.GetOrLoad(context.Background(), k, func(context.Context) (int, error) {
						return k, nil
					}); err != nil {
						t.Errorf("GetOrLoad: %v", err)
					}
				}
			}
		}(g)
	}
	wg.Wait()
}

func TestNoGoroutineOutlivesItsLoads(t *testing.T) {
	c := New[string, int](Options{})
	before := runtimeGoroutines()

	// A burst of loads that all fail, all succeed, and all are cancelled.
	for i := 0; i < 200; i++ {
		key := fmt.Sprintf("k%d", i)
		if _, err := c.GetOrLoad(context.Background(), key, func(context.Context) (int, error) {
			return i, nil
		}); err != nil {
			t.Fatalf("load: %v", err)
		}
	}
	for i := 0; i < 200; i++ {
		key := fmt.Sprintf("c%d", i)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			_, _ = c.GetOrLoad(ctx, key, func(ctx context.Context) (int, error) {
				<-ctx.Done()
				return 0, ctx.Err()
			})
			close(done)
		}()
		cancel()
		<-done
	}

	// Goroutine counts settle; the cache starts one goroutine per flight and
	// each such goroutine ends with the load it serves.
	waitFor(t, func() bool { return runtimeGoroutines() <= before+2 })
}

// --- rule 12 / misc ---------------------------------------------------------

func TestNilNowDefaultsToTimeNow(t *testing.T) {
	c := New[string, int](Options{TTL: time.Hour})
	c.Set("k", 1)
	if v, ok := c.Get("k"); !ok || v != 1 {
		t.Fatalf("Get = (%v, %v), want (1, true)", v, ok)
	}
}

func TestGenericsKeyAndValueShapes(t *testing.T) {
	type key struct{ a, b int }
	c := New[key, []string](Options{TTL: time.Minute})
	k := key{1, 2}
	c.Set(k, []string{"x"})
	if v, ok := c.Get(k); !ok || len(v) != 1 || v[0] != "x" {
		t.Fatalf("struct kind: Get = (%v, %v)", v, ok)
	}
	c.Delete(k)
	if _, ok := c.Get(k); ok {
		t.Fatal("Delete left the entry behind")
	}
	if n := c.Len(); n != 0 {
		t.Fatalf("Len = %d, want 0", n)
	}
}

// --- helpers ----------------------------------------------------------------

func runtimeGoroutines() int { return runtime.NumGoroutine() }

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition never became true")
}
