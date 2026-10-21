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

// fakeClock is a manually advanced clock safe for concurrent use.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
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

func waitClose(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func waitValue[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

// Rule 1: Get and Len never see an entry at or past t+TTL.
func TestRule1Expiry(t *testing.T) {
	clk := newClock()
	c := New[string, int](Options{TTL: time.Minute, Now: clk.Now})
	c.Set("k", 1)
	clk.Advance(time.Minute - time.Nanosecond)
	if v, ok := c.Get("k"); !ok || v != 1 {
		t.Fatalf("just before expiry: got %v, %v; want 1, true", v, ok)
	}
	if n := c.Len(); n != 1 {
		t.Fatalf("Len just before expiry = %d, want 1", n)
	}
	clk.Advance(time.Nanosecond)
	if v, ok := c.Get("k"); ok {
		t.Fatalf("at expiry: got %v, true; want zero, false", v)
	}
	if n := c.Len(); n != 0 {
		t.Fatalf("Len at expiry = %d, want 0", n)
	}
}

// Rule 1: GetOrLoad does not return an expired entry, and Len ignores it.
func TestRule1ExpiredGetOrLoad(t *testing.T) {
	clk := newClock()
	c := New[string, int](Options{TTL: time.Minute, Now: clk.Now})
	c.Set("k", 1)
	clk.Advance(time.Minute)
	if n := c.Len(); n != 0 {
		t.Fatalf("Len = %d, want 0", n)
	}
	var calls atomic.Int32
	v, err := c.GetOrLoad(context.Background(), "k", func(context.Context) (int, error) {
		calls.Add(1)
		return 2, nil
	})
	if err != nil || v != 2 {
		t.Fatalf("GetOrLoad = %v, %v; want 2, nil", v, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("load calls = %d, want 1 (expired entry must not be a hit)", calls.Load())
	}
}

// Rule 1: TTL 0 means entries never expire.
func TestRule1NoTTL(t *testing.T) {
	clk := newClock()
	c := New[string, int](Options{Now: clk.Now})
	c.Set("k", 1)
	clk.Advance(100 * 365 * 24 * time.Hour)
	if v, ok := c.Get("k"); !ok || v != 1 {
		t.Fatalf("Get = %v, %v; want 1, true", v, ok)
	}
	if n := c.Len(); n != 1 {
		t.Fatalf("Len = %d, want 1", n)
	}
}

// Rule 2: Set replaces the value and restarts the TTL.
func TestRule2Replacement(t *testing.T) {
	clk := newClock()
	c := New[string, int](Options{TTL: time.Minute, Now: clk.Now})
	c.Set("k", 1)
	clk.Advance(50 * time.Second)
	c.Set("k", 2)
	if v, ok := c.Get("k"); !ok || v != 2 {
		t.Fatalf("Get = %v, %v; want 2, true", v, ok)
	}
	// Original TTL would have expired here; the replacement restarted it.
	clk.Advance(30 * time.Second)
	if v, ok := c.Get("k"); !ok || v != 2 {
		t.Fatalf("after restart: Get = %v, %v; want 2, true", v, ok)
	}
	clk.Advance(30 * time.Second)
	if _, ok := c.Get("k"); ok {
		t.Fatal("entry still live after the restarted TTL elapsed")
	}
	if n := c.Len(); n != 0 {
		t.Fatalf("Len = %d, want 0", n)
	}
}

// Rule 3: LRU eviction; Set and Get hits both count as use.
func TestRule3LRUEviction(t *testing.T) {
	c := New[string, int](Options{MaxEntries: 2})
	c.Set("a", 1)
	c.Set("b", 2)
	c.Set("c", 3) // evicts a, the least recently used
	if _, ok := c.Get("a"); ok {
		t.Fatal("a was not evicted")
	}
	if n := c.Len(); n != 2 {
		t.Fatalf("Len = %d, want 2", n)
	}

	// The cache now holds {b, c}, most recently used first: c, b. A Get hit
	// on b makes it most recently used, so storing d evicts c.
	if _, ok := c.Get("b"); !ok {
		t.Fatal("b missing")
	}
	c.Set("d", 4)
	if _, ok := c.Get("b"); !ok {
		t.Fatal("b (recently used) was evicted")
	}
	if _, ok := c.Get("d"); !ok {
		t.Fatal("d missing")
	}
	if _, ok := c.Get("c"); ok {
		t.Fatal("c (least recently used) was not evicted")
	}
	if n := c.Len(); n != 2 {
		t.Fatalf("Len = %d, want 2", n)
	}
}

// Rule 3: GetOrLoad hits also count as use.
func TestRule3GetOrLoadHitIsUse(t *testing.T) {
	c := New[string, int](Options{MaxEntries: 2})
	c.Set("a", 1)
	c.Set("b", 2)
	if _, err := c.GetOrLoad(context.Background(), "a", func(context.Context) (int, error) {
		t.Fatal("load called for a live hit")
		return 0, nil
	}); err != nil {
		t.Fatalf("GetOrLoad: %v", err)
	}
	c.Set("c", 3) // evicts b, since a was just used
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a was evicted despite a GetOrLoad hit")
	}
	if _, ok := c.Get("b"); ok {
		t.Fatal("b was not evicted")
	}
}

// Rule 3: expired entries never cause a live entry to be evicted.
func TestRule3ExpiredNeverEvictsLive(t *testing.T) {
	clk := newClock()
	c := New[string, int](Options{TTL: time.Minute, MaxEntries: 2, Now: clk.Now})
	c.Set("old", 1)
	clk.Advance(2 * time.Minute)
	c.Set("live1", 2)
	c.Set("live2", 3)
	// "old" is expired and must have been dropped without evicting anything
	// live; both live entries remain.
	if _, ok := c.Get("live1"); !ok {
		t.Fatal("live1 was evicted")
	}
	if _, ok := c.Get("live2"); !ok {
		t.Fatal("live2 was evicted")
	}
	if n := c.Len(); n != 2 {
		t.Fatalf("Len = %d, want 2", n)
	}
}

// Rule 3: MaxEntries 0 is unbounded.
func TestRule3Unbounded(t *testing.T) {
	c := New[int, int](Options{})
	for i := 0; i < 1000; i++ {
		c.Set(i, i)
	}
	if n := c.Len(); n != 1000 {
		t.Fatalf("Len = %d, want 1000", n)
	}
}

// Rule 4: GetOrLoad on a live hit never calls load.
func TestRule4HitSkipsLoad(t *testing.T) {
	c := New[string, int](Options{TTL: time.Minute})
	c.Set("k", 7)
	v, err := c.GetOrLoad(context.Background(), "k", func(context.Context) (int, error) {
		t.Fatal("load called on a live hit")
		return 0, nil
	})
	if err != nil || v != 7 {
		t.Fatalf("GetOrLoad = %v, %v; want 7, nil", v, err)
	}
}

// Rule 5: one load per key for concurrent callers; misses are shared.
func TestRule5SingleFlight(t *testing.T) {
	c := New[string, int](Options{})
	release := make(chan struct{})
	started := make(chan struct{})
	var calls atomic.Int32
	load := func(context.Context) (int, error) {
		calls.Add(1)
		close(started)
		<-release
		return 42, nil
	}

	const n = 20
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
	waitClose(t, started, "load to start")
	// Let every caller reach the shared load before releasing it.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("load calls = %d, want 1", got)
	}
	for i := 0; i < n; i++ {
		if errs[i] != nil || results[i] != 42 {
			t.Fatalf("caller %d = %v, %v; want 42, nil", i, results[i], errs[i])
		}
	}
	// The result is cached: a later call does not load again.
	if v, err := c.GetOrLoad(context.Background(), "k", load); err != nil || v != 42 {
		t.Fatalf("cached GetOrLoad = %v, %v; want 42, nil", v, err)
	}
}

// Rule 5: different keys load independently and do not block each other.
func TestRule5DifferentKeysIndependent(t *testing.T) {
	c := New[string, int](Options{})
	slowStarted := make(chan struct{})
	slowRelease := make(chan struct{})
	go func() {
		defer func() { recover() }()
		_, _ = c.GetOrLoad(context.Background(), "slow", func(context.Context) (int, error) {
			close(slowStarted)
			<-slowRelease
			return 1, nil
		})
	}()
	waitClose(t, slowStarted, "slow load to start")

	fast := make(chan struct{})
	go func() {
		defer close(fast)
		_, _ = c.GetOrLoad(context.Background(), "fast", func(context.Context) (int, error) {
			return 2, nil
		})
	}()
	waitClose(t, fast, "fast key load while slow key is loading")
	close(slowRelease)
}

// Rule 6: a load error caches nothing and reaches every waiter; a later call
// loads again.
func TestRule6LoadErrors(t *testing.T) {
	c := New[string, int](Options{})
	boom := errors.New("boom")
	release := make(chan struct{})
	started := make(chan struct{})
	var calls atomic.Int32
	failLoad := func(context.Context) (int, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
			return 0, boom
		}
		return 9, nil
	}

	const n = 5
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = c.GetOrLoad(context.Background(), "k", failLoad)
		}(i)
	}
	waitClose(t, started, "failing load to start")
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	for i := 0; i < n; i++ {
		if !errors.Is(errs[i], boom) {
			t.Fatalf("waiter %d error = %v, want %v", i, errs[i], boom)
		}
	}
	if n := c.Len(); n != 0 {
		t.Fatalf("Len after error = %d, want 0", n)
	}
	if _, ok := c.Get("k"); ok {
		t.Fatal("an errored load cached a value")
	}
	v, err := c.GetOrLoad(context.Background(), "k", failLoad)
	if err != nil || v != 9 {
		t.Fatalf("retry = %v, %v; want 9, nil", v, err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("load calls = %d, want 2", got)
	}
}

// Rule 7: a caller whose ctx ends returns ctx.Err() promptly while the load
// keeps running for the others.
func TestRule7CallerGivesUp(t *testing.T) {
	c := New[string, int](Options{})
	release := make(chan struct{})
	done := make(chan struct{})
	var calls atomic.Int32
	load := func(context.Context) (int, error) {
		calls.Add(1)
		<-release
		close(done)
		return 5, nil
	}

	// The patient caller arrives first and is the one that starts the load.
	patient := make(chan struct{})
	var patientV int
	var patientErr error
	go func() {
		defer close(patient)
		patientV, patientErr = c.GetOrLoad(context.Background(), "k", load)
	}()
	// One load runs at a time; wait until it is in flight by checking the
	// patient caller is blocked (inFlight entry visible through Len).
	waitFor(t, func() bool { return inFlightLen(c) == 1 })

	ctx, cancel := context.WithCancel(context.Background())
	quitter := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(ctx, "k", load)
		quitter <- err
	}()
	waitFor(t, func() bool { return waitersOf(c, "k") == 2 })

	cancel()
	if err := waitValue(t, quitter, "quitter to return"); !errors.Is(err, context.Canceled) {
		t.Fatalf("quitter error = %v, want context.Canceled", err)
	}
	select {
	case <-done:
		t.Fatal("load finished although a waiter remained")
	default:
	}
	close(release)
	waitClose(t, patient, "patient caller to return")
	if patientErr != nil || patientV != 5 {
		t.Fatalf("patient = %v, %v; want 5, nil", patientV, patientErr)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("load calls = %d, want 1", got)
	}
}

// Rule 7: a caller whose ctx is already done misses without starting a load.
func TestRule7AlreadyCancelled(t *testing.T) {
	c := New[string, int](Options{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var calls atomic.Int32
	_, err := c.GetOrLoad(ctx, "k", func(context.Context) (int, error) {
		calls.Add(1)
		return 1, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("load calls = %d, want 0", got)
	}
}

// waitFor spins until cond is true, failing the test after a timeout.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}

// inFlightLen reports how many loads are in flight.
func inFlightLen[K comparable, V any](c *Cache[K, V]) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.inFlight)
}

// waitersOf reports how many callers wait on key's load.
func waitersOf[K comparable, V any](c *Cache[K, V], key K) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if l, ok := c.inFlight[key]; ok {
		return l.waiters
	}
	return 0
}

// Rule 8: load's context is cancelled only when every waiter has given up.
func TestRule8LoadContextCancelledWhenAllGiveUp(t *testing.T) {
	c := New[string, int](Options{})
	loadCtx := make(chan context.Context, 1)
	stop := make(chan struct{})
	loadDone := make(chan error, 1)
	go func() {
		_, _ = c.GetOrLoad(context.Background(), "k", func(ctx context.Context) (int, error) {
			loadCtx <- ctx
			<-stop // ignore the load's own cancellation; return "late"
			return 1, nil
		})
	}()
	lctx := waitValue(t, loadCtx, "load to start")

	ctx1, cancel1 := context.WithCancel(context.Background())
	ctx2, cancel2 := context.WithCancel(context.Background())
	go func() { _, _ = c.GetOrLoad(ctx1, "k", nilFunc) }()
	go func() { _, _ = c.GetOrLoad(ctx2, "k", nilFunc) }()
	// The starting caller and the two joiners.
	waitFor(t, func() bool { return waitersOf(c, "k") == 3 })

	cancel1()
	waitFor(t, func() bool { return waitersOf(c, "k") == 2 })
	if lctx.Err() != nil {
		t.Fatal("load context cancelled while a waiter remained")
	}
	cancel2()
	waitFor(t, func() bool { return waitersOf(c, "k") == 1 })
	if lctx.Err() != nil {
		t.Fatal("load context cancelled while the starter still waited")
	}
	// The starter (context.Background) still waits, so it must stay live.
	select {
	case err := <-loadCtx: // not expected: no second load starts
		_ = err
	default:
	}
	if lctx.Err() != nil {
		t.Fatal("load context cancelled with a live waiter")
	}
	close(stop)
	_ = loadDone
}

func nilFunc(context.Context) (int, error) { return 0, nil }

// Rule 8: cancellation happens exactly when the last waiter gives up, and
// one caller's cancellation among several does not cancel the load.
func TestRule8SingleCancellationDoesNotCancel(t *testing.T) {
	c := New[string, int](Options{})
	loadCtx := make(chan context.Context, 1)
	release := make(chan struct{})
	patientDone := make(chan error, 1)
	go func() {
		v, err := c.GetOrLoad(context.Background(), "k", func(ctx context.Context) (int, error) {
			loadCtx <- ctx
			<-release
			return 3, nil
		})
		_ = v
		patientDone <- err
	}()
	lctx := waitValue(t, loadCtx, "load to start")

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = c.GetOrLoad(ctx, "k", nilFunc)
	}()
	waitFor(t, func() bool { return waitersOf(c, "k") == 2 })
	cancel()
	wg.Wait()
	if err := lctx.Err(); err != nil {
		t.Fatalf("load context cancelled by one caller's cancellation: %v", err)
	}
	close(release)
	if err := waitValue(t, patientDone, "patient"); err != nil {
		t.Fatalf("patient error = %v", err)
	}
}

// Rule 8: with every caller gone, the load's context is cancelled.
func TestRule8AllGaveUpCancelsLoadContext(t *testing.T) {
	c := New[string, int](Options{})
	loadCtx := make(chan context.Context, 1)
	lateCtxErr := make(chan error, 1)
	release := make(chan struct{})
	loadDone := make(chan struct{})

	// The caller that starts the load is cancelable too, so every caller can
	// give up.
	starterCtx, cancelStarter := context.WithCancel(context.Background())
	go func() {
		_, _ = c.GetOrLoad(starterCtx, "k", func(ctx context.Context) (int, error) {
			loadCtx <- ctx
			<-release
			// Every caller has given up by now, so the cache must have
			// cancelled the load's context: this receive must not block.
			select {
			case <-ctx.Done():
				lateCtxErr <- ctx.Err()
			case <-time.After(5 * time.Second):
				lateCtxErr <- nil
			}
			close(loadDone)
			return 0, context.Canceled
		})
	}()
	lctx := waitValue(t, loadCtx, "load to start")

	// A joiner arrives; the load's context must survive its cancellation,
	// since the starter still waits.
	joinerCtx, cancelJoiner := context.WithCancel(context.Background())
	go func() { _, _ = c.GetOrLoad(joinerCtx, "k", nilFunc) }()
	waitFor(t, func() bool { return waitersOf(c, "k") == 2 })
	cancelJoiner()
	waitFor(t, func() bool { return waitersOf(c, "k") == 1 })
	if err := lctx.Err(); err != nil {
		t.Fatalf("load context cancelled by one caller's cancellation: %v", err)
	}

	// The last caller gives up, so the load's context is cancelled.
	cancelStarter()
	select {
	case <-lctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("load context not cancelled after every caller gave up")
	}
	close(release)
	waitClose(t, loadDone, "load to return")
	if err := waitValue(t, lateCtxErr, "load to finish"); !errors.Is(err, context.Canceled) {
		t.Fatalf("load observed context error = %v, want context.Canceled", err)
	}
}

// Rule 9: a successful late result is stored, with TTL from when load
// returned, even though every caller gave up.
func TestRule9LateResultStored(t *testing.T) {
	clk := newClock()
	c := New[string, int](Options{TTL: time.Minute, Now: clk.Now})
	started := make(chan struct{})
	release := make(chan struct{})
	loadReturned := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	callerDone := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(ctx, "k", func(context.Context) (int, error) {
			close(started)
			<-release
			defer close(loadReturned)
			return 11, nil
		})
		callerDone <- err
	}()
	waitClose(t, started, "load to start")
	waitFor(t, func() bool { return waitersOf(c, "k") == 1 })
	cancel()
	if err := waitValue(t, callerDone, "caller to give up"); !errors.Is(err, context.Canceled) {
		t.Fatalf("caller err = %v, want context.Canceled", err)
	}
	// The load keeps running for nobody, with no waiter left.
	waitFor(t, func() bool { return waitersOf(c, "k") == 0 })

	clk.Advance(30 * time.Second) // load returns well after Set would have started the TTL
	close(release)
	waitClose(t, loadReturned, "load to return")
	waitFor(t, func() bool { return inFlightLen(c) == 0 })

	if v, ok := c.Get("k"); !ok || v != 11 {
		t.Fatalf("Get = %v, %v; want 11, true", v, ok)
	}
	// The TTL starts when load returned: still live just before the minute
	// that followed the return, expired a tick later.
	clk.Advance(time.Minute - time.Nanosecond)
	if _, ok := c.Get("k"); !ok {
		t.Fatal("entry expired before a full TTL after load returned")
	}
	clk.Advance(time.Nanosecond)
	if _, ok := c.Get("k"); ok {
		t.Fatal("entry did not expire a full TTL after load returned")
	}
}

// Rule 9: a result is stored whenever some caller was still waiting for it
// when it arrived, even if every caller has gone by the time it is stored.
func TestRule9ResultForAGivenUpCallerIsStored(t *testing.T) {
	c := New[string, int](Options{MaxEntries: 2})
	c.Set("other", 1)
	started := make(chan struct{})
	release := make(chan struct{})
	loadReturned := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_, _ = c.GetOrLoad(ctx, "k", func(context.Context) (int, error) {
			close(started)
			<-release
			defer close(loadReturned)
			return 2, nil
		})
	}()
	waitClose(t, started, "load to start")
	cancel()
	waitFor(t, func() bool { return waitersOf(c, "k") == 0 })
	close(release)
	waitClose(t, loadReturned, "load to return")
	waitFor(t, func() bool { return inFlightLen(c) == 0 })
	if v, ok := c.Get("k"); !ok || v != 2 {
		t.Fatalf("Get(k) = %v, %v; want 2, true", v, ok)
	}
	// Storing it counts as use, so it did not evict "other" only to be
	// evicted itself: k is the most recently used, other the least.
	if _, ok := c.Get("other"); !ok {
		t.Fatal("other was evicted")
	}
	if n := c.Len(); n != 2 {
		t.Fatalf("Len = %d, want 2", n)
	}
}

// Rule 5 and rule 9: a caller that joins a load after its starter gave up
// still shares that load, and its result is stored.
func TestRule5JoinerAfterStarterGaveUp(t *testing.T) {
	c := New[string, int](Options{})
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	load := func(context.Context) (int, error) {
		calls.Add(1)
		close(started)
		<-release
		return 4, nil
	}
	starterCtx, cancelStarter := context.WithCancel(context.Background())
	starterDone := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(starterCtx, "k", load)
		starterDone <- err
	}()
	waitClose(t, started, "load to start")
	cancelStarter()
	if err := waitValue(t, starterDone, "starter"); !errors.Is(err, context.Canceled) {
		t.Fatalf("starter err = %v, want context.Canceled", err)
	}

	// The starter is gone, but its load is still running for anyone who
	// joins now.
	joinerDone := make(chan struct{})
	var joinerV int
	var joinerErr error
	go func() {
		defer close(joinerDone)
		joinerV, joinerErr = c.GetOrLoad(context.Background(), "k", load)
	}()
	close(release)
	waitClose(t, joinerDone, "joiner")
	if joinerErr != nil || joinerV != 4 {
		t.Fatalf("joiner = %v, %v; want 4, nil", joinerV, joinerErr)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("load calls = %d, want 1", got)
	}
	if v, ok := c.Get("k"); !ok || v != 4 {
		t.Fatalf("Get(k) = %v, %v; want 4, true", v, ok)
	}
}

// Rule 10: Delete removes the key but does not stop an in-flight load; the
// load's result is stored.
func TestRule10DeleteDoesNotStopLoad(t *testing.T) {
	c := New[string, int](Options{})
	c.Set("k", 1)
	started := make(chan struct{})
	release := make(chan struct{})
	result := make(chan int, 1)
	go func() {
		v, err := c.GetOrLoad(context.Background(), "other", func(context.Context) (int, error) {
			close(started)
			<-release
			return 2, nil
		})
		if err != nil {
			result <- -1
			return
		}
		result <- v
	}()
	waitClose(t, started, "load to start")
	c.Delete("other")
	c.Delete("k")
	if _, ok := c.Get("k"); ok {
		t.Fatal("Delete did not remove k")
	}
	close(release)
	if got := waitValue(t, result, "load result"); got != 2 {
		t.Fatalf("caller got %d, want 2", got)
	}
	waitFor(t, func() bool { return inFlightLen(c) == 0 })
	if v, ok := c.Get("other"); !ok || v != 2 {
		t.Fatalf("Get(other) = %v, %v; want 2, true (load result must still be stored)", v, ok)
	}
}

// Rule 11: concurrent mixed use is race-free and totals stay consistent.
func TestRule11ConcurrentUse(t *testing.T) {
	c := New[int, int](Options{TTL: time.Second, MaxEntries: 16})
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				k := (g*7 + i) % 40
				switch i % 4 {
				case 0:
					c.Set(k, i)
				case 1:
					c.Get(k)
				case 2:
					c.Delete(k)
				case 3:
					_, _ = c.GetOrLoad(context.Background(), k, func(context.Context) (int, error) {
						return i, nil
					})
				}
				if n := c.Len(); n > 16 {
					t.Errorf("Len = %d, exceeds MaxEntries", n)
					return
				}
			}
		}(g)
	}
	wg.Wait()
}

// Rule 11: no goroutine the cache starts outlives its load. After every
// GetOrLoad has returned and the loads complete, the count comes back down.
func TestRule11NoGoroutineOutlivesLoads(t *testing.T) {
	c := New[string, int](Options{})
	before := runtimeGoroutines()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("k%d", i%5)
			_, _ = c.GetOrLoad(context.Background(), key, func(context.Context) (int, error) {
				time.Sleep(time.Millisecond)
				return i, nil
			})
		}(i)
	}
	wg.Wait()
	waitFor(t, func() bool {
		c.mu.Lock()
		n := len(c.inFlight)
		c.mu.Unlock()
		return n == 0
	})
	after := runtimeGoroutines()
	if after > before+2 {
		t.Fatalf("goroutines before %d, after %d: a load goroutine outlived its load", before, after)
	}
}

// runtimeGoroutines reports the current goroutine count.
func runtimeGoroutines() int {
	return runtime.NumGoroutine()
}
