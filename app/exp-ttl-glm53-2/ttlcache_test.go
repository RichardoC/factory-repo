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

// fakeClock is a controllable clock for deterministic TTL tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Unix(1700000000, 0).UTC()}
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) Add(d time.Duration) {
	f.mu.Lock()
	f.t = f.t.Add(d)
	f.mu.Unlock()
}

type res struct {
	v   int
	err error
}

// Rule 1: expiry. Live while Now() is before t+TTL; expired entries are
// never returned and not counted by Len; a TTL of 0 means never expire.
func TestExpiry(t *testing.T) {
	ttl := time.Minute
	clk := newFakeClock()
	c := New[string, int](Options{TTL: ttl, Now: clk.Now})

	c.Set("a", 1)
	if v, ok := c.Get("a"); !ok || v != 1 {
		t.Fatalf("Get right after Set = (%v, %v), want (1, true)", v, ok)
	}
	// Live strictly before t+TTL.
	clk.Add(ttl - time.Nanosecond)
	if v, ok := c.Get("a"); !ok || v != 1 {
		t.Fatalf("Get at t+TTL-1ns = (%v, %v), want live", v, ok)
	}
	// Expired at exactly t+TTL.
	clk.Add(time.Nanosecond)
	if _, ok := c.Get("a"); ok {
		t.Fatal("Get at t+TTL returned an expired entry")
	}
	if c.Len() != 0 {
		t.Fatalf("Len counts expired entries: %d, want 0", c.Len())
	}
	// GetOrLoad never returns an expired entry: it must load afresh.
	calls := 0
	v, err := c.GetOrLoad(context.Background(), "a", func(context.Context) (int, error) {
		calls++
		return 2, nil
	})
	if err != nil || v != 2 || calls != 1 {
		t.Fatalf("GetOrLoad on expired entry = (%v, %v), calls=%d", v, err, calls)
	}
	// And the fresh value is cached under the current clock.
	if v, ok := c.Get("a"); !ok || v != 2 {
		t.Fatalf("loaded value not cached: (%v, %v)", v, ok)
	}
}

// Rule 1: TTL 0 means entries never expire.
func TestZeroTTLNeverExpires(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{Now: clk.Now})
	c.Set("a", 1)
	clk.Add(100 * 365 * 24 * time.Hour)
	if v, ok := c.Get("a"); !ok || v != 1 {
		t.Fatalf("entry with TTL 0 expired: (%v, %v)", v, ok)
	}
	if v, err := c.GetOrLoad(context.Background(), "a", func(context.Context) (int, error) {
		return 0, errors.New("must not be called")
	}); err != nil || v != 1 {
		t.Fatalf("GetOrLoad on never-expiring hit = (%v, %v)", v, err)
	}
}

// Rule 2: Set on an existing key replaces the value and restarts the TTL.
func TestReplaceRestartsTTL(t *testing.T) {
	ttl := time.Minute
	clk := newFakeClock()
	c := New[string, int](Options{TTL: ttl, Now: clk.Now})

	c.Set("a", 1)
	clk.Add(30 * time.Second)
	c.Set("a", 2)
	if v, ok := c.Get("a"); !ok || v != 2 {
		t.Fatalf("Set did not replace the value: (%v, %v)", v, ok)
	}
	clk.Add(30 * time.Second) // t0+60s: the original TTL would have expired
	if _, ok := c.Get("a"); !ok {
		t.Fatal("Set did not restart the TTL")
	}
	clk.Add(30 * time.Second) // t0+90s = second store + 60s: now expired
	if _, ok := c.Get("a"); ok {
		t.Fatal("entry still live after the restarted TTL")
	}
}

// Rule 3: LRU eviction, including recency refresh on a Get hit.
func TestLRUEviction(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{TTL: time.Hour, MaxEntries: 2, Now: clk.Now})

	c.Set("a", 1)
	c.Set("b", 2)
	c.Set("c", 3) // would leave 3 live entries: evict a, the least recently used
	if _, ok := c.Get("a"); ok {
		t.Fatal("a was not evicted")
	}
	if v, ok := c.Get("b"); !ok || v != 2 {
		t.Fatalf("b = (%v, %v), want live", v, ok)
	}
	if v, ok := c.Get("c"); !ok || v != 3 {
		t.Fatalf("c = (%v, %v), want live", v, ok)
	}
	// A hit counts as a use: refreshing b makes c the LRU entry.
	c.Get("b")
	c.Set("d", 4) // evicts c
	if _, ok := c.Get("c"); ok {
		t.Fatal("c was not evicted after b was refreshed")
	}
	if v, ok := c.Get("b"); !ok || v != 2 {
		t.Fatalf("b = (%v, %v), want live", v, ok)
	}
	if v, ok := c.Get("d"); !ok || v != 4 {
		t.Fatalf("d = (%v, %v), want live", v, ok)
	}
}

// Rule 3: expired entries never cause a live entry to be evicted.
func TestExpiredEntriesNeverEvict(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{TTL: time.Minute, MaxEntries: 2, Now: clk.Now})

	c.Set("a", 1)
	clk.Add(time.Hour) // a has expired but is still stored internally
	c.Set("b", 2)      // live count was 0: nothing may be evicted
	c.Set("c", 3)      // live count was 1: another entry fits
	if v, ok := c.Get("b"); !ok || v != 2 {
		t.Fatalf("b evicted although only an expired entry occupied the cache: (%v, %v)", v, ok)
	}
	if v, ok := c.Get("c"); !ok || v != 3 {
		t.Fatalf("c = (%v, %v), want live", v, ok)
	}
	// Now two live entries (b, c), so a new store evicts the LRU live one.
	c.Set("d", 4)
	if _, ok := c.Get("b"); ok {
		t.Fatal("b should have been evicted once two live entries filled the bound")
	}
	if v, ok := c.Get("c"); !ok || v != 3 {
		t.Fatalf("c = (%v, %v), want live", v, ok)
	}
	if v, ok := c.Get("d"); !ok || v != 4 {
		t.Fatalf("d = (%v, %v), want live", v, ok)
	}
}

// Rule 3: a completed load counts as a use and its store also evicts.
func TestLoadedEntryEviction(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{TTL: time.Hour, MaxEntries: 1, Now: clk.Now})

	gate := make(chan struct{})
	loaded := make(chan int, 1)
	go func() {
		v, err := c.GetOrLoad(context.Background(), "k1", func(context.Context) (int, error) {
			<-gate
			return 1, nil
		})
		if err != nil {
			t.Errorf("GetOrLoad k1: %v", err)
		}
		loaded <- v
	}()
	close(gate)
	if v := <-loaded; v != 1 {
		t.Fatalf("GetOrLoad k1 = %d, want 1", v)
	}
	// Storing k2 while k1 fills the single-entry bound evicts k1.
	v, err := c.GetOrLoad(context.Background(), "k2", func(context.Context) (int, error) {
		return 2, nil
	})
	if err != nil || v != 2 {
		t.Fatalf("GetOrLoad k2 = (%v, %v)", v, err)
	}
	if _, ok := c.Get("k1"); ok {
		t.Fatal("load-completed entry k1 was not subject to MaxEntries eviction")
	}
	if v, ok := c.Get("k2"); !ok || v != 2 {
		t.Fatalf("k2 = (%v, %v), want live", v, ok)
	}
}

// Rule 4: a live hit serves from the cache without calling load.
func TestHitDoesNotLoad(t *testing.T) {
	c := New[string, int](Options{})
	c.Set("k", 5)
	calls := 0
	v, err := c.GetOrLoad(context.Background(), "k", func(context.Context) (int, error) {
		calls++
		return 0, nil
	})
	if calls != 0 {
		t.Fatalf("load called on a live hit %d times", calls)
	}
	if err != nil || v != 5 {
		t.Fatalf("GetOrLoad hit = (%v, %v), want (5, nil)", v, err)
	}
}

// Rule 5: concurrent callers for one key share a single call of load.
func TestSingleFlight(t *testing.T) {
	c := New[int, int](Options{MaxEntries: 100})
	var calls atomic.Int64
	release := make(chan struct{})
	load := func(context.Context) (int, error) {
		calls.Add(1)
		<-release
		return 42, nil
	}
	const n = 5
	results := make(chan res, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := c.GetOrLoad(context.Background(), 7, load)
			results <- res{v, err}
		}()
	}
	time.Sleep(20 * time.Millisecond) // let every caller join the in-flight load
	close(release)
	wg.Wait()
	close(results)
	for r := range results {
		if r.err != nil || r.v != 42 {
			t.Fatalf("waiter got (%v, %v), want (42, nil)", r.v, r.err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("load called %d times for concurrent same-key callers, want 1", got)
	}
}

// Rule 5: loads for different keys run independently, without blocking
// each other.
func TestDifferentKeysLoadIndependently(t *testing.T) {
	c := New[int, int](Options{MaxEntries: 100})
	releaseA := make(chan struct{})
	releaseB := make(chan struct{})
	resA := make(chan res, 1)
	resB := make(chan res, 1)
	startedK := make(chan int, 2)
	go func() {
		v, err := c.GetOrLoad(context.Background(), 1, func(context.Context) (int, error) {
			startedK <- 1
			<-releaseA
			return 10, nil
		})
		resA <- res{v, err}
	}()
	go func() {
		v, err := c.GetOrLoad(context.Background(), 2, func(context.Context) (int, error) {
			startedK <- 2
			<-releaseB
			return 20, nil
		})
		resB <- res{v, err}
	}()
	<-startedK
	<-startedK
	close(releaseA)
	select {
	case r := <-resA:
		if r.err != nil || r.v != 10 {
			t.Fatalf("key 1 = (%v, %v)", r.v, r.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("key 1 blocked behind key 2's load")
	}
	select {
	case r := <-resB:
		t.Fatalf("key 2 finished while its own load was still blocked: %+v", r)
	default:
	}
	close(releaseB)
	select {
	case r := <-resB:
		if r.err != nil || r.v != 20 {
			t.Fatalf("key 2 = (%v, %v)", r.v, r.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("key 2 did not finish after its release")
	}
}

// Rule 6: a load error is returned to every waiter, caches nothing, and a
// later call loads again.
func TestLoadErrorsSharedAndNotCached(t *testing.T) {
	c := New[string, int](Options{})
	sentinel := errors.New("boom")
	var calls atomic.Int64
	release := make(chan struct{})
	load := func(context.Context) (int, error) {
		calls.Add(1)
		<-release
		return 0, sentinel
	}
	const n = 3
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.GetOrLoad(context.Background(), "k", load)
			errs <- err
		}()
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if !errors.Is(err, sentinel) {
			t.Fatalf("waiter got %v, want the load's error %v", err, sentinel)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("load called %d times, want 1 (shared failure)", got)
	}
	if c.Len() != 0 {
		t.Fatalf("failed load left %d entries", c.Len())
	}
	if _, ok := c.Get("k"); ok {
		t.Fatal("failed load cached a value")
	}
	// A later GetOrLoad retries the load.
	v, err := c.GetOrLoad(context.Background(), "k", func(context.Context) (int, error) {
		calls.Add(1)
		return 3, nil
	})
	if err != nil || v != 3 {
		t.Fatalf("retry = (%v, %v)", v, err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("load called %d times after retry, want 2", got)
	}
	if v, ok := c.Get("k"); !ok || v != 3 {
		t.Fatalf("retry result not cached: (%v, %v)", v, ok)
	}
}

// Rules 7 + 8: a caller whose ctx is done returns ctx.Err() promptly; the
// load keeps running for the others and its context is not cancelled while a
// waiter remains; loads for the key keep to one call in total.
func TestCancelledCallerSparesLoadContext(t *testing.T) {
	c := New[string, int](Options{})
	var calls atomic.Int64
	started := make(chan struct{})
	release := make(chan struct{})
	ctxSeen := make(chan error, 1)
	load := func(lctx context.Context) (int, error) {
		calls.Add(1)
		close(started)
		<-release
		ctxSeen <- lctx.Err() // observed while caller B is still waiting
		return 7, nil
	}
	ctxA, cancelA := context.WithCancel(context.Background())
	resA := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(ctxA, "k", load)
		resA <- err
	}()
	<-started
	// Caller B joins the in-flight load, then A gives up.
	resB := make(chan res, 1)
	go func() {
		v, err := c.GetOrLoad(context.Background(), "k", load)
		resB <- res{v, err}
	}()
	time.Sleep(20 * time.Millisecond) // let B join
	cancelA()
	select {
	case err := <-resA:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled caller got %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled caller did not return promptly")
	}
	// One of several callers cancelled must not cancel the load's context.
	close(release)
	select {
	case err := <-ctxSeen:
		if err != nil {
			t.Fatalf("load context cancelled while a waiter remained: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("load did not finish")
	}
	select {
	case r := <-resB:
		if r.err != nil || r.v != 7 {
			t.Fatalf("remaining waiter got (%v, %v)", r.v, r.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("remaining waiter never got the value")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("load ran %d times, want 1", got)
	}
}

// Rules 7 + 8: when every caller waiting on a load has given up, the load's
// context is cancelled; the aborted result is not cached and a later call
// loads again.
func TestAllCallersGiveUpCancelsLoadContext(t *testing.T) {
	c := New[string, int](Options{})
	var calls atomic.Int64
	started := make(chan struct{})
	seen := make(chan error, 1)
	load := func(lctx context.Context) (int, error) {
		calls.Add(1)
		close(started)
		<-lctx.Done()
		seen <- lctx.Err()
		return 0, lctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(ctx, "k", load)
		got <- err
	}()
	<-started
	cancel()
	select {
	case err := <-got:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("caller got %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("caller did not return promptly after cancel")
	}
	select {
	case err := <-seen:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("load context = %v, want cancelled after every caller gave up", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("load context was never cancelled")
	}
	time.Sleep(20 * time.Millisecond) // let the abandoned load wind down
	if _, ok := c.Get("k"); ok {
		t.Fatal("aborted load cached a value")
	}
	// A later call loads again.
	v, err := c.GetOrLoad(context.Background(), "k", func(context.Context) (int, error) {
		calls.Add(1)
		return 3, nil
	})
	if err != nil || v != 3 {
		t.Fatalf("later load = (%v, %v)", v, err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("load ran %d times, want 2", got)
	}
}

// Rule 9: a result delivered after every caller gave up is still stored,
// with its TTL starting when the load returned.
func TestLateResultStoredTTLFromReturn(t *testing.T) {
	ttl := time.Minute
	clk := newFakeClock()
	c := New[string, int](Options{TTL: ttl, Now: clk.Now})
	started := make(chan struct{})
	release := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	caller := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(ctx, "k", func(context.Context) (int, error) {
			close(started)
			<-release
			return 7, nil
		})
		caller <- err
	}()
	<-started
	cancel()
	select {
	case err := <-caller:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("caller got %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("caller did not return promptly after cancel")
	}
	// Time moves on while the abandoned load keeps running.
	clk.Add(50 * time.Second)
	close(release)
	// The late result is stored (TTL from load return, i.e. from now).
	deadline := time.Now().Add(2 * time.Second)
	for {
		if v, ok := c.Get("k"); ok {
			if v != 7 {
				t.Fatalf("stored value = %d, want 7", v)
			}
			if c.Len() != 1 {
				t.Fatalf("Len = %d, want 1", c.Len())
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("late load result was not stored")
		}
		time.Sleep(time.Millisecond)
	}
	// Live until (load-return time + TTL), expired at exactly that instant.
	clk.Add(ttl - time.Nanosecond)
	if v, ok := c.Get("k"); !ok || v != 7 {
		t.Fatal("TTL did not start at load return: unreachable entry")
	}
	clk.Add(time.Nanosecond)
	if _, ok := c.Get("k"); ok {
		t.Fatal("late-stored entry did not expire after TTL")
	}
}

// Rule 10: Delete removes the key, does not panic on missing keys, and does
// not stop an in-flight load: its result is still stored when it completes.
func TestDeleteDuringInFlightLoad(t *testing.T) {
	c := New[string, int](Options{})
	c.Set("gone", 0)
	c.Set("there", 0)
	c.Delete("gone")
	if _, ok := c.Get("gone"); ok {
		t.Fatal("Delete did not remove the key")
	}
	if v, ok := c.Get("there"); !ok || v != 0 {
		t.Fatalf("Delete removed the wrong key: (%v, %v)", v, ok)
	}
	c.Delete("missing") // no panic, no effect
	if c.Len() != 1 {
		t.Fatalf("Len = %d, want 1", c.Len())
	}

	started := make(chan struct{})
	release := make(chan struct{})
	waiter := make(chan res, 1)
	go func() {
		v, err := c.GetOrLoad(context.Background(), "k", func(context.Context) (int, error) {
			close(started)
			<-release
			return 9, nil
		})
		waiter <- res{v, err}
	}()
	<-started
	c.Delete("k") // must not stop the load
	if _, ok := c.Get("k"); ok {
		t.Fatal("key visible while its load is in flight")
	}
	close(release)
	select {
	case r := <-waiter:
		if r.err != nil || r.v != 9 {
			t.Fatalf("delete interrupted the load: waiter got (%v, %v)", r.v, r.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter never returned")
	}
	// The load's result was still stored when it completed.
	if v, ok := c.Get("k"); !ok || v != 9 {
		t.Fatalf("in-flight load result not stored after Delete: (%v, %v)", v, ok)
	}
}

// Rule 11: safe for concurrent use by mixed operations (race-detector
// territory) and bounded by MaxEntries even under churn.
func TestConcurrentMixedOperations(t *testing.T) {
	c := New[int, int](Options{TTL: 5 * time.Millisecond, MaxEntries: 50})
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				k := (w*31 + i) % 64
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
					c.GetOrLoad(context.Background(), k, func(context.Context) (int, error) {
						return i, nil
					})
				}
			}
		}(w)
	}
	wg.Wait()
	if l := c.Len(); l > 50 {
		t.Fatalf("Len = %d exceeds MaxEntries 50", l)
	}
}

// Rule 11: no goroutine the cache starts outlives the loads it serves.
func TestNoGoroutinesOutliveLoads(t *testing.T) {
	base := runtime.NumGoroutine()
	c := New[int, int](Options{})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = c.GetOrLoad(context.Background(), i, func(context.Context) (int, error) {
				return i, nil
			})
		}(i)
	}
	wg.Wait()
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > base && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > base {
		t.Fatalf("goroutines still running after loads finished: %d > %d", n, base)
	}
}

// Unbounded (Options zero) keeps everything; Len counts live entries.
func TestUnboundedZeroOptions(t *testing.T) {
	clk := newFakeClock()
	c := New[int, int](Options{Now: clk.Now})
	for i := 0; i < 100; i++ {
		c.Set(i, i)
	}
	if c.Len() != 100 {
		t.Fatalf("unbounded cache Len = %d, want 100", c.Len())
	}
	for i := 0; i < 100; i++ {
		if v, ok := c.Get(i); !ok || v != i {
			t.Fatalf("Get(%d) = (%v, %v)", i, v, ok)
		}
	}
}
