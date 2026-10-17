package ttlcache

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// clock is a manually advanced clock.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock { return &clock{now: time.Unix(1_700_000_000, 0)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Rule 1: an entry stored at t is live while Now() < t+TTL; Get and Len
// respect expiry; TTL 0 never expires.
func TestExpiry(t *testing.T) {
	clk := newClock()
	c := New[string, int](Options{TTL: time.Second, Now: clk.Now})
	c.Set("a", 1)
	clk.Advance(500 * time.Millisecond)
	if v, ok := c.Get("a"); !ok || v != 1 {
		t.Fatalf("want live entry at t+0.5s, got %v,%v", v, ok)
	}
	if n := c.Len(); n != 1 {
		t.Fatalf("Len before expiry = %d, want 1", n)
	}
	clk.Advance(500 * time.Millisecond) // exactly t+TTL: no longer before
	if _, ok := c.Get("a"); ok {
		t.Fatal("entry at exactly t+TTL must not be returned")
	}
	if n := c.Len(); n != 0 {
		t.Fatalf("Len after expiry = %d, want 0", n)
	}

	// TTL 0 means never expire.
	clk2 := newClock()
	c2 := New[string, int](Options{Now: clk2.Now})
	c2.Set("a", 1)
	clk2.Advance(100 * time.Hour)
	if v, ok := c2.Get("a"); !ok || v != 1 {
		t.Fatalf("zero TTL entry must never expire, got %v,%v", v, ok)
	}
	if n := c2.Len(); n != 1 {
		t.Fatalf("Len for zero TTL = %d, want 1", n)
	}
}

// Rule 2: Set on an existing key replaces the value and restarts its TTL.
func TestSetReplaceRestartsTTL(t *testing.T) {
	clk := newClock()
	c := New[string, int](Options{TTL: time.Second, Now: clk.Now})
	c.Set("a", 1)
	clk.Advance(900 * time.Millisecond)
	c.Set("a", 2)
	clk.Advance(900 * time.Millisecond) // past the first TTL
	v, ok := c.Get("a")
	if !ok || v != 2 {
		t.Fatalf("want replaced value 2 live, got %v,%v", v, ok)
	}
	clk.Advance(200 * time.Millisecond) // past the restarted TTL
	if _, ok := c.Get("a"); ok {
		t.Fatal("entry must expire relative to the second Set")
	}
}

// Rule 3: LRU eviction keeps at most MaxEntries live entries; use = store
// and hit; expired entries never evict live ones.
func TestLRUEviction(t *testing.T) {
	clk := newClock()
	c := New[string, int](Options{MaxEntries: 2, Now: clk.Now})
	c.Set("a", 1)
	c.Set("b", 2)
	// "a" was used most recently via Get, so storing "c" evicts "b".
	if _, ok := c.Get("a"); !ok {
		t.Fatal("want a live")
	}
	c.Set("c", 3)
	if _, ok := c.Get("b"); ok {
		t.Fatal("b (least recently used) must be evicted")
	}
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a must survive")
	}
	if _, ok := c.Get("c"); !ok {
		t.Fatal("c must survive")
	}
	if n := c.Len(); n != 2 {
		t.Fatalf("Len = %d, want 2", n)
	}

	// Get hits mark recency: hitting "c" then storing "d" must evict "a".
	c.Set("d", 4)
	if _, ok := c.Get("a"); ok {
		t.Fatal("a must be evicted after c became more recent")
	}
	if _, ok := c.Get("c"); !ok {
		t.Fatal("c must survive")
	}

	// Storing the same key again must not exceed the limit (replace, not add).
	c.Set("c", 33)
	if n := c.Len(); n != 2 {
		t.Fatalf("Len after re-Set = %d, want 2", n)
	}
	if v, ok := c.Get("c"); !ok || v != 33 {
		t.Fatalf("replaced value: got %v,%v", v, ok)
	}
}

func TestLRUEvictionHitCountsAsUse(t *testing.T) {
	clk := newClock()
	c := New[string, int](Options{MaxEntries: 2, Now: clk.Now})
	c.Set("a", 1)
	c.Set("b", 2)
	// Without the Get below, storing "c" would evict "a".
	if _, ok := c.Get("a"); !ok {
		t.Fatal("want a live")
	}
	c.Set("c", 3)
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a was just used; it must not be evicted")
	}
	if _, ok := c.Get("b"); ok {
		t.Fatal("b is least recently used; it must be evicted")
	}
}

// Rule 3 (expired never evict live): expired entries are purged before the
// limit is applied.
func TestExpiredEntriesDoNotEvictLive(t *testing.T) {
	clk := newClock()
	c := New[string, int](Options{TTL: time.Second, MaxEntries: 2, Now: clk.Now})
	c.Set("old1", 1)
	clk.Advance(2 * time.Second) // old1 expired
	c.Set("a", 10)
	clk.Advance(10 * time.Millisecond)
	c.Set("b", 20) // live entries: a,b; expired old1 must be purged first
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a must survive; expired old1 must not cause eviction")
	}
	if _, ok := c.Get("b"); !ok {
		t.Fatal("b must survive")
	}
	if n := c.Len(); n != 2 {
		t.Fatalf("Len = %d, want 2 (old1 purged)", n)
	}
}

// A completed load counts as a store for recency.
func TestLRUEvictionViaGetOrLoad(t *testing.T) {
	clk := newClock()
	c := New[string, int](Options{MaxEntries: 2, Now: clk.Now})
	load := func(context.Context) (int, error) { return 1, nil }
	c.GetOrLoad(context.Background(), "a", load)
	c.GetOrLoad(context.Background(), "b", load)
	// hit "a" so "b" becomes LRU
	if _, err := c.GetOrLoad(context.Background(), "a", func(context.Context) (int, error) {
		t.Fatal("load must not be called on a hit")
		return 0, nil
	}); err != nil {
		t.Fatal(err)
	}
	c.GetOrLoad(context.Background(), "c", load)
	if _, ok := c.Get("b"); ok {
		t.Fatal("b must be evicted")
	}
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a must survive")
	}
}

// Rule 4: GetOrLoad on a live hit returns the cached value without calling load.
func TestGetOrLoadHitSkipsLoad(t *testing.T) {
	c := New[string, int](Options{})
	c.Set("k", 42)
	var calls atomic.Int32
	v, err := c.GetOrLoad(context.Background(), "k", func(context.Context) (int, error) {
		calls.Add(1)
		return 0, nil
	})
	if err != nil || v != 42 {
		t.Fatalf("got %v,%v; want 42,nil", v, err)
	}
	if calls.Load() != 0 {
		t.Fatal("load must not be called on a live hit")
	}
}

// Rules 5+6: concurrent misses share one load; different keys load
// independently; error callers all get that same error and nothing is cached.
func TestGetOrLoadSingleflightConcurrent(t *testing.T) {
	c := New[string, int](Options{})
	release := make(chan struct{})
	var calls atomic.Int32
	wantErr := errors.New("boom")
	load := func(context.Context) (int, error) {
		calls.Add(1)
		<-release
		return 0, wantErr
	}

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = c.GetOrLoad(context.Background(), "k", load)
		}(i)
	}
	waitFor(t, "load to start", func() bool { return calls.Load() == 1 })
	// Give other callers a chance to join the same flight.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("load called %d times, want exactly 1 for concurrent callers", got)
	}
	for i, err := range errs {
		if !errors.Is(err, wantErr) {
			t.Fatalf("caller %d err = %v, want %v", i, err, wantErr)
		}
	}
	if n := c.Len(); n != 0 {
		t.Fatalf("Len after failed load = %d, want 0 (nothing cached)", n)
	}
	// A later call loads again.
	v, err := c.GetOrLoad(context.Background(), "k", func(context.Context) (int, error) { return 7, nil })
	if err != nil || v != 7 {
		t.Fatalf("retry after error: got %v,%v; want 7,nil", v, err)
	}
}

func TestGetOrLoadDifferentKeysIndependent(t *testing.T) {
	c := New[string, int](Options{})
	releaseB := make(chan struct{})
	startedA := make(chan struct{}, 1)
	loadA := func(context.Context) (int, error) {
		startedA <- struct{}{}
		return 1, nil
	}
	loadB := func(context.Context) (int, error) {
		<-releaseB
		return 2, nil
	}
	doneB := make(chan struct{})
	go func() {
		defer close(doneB)
		c.GetOrLoad(context.Background(), "b", loadB)
	}()
	// While b's load is blocked, a's load must proceed unimpeded.
	_, err := c.GetOrLoad(context.Background(), "a", loadA)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-startedA:
	case <-time.After(2 * time.Second):
		t.Fatal("loadA never started: keys must load independently")
	}
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a's value must be stored while b still loads")
	}
	close(releaseB)
	<-doneB
	if v, ok := c.Get("b"); !ok || v != 2 {
		t.Fatalf("b: got %v,%v", v, ok)
	}
}

// Rule 7+8: a caller whose ctx is done returns ctx.Err() promptly while the
// load keeps running; the load's ctx is cancelled only when the last waiter
// gives up.
func TestGetOrLoadCallerCancellation(t *testing.T) {
	c := New[string, int](Options{})
	release := make(chan struct{})
	loadCtxCancelled := make(chan struct{})
	loadReturned := make(chan struct{})
	load := func(ctx context.Context) (int, error) {
		go func() {
			<-ctx.Done()
			close(loadCtxCancelled)
		}()
		<-release
		close(loadReturned)
		return 9, nil
	}

	ctx1, cancel1 := context.WithCancel(context.Background())
	ctx2, cancel2 := context.WithCancel(context.Background())
	err1 := make(chan error, 1)
	err2 := make(chan error, 1)
	go func() { _, err := c.GetOrLoad(ctx1, "k", load); err1 <- err }()
	go func() { _, err := c.GetOrLoad(ctx2, "k", load); err2 <- err }()
	waitFor(t, "both waiters joined", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		f := c.flights["k"]
		if f == nil {
			return false
		}
		return f.waiters == 2
	})

	cancel1()
	select {
	case err := <-err1:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled caller err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled caller did not return promptly")
	}
	// The load's ctx must NOT be cancelled while ctx2's waiter remains.
	select {
	case <-loadCtxCancelled:
		t.Fatal("load ctx cancelled while a waiter remained")
	case <-time.After(100 * time.Millisecond):
	}
	// Load still lives for the remaining waiter.
	cancel2()
	select {
	case err := <-err2:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("second caller err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second caller did not return promptly")
	}
	// Now the last waiter is gone: the load's ctx must be cancelled, but the
	// load itself continues and its result is still stored (rules 8+9).
	select {
	case <-loadCtxCancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("load ctx not cancelled after every caller gave up")
	}
	close(release)
	select {
	case <-loadReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("load did not finish")
	}
	waitFor(t, "late result stored", func() bool {
		_, ok := c.Get("k")
		return ok
	})
	if v, ok := c.Get("k"); !ok || v != 9 {
		t.Fatalf("late result: got %v,%v; want 9,true", v, ok)
	}
}

// Rule 9: a successful load stores its value even if every caller gave up,
// with the TTL starting when load returned.
func TestLateResultStoredWithTTLFromLoadReturn(t *testing.T) {
	clk := newClock()
	// Manual clock for the cache; real time only for synchronization.
	c := New[string, int](Options{TTL: time.Second, Now: clk.Now})
	release := make(chan struct{})
	load := func(context.Context) (int, error) {
		<-release
		clk.Advance(400 * time.Millisecond) // load "took" 400ms
		return 5, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := c.GetOrLoad(ctx, "k", load); done <- err }()
	waitFor(t, "load started", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.flights["k"] != nil
	})
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("caller err = %v, want context.Canceled", err)
	}
	// TTL clock must not have started yet: nothing is stored pre-return.
	clk.Advance(300 * time.Millisecond)
	close(release)
	waitFor(t, "result stored", func() bool {
		_, ok := c.Get("k")
		return ok
	})
	// Stored at load-return time (t=400ms). Live until t=1.4s.
	clk.Advance(900 * time.Millisecond) // total 1.3s: still live
	if v, ok := c.Get("k"); !ok || v != 5 {
		t.Fatalf("late result before TTL: got %v,%v", v, ok)
	}
	clk.Advance(200 * time.Millisecond) // total 1.5s > 400ms + 1s TTL
	if _, ok := c.Get("k"); ok {
		t.Fatal("late result must expire TTL after load returned")
	}
}

// Rule 10: Delete removes the key; an in-flight load is not stopped and its
// result is still stored when it completes.
func TestDeleteDoesNotStopInFlightLoad(t *testing.T) {
	c := New[string, int](Options{})
	c.Set("k", 1)
	c.Delete("k")
	if _, ok := c.Get("k"); ok {
		t.Fatal("Delete must remove the key")
	}
	if n := c.Len(); n != 0 {
		t.Fatalf("Len after Delete = %d, want 0", n)
	}

	release := make(chan struct{})
	started := make(chan struct{}, 1)
	load := func(context.Context) (int, error) {
		started <- struct{}{}
		<-release
		return 42, nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.GetOrLoad(context.Background(), "k", load)
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("load never started")
	}
	c.Delete("k")
	close(release)
	<-done
	if v, ok := c.Get("k"); !ok || v != 42 {
		t.Fatalf("in-flight load result must still be stored after Delete: got %v,%v", v, ok)
	}
}

// Rule 11: concurrent use is race-clean (validated by -race), and goroutines
// started by the cache do not outlive the loads they serve.
func TestConcurrentUseStress(t *testing.T) {
	c := New[int, int](Options{TTL: 5 * time.Second, MaxEntries: 64})
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
				k := (w*997 + i) % 128
				switch i % 5 {
				case 0:
					c.Set(k, i)
				case 1:
					c.Get(k)
				case 2:
					c.Delete(k)
				case 3:
					c.Len()
				case 4:
					c.GetOrLoad(context.Background(), k, func(context.Context) (int, error) {
						return i, nil
					})
				}
			}
		}(w)
	}
	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// Rule 8 refinement: the flight ctx is not derived from (i.e. not cancelled
// with) any single caller within a multi-caller flight while others remain.
func TestFlightCtxSurvivesPartialCancellation(t *testing.T) {
	c := New[string, int](Options{})
	release := make(chan struct{})
	ctxSeen := make(chan context.Context, 1)
	load := func(ctx context.Context) (int, error) {
		ctxSeen <- ctx
		<-release
		return 1, nil
	}
	ctx1, cancel1 := context.WithCancel(context.Background())
	go c.GetOrLoad(ctx1, "k", load)
	go c.GetOrLoad(context.Background(), "k", load)
	var loadCtx context.Context
	select {
	case loadCtx = <-ctxSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("load never ran")
	}
	waitFor(t, "two waiters", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		f := c.flights["k"]
		return f != nil && f.waiters == 2
	})
	cancel1()
	select {
	case <-loadCtx.Done():
		t.Fatal("load ctx cancelled just because one of several callers was cancelled")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
}
