package ttlcache

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a controllable clock used by the time-based rules.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}
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

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// inflight loads for key, for leak checks.
func (c *Cache[K, V]) inFlight() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.loads)
}

// R1: an entry stored at t is live while Now() is before t+TTL; Get, GetOrLoad,
// Len all ignore an expired entry.
func TestR1Expiry(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{TTL: 10 * time.Second, Now: clk.Now})

	c.Set("k", 1)
	if got, ok := c.Get("k"); !ok || got != 1 {
		t.Fatalf("live entry: got (%v, %v), want (1, true)", got, ok)
	}
	if n := c.Len(); n != 1 {
		t.Fatalf("Len before expiry = %d, want 1", n)
	}

	clk.Advance(9*time.Second + 999*time.Millisecond)
	if _, ok := c.Get("k"); !ok {
		t.Fatal("entry expired before t+TTL")
	}

	clk.Advance(time.Millisecond) // exactly t+TTL: no longer "before"
	if got, ok := c.Get("k"); ok {
		t.Fatalf("Get at t+TTL = (%v, true), want zero,false", got)
	}
	if n := c.Len(); n != 0 {
		t.Fatalf("Len after expiry = %d, want 0", n)
	}

	var calls int32
	got, err := c.GetOrLoad(context.Background(), "k", func(context.Context) (int, error) {
		atomic.AddInt32(&calls, 1)
		return 99, nil
	})
	if err != nil || got != 99 {
		t.Fatalf("GetOrLoad after expiry = (%v, %v), want (99, nil)", got, err)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("load calls = %d, want 1 (expired entry must not be served)", n)
	}
}

// R2: Set on an existing key replaces the value and restarts the TTL.
func TestR2Replacement(t *testing.T) {
	clk := newFakeClock()
	c := New[string, string](Options{TTL: 10 * time.Second, MaxEntries: 5, Now: clk.Now})

	c.Set("k", "first")
	clk.Advance(8 * time.Second)
	c.Set("k", "second")
	if got, _ := c.Get("k"); got != "second" {
		t.Fatalf("value after replacement = %q, want %q", got, "second")
	}
	if n := c.Len(); n != 1 {
		t.Fatalf("Len after replacement = %d, want 1", n)
	}

	clk.Advance(8 * time.Second) // 16s after the first Set, 8s after the second
	if got, ok := c.Get("k"); !ok || got != "second" {
		t.Fatalf("TTL did not restart on replacement: got (%q, %v)", got, ok)
	}

	clk.Advance(2 * time.Second) // 10s after the replacement
	if _, ok := c.Get("k"); ok {
		t.Fatal("entry still live at t+TTL measured from the replacement")
	}
}

// R3: storing beyond MaxEntries evicts the least recently used LIVE entry.
// Usage comes from Set, a completed load and Get/GetOrLoad hits.
func TestR3LRUEviction(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{TTL: time.Minute, MaxEntries: 2, Now: clk.Now})

	c.Set("a", 1)
	c.Set("b", 2)
	if got, _ := c.Get("a"); got != 1 { // "a" becomes most recently used
		t.Fatalf("Get(a) = %d, want 1", got)
	}
	c.Set("c", 3) // evicts the LRU live entry, "b"

	if _, ok := c.Get("b"); ok {
		t.Fatal("b was the least recently used live entry and should be evicted")
	}
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a was recently used and should survive")
	}
	if _, ok := c.Get("c"); !ok {
		t.Fatal("c was just stored and should be present")
	}
	if n := c.Len(); n != 2 {
		t.Fatalf("Len = %d, want 2", n)
	}

	// A completed load counts as use: after loading "d", "c" is the LRU.
	var calls int32
	if _, err := c.GetOrLoad(context.Background(), "d", func(context.Context) (int, error) {
		atomic.AddInt32(&calls, 1)
		return 4, nil
	}); err != nil {
		t.Fatalf("GetOrLoad(d): %v", err)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("load calls = %d, want 1", n)
	}
	c.Set("e", 5)
	if _, ok := c.Get("c"); ok {
		t.Fatal("c should have been evicted: the load of d used d most recently")
	}

	// A GetOrLoad hit counts as use.
	c2 := New[string, int](Options{TTL: time.Minute, MaxEntries: 2, Now: clk.Now})
	c2.Set("a", 1)
	c2.Set("b", 2)
	if got, err := c2.GetOrLoad(context.Background(), "a", func(context.Context) (int, error) {
		return -1, errors.New("hit must not load")
	}); err != nil || got != 1 {
		t.Fatalf("GetOrLoad(a) hit = (%d, %v), want (1, nil)", got, err)
	}
	c2.Set("c", 3) // evicts "b", the LRU after the GetOrLoad hit on "a"
	if _, ok := c2.Get("b"); ok {
		t.Fatal("b should be evicted: a GetOrLoad hit used a most recently")
	}
}

// R3, second half: an expired entry never causes a live entry to be evicted.
func TestR3ExpiredNeverEvictsLive(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{TTL: 10 * time.Second, MaxEntries: 2, Now: clk.Now})

	c.Set("a", 1)
	clk.Advance(11 * time.Second) // "a" is expired now
	c.Set("b", 2)
	clk.Advance(time.Second)
	c.Set("c", 3) // must purge expired "a" rather than evict live "b"

	if got, ok := c.Get("b"); !ok || got != 2 {
		t.Fatalf("live b was evicted although expired a could make room: (%d, %v)", got, ok)
	}
	if _, ok := c.Get("a"); ok {
		t.Fatal("expired a should be gone")
	}
	if n := c.Len(); n != 2 {
		t.Fatalf("Len = %d, want 2", n)
	}
}

// R4: GetOrLoad on a live hit returns the cached value without calling load.
func TestR4Hits(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{TTL: time.Minute, Now: clk.Now})
	c.Set("k", 7)

	var calls int32
	got, err := c.GetOrLoad(context.Background(), "k", func(context.Context) (int, error) {
		atomic.AddInt32(&calls, 1)
		return -1, errors.New("load must not run")
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 7 {
		t.Fatalf("GetOrLoad hit = %d, want 7", got)
	}
	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Fatalf("load was called %d times on a hit, want 0", n)
	}
}

// R5: concurrent GetOrLoad callers for one key share exactly one load, and
// different keys load independently without blocking each other.
func TestR5OneLoadPerKey(t *testing.T) {
	c := New[string, int](Options{TTL: time.Minute})
	const callers = 20
	release := make(chan struct{})
	started := make(chan struct{})
	var loads int32

	var wg sync.WaitGroup
	results := make([]int, callers)
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = c.GetOrLoad(context.Background(), "shared",
				func(context.Context) (int, error) {
					if atomic.AddInt32(&loads, 1) == 1 {
						close(started)
					}
					<-release
					return 42, nil
				})
		}(i)
	}
	<-started
	time.Sleep(100 * time.Millisecond) // let the rest join the in-flight load
	if n := atomic.LoadInt32(&loads); n != 1 {
		t.Fatalf("loads in flight = %d, want exactly 1", n)
	}
	close(release)
	wg.Wait()

	if n := atomic.LoadInt32(&loads); n != 1 {
		t.Fatalf("total loads for one key = %d, want 1", n)
	}
	for i := range results {
		if errs[i] != nil || results[i] != 42 {
			t.Fatalf("caller %d got (%d, %v), want (42, nil)", i, results[i], errs[i])
		}
	}

	// Two distinct keys must be able to load at the same time.
	c2 := New[string, int](Options{TTL: time.Minute})
	inFlight := int32(0)
	gate := make(chan struct{})
	bothRunning := make(chan struct{})
	var once sync.Once
	var wg2 sync.WaitGroup
	for _, k := range []string{"x", "y"} {
		wg2.Add(1)
		go func(k string) {
			defer wg2.Done()
			_, _ = c2.GetOrLoad(context.Background(), k, func(context.Context) (int, error) {
				if atomic.AddInt32(&inFlight, 1) == 2 {
					once.Do(func() { close(bothRunning) })
				}
				<-gate
				atomic.AddInt32(&inFlight, -1)
				return 1, nil
			})
		}(k)
	}
	select {
	case <-bothRunning:
	case <-time.After(2 * time.Second):
		t.Fatal("two keys did not load concurrently, so one blocked the other")
	}
	close(gate)
	wg2.Wait()
}

// R6: a load error caches nothing, reaches every waiter, and a later GetOrLoad
// calls load again.
func TestR6LoadErrors(t *testing.T) {
	c := New[string, int](Options{TTL: time.Minute})
	wantErr := errors.New("boom")
	const callers = 10
	release := make(chan struct{})
	var loads int32

	var wg sync.WaitGroup
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = c.GetOrLoad(context.Background(), "k", func(context.Context) (int, error) {
				atomic.AddInt32(&loads, 1)
				<-release
				return 0, wantErr
			})
		}(i)
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()

	if n := atomic.LoadInt32(&loads); n != 1 {
		t.Fatalf("loads = %d, want 1", n)
	}
	for i, err := range errs {
		if !errors.Is(err, wantErr) {
			t.Fatalf("caller %d error = %v, want %v", i, err, wantErr)
		}
	}
	if n := c.Len(); n != 0 {
		t.Fatalf("Len after a failed load = %d, want 0", n)
	}

	got, err := c.GetOrLoad(context.Background(), "k", func(context.Context) (int, error) {
		return 5, nil
	})
	if err != nil || got != 5 {
		t.Fatalf("retry after an error = (%d, %v), want (5, nil)", got, err)
	}
}

// R7: a caller whose ctx is done before the load finishes returns ctx.Err()
// promptly, and the load keeps running for the other waiters.
func TestR7CallersThatGiveUp(t *testing.T) {
	c := New[string, int](Options{TTL: time.Minute})
	release := make(chan struct{})
	started := make(chan struct{})

	patient := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(context.Background(), "k", func(context.Context) (int, error) {
			close(started)
			<-release
			return 21, nil
		})
		patient <- err
	}()
	<-started // the load is in flight

	ctx, cancel := context.WithCancel(context.Background())
	quitter := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(ctx, "k", func(context.Context) (int, error) {
			return 0, errors.New("a giving-up caller must not start a second load")
		})
		quitter <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-quitter:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("giving-up caller returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("giving-up caller did not return promptly")
	}

	// The load is still running for the patient caller.
	select {
	case err := <-patient:
		t.Fatalf("the load finished although a waiter remained (err=%v)", err)
	default:
	}

	close(release)
	select {
	case err := <-patient:
		if err != nil {
			t.Fatalf("patient caller: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the load for the remaining waiter never completed")
	}
}

// R8: the context passed to load is cancelled exactly when every caller waiting
// on that load has given up.
func TestR8LoadContext(t *testing.T) {
	c := New[string, int](Options{TTL: time.Minute})
	release := make(chan struct{})
	loadCtxCh := make(chan context.Context, 1)
	started := make(chan struct{})

	// Caller 1 has no cancellation: it waits to the end.
	first := make(chan struct{})
	go func() {
		_, _ = c.GetOrLoad(context.Background(), "k", func(ctx context.Context) (int, error) {
			loadCtxCh <- ctx
			close(started)
			<-release
			return 1, nil
		})
		close(first)
	}()
	<-started
	loadCtx := <-loadCtxCh

	// Caller 2 joins the same in-flight load and is cancellable.
	ctx2, cancel2 := context.WithCancel(context.Background())
	second := make(chan struct{})
	go func() {
		_, _ = c.GetOrLoad(ctx2, "k", func(context.Context) (int, error) {
			return 0, errors.New("second caller must not start a load")
		})
		close(second)
	}()
	time.Sleep(100 * time.Millisecond)

	// Cancelling one of two waiters must not cancel the load's context.
	cancel2()
	select {
	case <-loadCtx.Done():
		t.Fatalf("load ctx was cancelled although a waiter remained: %v", loadCtx.Err())
	default:
	}

	// Now the last waiter goes: the load's context must be cancelled, even
	// though the load is still blocked inside load.
	ctx3, cancel3 := context.WithCancel(context.Background())
	third := make(chan struct{})
	go func() {
		_, _ = c.GetOrLoad(ctx3, "k", func(context.Context) (int, error) {
			return 0, errors.New("third caller must not start a load")
		})
		close(third)
	}()
	time.Sleep(50 * time.Millisecond)
	_ = first
	_ = second
	cancel3()
	// Caller 1 is still waiting, so the load ctx must still be live.
	time.Sleep(50 * time.Millisecond)
	select {
	case <-loadCtx.Done():
		t.Fatal("load ctx was cancelled while caller 1 still waited")
	default:
	}

	// A dedicated final check: with one cancellable caller only, cancelling it
	// cancels the load's context.
	t.Run("last waiter cancels the load context", func(t *testing.T) {
		c2 := New[string, int](Options{TTL: time.Minute})
		release2 := make(chan struct{})
		ctxCh := make(chan context.Context, 1)
		loadStarted := make(chan struct{})

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := c2.GetOrLoad(ctx, "z", func(lctx context.Context) (int, error) {
				ctxCh <- lctx
				close(loadStarted)
				<-release2
				return 2, nil
			})
			done <- err
		}()
		<-loadStarted
		lctx := <-ctxCh
		if lctx.Err() != nil {
			t.Fatalf("load ctx cancelled before any waiter gave up: %v", lctx.Err())
		}
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("caller returned %v, want context.Canceled", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("caller did not give up promptly")
		}
		select {
		case <-lctx.Done():
		case <-time.After(2 * time.Second):
			t.Fatal("load ctx was not cancelled when the last waiter gave up")
		}
		if !errors.Is(lctx.Err(), context.Canceled) {
			t.Fatalf("load ctx error = %v, want context.Canceled", lctx.Err())
		}
		close(release2)
	})

	close(release)
	<-first
	<-second
	<-third
}

// R9: a successful load is stored with its TTL starting when load returned,
// even when every caller has given up first.
func TestR9LateResults(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{TTL: 10 * time.Second, Now: clk.Now})

	started := make(chan struct{})
	release := make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(ctx, "k", func(context.Context) (int, error) {
			close(started)
			<-release
			return 11, nil
		})
		done <- err
	}()
	<-started
	cancel() // the only caller gives up while load is still blocked

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("caller returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("caller did not give up promptly")
	}

	close(release) // load returns after every caller is gone
	waitFor(t, 2*time.Second, "the late load result to be cached", func() bool {
		v, ok := c.Get("k")
		return ok && v == 11
	})

	// The TTL started when load returned: live just under it, gone at it.
	clk.Advance(10 * time.Second)
	if _, ok := c.Get("k"); ok {
		t.Fatal("entry outlived a TTL measured from the load's return")
	}
}

// R10: Delete removes the key but does not stop an in-flight load, whose result
// is still stored when it completes.
func TestR10DeleteDoesNotStopLoad(t *testing.T) {
	c := New[string, int](Options{TTL: time.Minute})
	release := make(chan struct{})
	started := make(chan struct{})

	done := make(chan struct{})
	go func() {
		_, _ = c.GetOrLoad(context.Background(), "k", func(context.Context) (int, error) {
			close(started)
			<-release
			return 3, nil
		})
		close(done)
	}()
	<-started
	c.Delete("k")
	if n := c.Len(); n != 0 {
		t.Fatalf("Len after Delete = %d, want 0", n)
	}

	close(release)
	waitFor(t, 2*time.Second, "the in-flight load to store its result after Delete",
		func() bool {
			v, ok := c.Get("k")
			return ok && v == 3
		})
	<-done
}

// R11: concurrent use is race-clean, leaves the cache live and transparent, and
// leaves no per-key load state or goroutine behind.
func TestR11Concurrency(t *testing.T) {
	c := New[int, int](Options{TTL: 20 * time.Millisecond, MaxEntries: 8})

	const goroutines = 32
	const keys = 4
	var loads int32

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 60; i++ {
				key := (g + i) % keys
				switch i % 4 {
				case 0:
					c.Set(key, i)
				case 1:
					c.Get(key)
				case 2:
					c.Delete(key)
				case 3:
					ctx, cancel := context.WithCancel(context.Background())
					_, _ = c.GetOrLoad(ctx, key, func(context.Context) (int, error) {
						atomic.AddInt32(&loads, 1)
						time.Sleep(200 * time.Microsecond)
						return i, nil
					})
					cancel()
				}
				c.Len()
			}
		}(g)
	}
	wg.Wait()

	// The cache is still live and usable.
	c.Set(100, 100)
	if v, ok := c.Get(100); !ok || v != 100 {
		t.Fatalf("cache unusable after the stress run: got (%d, %v)", v, ok)
	}
	if n := c.Len(); n > 8 {
		t.Fatalf("Len = %d, want at most MaxEntries", n)
	}

	// No per-key load state outlives the loads it served.
	waitFor(t, 5*time.Second, "all in-flight loads to retire", func() bool {
		return c.inFlight() == 0
	})
	if atomic.LoadInt32(&loads) == 0 {
		t.Fatal("the stress test never exercised a load")
	}
}

// R11, second half: a waiter woken by ctx.Done while the load finishes gets the
// load's result, no second load starts and nothing deadlocks.
func TestR11CancelRacesLoadCompletion(t *testing.T) {
	for i := 0; i < 300; i++ {
		c := New[string, int](Options{TTL: time.Minute})
		var loads int32
		release := make(chan struct{})
		started := make(chan struct{})

		go func() {
			_, _ = c.GetOrLoad(context.Background(), "k", func(context.Context) (int, error) {
				close(started)
				<-release
				return 1, nil
			})
		}()
		<-started

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := c.GetOrLoad(ctx, "k", func(context.Context) (int, error) {
				atomic.AddInt32(&loads, 1)
				return 2, nil
			})
			done <- err
		}()
		time.Sleep(100 * time.Microsecond)
		cancel() // races against the completion right below
		close(release)

		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("iteration %d: waiter did not return (deadlock)", i)
		}
		waitFor(t, 2*time.Second, "the key's load to retire", func() bool {
			return c.inFlight() == 0
		})
		if n := atomic.LoadInt32(&loads); n != 0 {
			t.Fatalf("iteration %d: a second load was started %d time(s)", i, n)
		}
	}
}

// A nil Now means time.Now, and TTL 0 means entries never expire.
func TestZeroOptionsDefaults(t *testing.T) {
	c := New[string, int](Options{})
	c.Set("k", 1)
	if got, ok := c.Get("k"); !ok || got != 1 {
		t.Fatalf("default options: got (%d, %v), want (1, true)", got, ok)
	}
	time.Sleep(2 * time.Millisecond)
	if _, ok := c.Get("k"); !ok {
		t.Fatal("TTL 0 must mean entries never expire")
	}
}

// A nil MaxEntries (0) means unbounded.
func TestUnboundedWhenMaxEntriesZero(t *testing.T) {
	c := New[int, int](Options{TTL: time.Minute})
	for i := 0; i < 100; i++ {
		c.Set(i, i)
	}
	if n := c.Len(); n != 100 {
		t.Fatalf("Len = %d, want 100 (unbounded)", n)
	}
}

// R7/R8/R11 sharpened: many cancellable callers against one blocked load. Each
// must return promptly with ctx.Err(), the load must survive while even one
// waiter remains, and the load's context must be cancelled exactly when the last
// of them goes. The load finally returning must satisfy a fresh caller with no
// second load.
func TestGiveUpPromptlyKeepsLoadAliveForRemainingWaiters(t *testing.T) {
	c := New[string, int](Options{TTL: time.Minute})
	release := make(chan struct{})
	ctxCh := make(chan context.Context, 1)
	started := make(chan struct{})
	var loads int32

	// The patient loader owns the in-flight load.
	patientDone := make(chan int, 1)
	go func() {
		v, _ := c.GetOrLoad(context.Background(), "k", func(ctx context.Context) (int, error) {
			ctxCh <- ctx
			close(started)
			<-release
			return 7, nil
		})
		patientDone <- v
	}()
	<-started
	loadCtx := <-ctxCh

	const quitters = 8
	cancels := make([]context.CancelFunc, quitters)
	quitterDone := make([]chan error, quitters)
	for i := 0; i < quitters; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancels[i] = cancel
		quitterDone[i] = make(chan error, 1)
		go func(i int) {
			_, err := c.GetOrLoad(ctx, "k", func(context.Context) (int, error) {
				atomic.AddInt32(&loads, 1)
				return 0, errors.New("no second load may start")
			})
			quitterDone[i] <- err
		}(i)
	}
	time.Sleep(100 * time.Millisecond) // all quitters joined the load

	for i := 0; i < quitters; i++ {
		cancels[i]()
	}
	for i := 0; i < quitters; i++ {
		select {
		case err := <-quitterDone[i]:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("quitter %d returned %v, want context.Canceled", i, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("quitter %d did not return promptly", i)
		}
	}

	// Every cancellable caller has gone, but the patient loader remains, so the
	// load's context is still live.
	select {
	case <-loadCtx.Done():
		t.Fatal("load ctx was cancelled although the patient waiter remained")
	default:
	}
	if n := atomic.LoadInt32(&loads); n != 0 {
		t.Fatalf("a quitter started %d second load(s)", n)
	}

	close(release)
	select {
	case v := <-patientDone:
		if v != 7 {
			t.Fatalf("patient loader got %d, want 7", v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the patient loader never got its value")
	}

	// The load's context is cancelled once the load has returned.
	waitFor(t, 2*time.Second, "the load ctx to be cancelled after the load returned", func() bool {
		return loadCtx.Err() != nil
	})
}

// R5/R11 sharpened: a caller that misses while a load is in flight must never
// start a second load, however its ctx behaves.
func TestNoSecondLoadFromWaiters(t *testing.T) {
	c := New[string, int](Options{TTL: time.Minute})
	for i := 0; i < 100; i++ {
		var loads int32
		release := make(chan struct{})
		started := make(chan struct{})
		go func() {
			_, _ = c.GetOrLoad(context.Background(), "k", func(context.Context) (int, error) {
				atomic.AddInt32(&loads, 1)
				close(started)
				<-release
				return i, nil
			})
		}()
		<-started

		var wg sync.WaitGroup
		for j := 0; j < 4; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithCancel(context.Background())
				go func() { time.Sleep(50 * time.Microsecond); cancel() }()
				_, _ = c.GetOrLoad(ctx, "k", func(context.Context) (int, error) {
					atomic.AddInt32(&loads, 1)
					return 0, errors.New("waiter started a second load")
				})
			}()
		}
		time.Sleep(200 * time.Microsecond)
		close(release)
		wg.Wait()
		waitFor(t, 2*time.Second, "the load to retire", func() bool { return c.inFlight() == 0 })

		if n := atomic.LoadInt32(&loads); n != 1 {
			t.Fatalf("iteration %d: loads = %d, want exactly 1", i, n)
		}
		c.Delete("k")
	}
}
