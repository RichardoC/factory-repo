package ttlcache_test

import (
	"context"
	"errors"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ttlcache "github.com/RichardoC/factory-repo/app/exp-ttl-glm53-1"
)

// fakeClock is a mutex-protected clock the tests control.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

// fcNow returns a value of the Options.Now field type: func() time.Time.
func fcNow() time.Time { return newClock().Now() }

func newClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", desc)
}

// Compile-time pins of the exact exported API (rule: the API surface).
var (
	_ func(ttlcache.Options) *ttlcache.Cache[string, int]                                                          = ttlcache.New[string, int]
	_ func(*ttlcache.Cache[string, int], string) (int, bool)                                                       = (*ttlcache.Cache[string, int]).Get
	_ func(*ttlcache.Cache[string, int], string, int)                                                              = (*ttlcache.Cache[string, int]).Set
	_ func(*ttlcache.Cache[string, int], string)                                                                   = (*ttlcache.Cache[string, int]).Delete
	_ func(*ttlcache.Cache[string, int]) int                                                                       = (*ttlcache.Cache[string, int]).Len
	_ func(*ttlcache.Cache[string, int], context.Context, string, func(context.Context) (int, error)) (int, error) = (*ttlcache.Cache[string, int]).GetOrLoad
)

// TestAPIShape pins the exported surface: exactly the three Options
// fields in the documented order, only unexported fields on Cache, and
// exactly the five methods on *Cache.
func TestAPIShape(t *testing.T) {
	ot := reflect.TypeOf(ttlcache.Options{})
	if got := ot.NumField(); got != 3 {
		t.Fatalf("Options has %d exported+unexported fields, want exactly 3", got)
	}
	wantFields := []struct {
		name string
		typ  reflect.Type
	}{
		{"TTL", reflect.TypeOf(time.Duration(0))},
		{"MaxEntries", reflect.TypeOf(int(0))},
		{"Now", reflect.TypeOf(fcNow)},
	}
	for i, w := range wantFields {
		f := ot.Field(i)
		if !f.IsExported() || f.Name != w.name || f.Type != w.typ {
			t.Errorf("Options field %d = %s %s, want exported %s %s", i, f.Name, f.Type, w.name, w.typ)
		}
	}

	ct := reflect.TypeOf(ttlcache.Cache[string, int]{})
	for i := 0; i < ct.NumField(); i++ {
		if ct.Field(i).IsExported() {
			t.Errorf("Cache has exported field %s; all fields must be unexported", ct.Field(i).Name)
		}
	}

	mt := reflect.TypeOf(&ttlcache.Cache[string, int]{})
	wantMethods := []string{"Delete", "Get", "GetOrLoad", "Len", "Set"}
	if got := mt.NumMethod(); got != len(wantMethods) {
		t.Fatalf("*Cache has %d methods, want exactly %d", got, len(wantMethods))
	}
	for i, name := range wantMethods {
		if mt.Method(i).Name != name {
			t.Errorf("*Cache method %d = %s, want %s", i, mt.Method(i).Name, name)
		}
	}
}

// TestExpiry covers rule 1: an entry stored at t is live while Now() is
// before t+TTL, Get and GetOrLoad never return an expired entry, and Len
// counts only live entries.
func TestExpiry(t *testing.T) {
	clk := newClock()
	c := ttlcache.New[string, string](ttlcache.Options{TTL: 5 * time.Minute, Now: clk.Now})

	c.Set("k", "v")
	if v, ok := c.Get("k"); !ok || v != "v" {
		t.Fatalf("fresh entry: got (%q, %v), want (\"v\", true)", v, ok)
	}
	clk.Advance(4 * time.Minute)
	if _, ok := c.Get("k"); !ok {
		t.Fatal("entry expired 1 minute before t+TTL")
	}
	clk.Advance(1 * time.Minute) // exactly t+TTL: no longer live
	if _, ok := c.Get("k"); ok {
		t.Fatal("Get returned an expired entry (live only before t+TTL)")
	}
	if c.Len() != 0 {
		t.Fatalf("Len = %d, want 0 (expired entries are not live)", c.Len())
	}

	// GetOrLoad must also treat an expired entry as a miss (rule 1).
	var calls atomic.Int32
	got, err := c.GetOrLoad(context.Background(), "k", func(context.Context) (string, error) {
		calls.Add(1)
		return "fresh", nil
	})
	if err != nil || got != "fresh" {
		t.Fatalf("GetOrLoad on expired entry: got (%q, %v)", got, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("load called %d times, want 1", calls.Load())
	}
}

// TestLenCountsOnlyLiveEntries covers rule 1: Len counts live entries
// only, as they lapse at different times.
func TestLenCountsOnlyLiveEntries(t *testing.T) {
	clk := newClock()
	c := ttlcache.New[string, string](ttlcache.Options{TTL: 5 * time.Minute, Now: clk.Now})
	c.Set("a", "1") // expires at t=5m
	clk.Advance(3 * time.Minute)
	c.Set("b", "2") // expires at t=8m
	if c.Len() != 2 {
		t.Fatalf("Len = %d, want 2", c.Len())
	}
	clk.Advance(2 * time.Minute) // t=5m: a expired, b live
	if c.Len() != 1 {
		t.Fatalf("Len = %d, want 1 after a expired", c.Len())
	}
	clk.Advance(4 * time.Minute) // t=9m: b expired too
	if c.Len() != 0 {
		t.Fatalf("Len = %d, want 0", c.Len())
	}
}

// TestSetRestartsTTL covers rule 2: Set on an existing key replaces the
// value and restarts the TTL.
func TestSetRestartsTTL(t *testing.T) {
	clk := newClock()
	c := ttlcache.New[string, string](ttlcache.Options{TTL: 10 * time.Minute, Now: clk.Now})

	c.Set("k", "old")
	clk.Advance(6 * time.Minute)
	c.Set("k", "new")            // restarts: now expires at t=16m, old expiry was 10m
	clk.Advance(4 * time.Minute) // t=10m: the old entry would be dead now
	if v, ok := c.Get("k"); !ok || v != "new" {
		t.Fatalf("replaced entry not live after old TTL would have passed: (%q, %v)", v, ok)
	}
	clk.Advance(5 * time.Minute) // t=15m: still before the restarted TTL
	if v, ok := c.Get("k"); !ok || v != "new" {
		t.Fatalf("replaced entry should still be live: (%q, %v)", v, ok)
	}
	clk.Advance(1 * time.Minute) // t=16m: restarted TTL over
	if _, ok := c.Get("k"); ok {
		t.Fatal("replaced entry outlived its restarted TTL")
	}
}

// TestLRUEviction covers rule 3 with the model cache bound at 2.
func TestLRUEviction(t *testing.T) {
	clk := newClock()
	c := ttlcache.New[string, string](ttlcache.Options{TTL: time.Hour, MaxEntries: 2, Now: clk.Now})

	c.Set("a", "1")
	c.Set("b", "2")
	c.Get("a") // a becomes most recently used, b least recently used
	c.Set("c", "3")
	if _, ok := c.Get("b"); ok {
		t.Fatal("least recently used live entry was not evicted")
	}
	if v, ok := c.Get("a"); !ok || v != "1" {
		t.Fatalf("recently used entry evicted: (%q, %v)", v, ok)
	}
	if v, ok := c.Get("c"); !ok || v != "3" {
		t.Fatalf("newest entry missing: (%q, %v)", v, ok)
	}

	// Recency also follows store order and completed loads (rule 3).
	c2 := ttlcache.New[string, string](ttlcache.Options{MaxEntries: 2})
	c2.Set("x", "1")
	c2.Set("y", "2") // y more recent than x by store order
	c2.Set("z", "3")
	if _, ok := c2.Get("x"); ok {
		t.Fatal("entry stored first should be evicted when all recency is store order")
	}

	c3 := ttlcache.New[string, string](ttlcache.Options{MaxEntries: 2})
	if _, err := c3.GetOrLoad(context.Background(), "p", func(context.Context) (string, error) { return "P", nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := c3.GetOrLoad(context.Background(), "q", func(context.Context) (string, error) { return "Q", nil }); err != nil {
		t.Fatal(err)
	}
	// A completed load marks the entry used; a hit marks it used again.
	var reloads atomic.Int32
	if v, err := c3.GetOrLoad(context.Background(), "p", func(context.Context) (string, error) { reloads.Add(1); return "P2", nil }); err != nil || v != "P" {
		t.Fatalf("hit on p: got (%q, %v), want (\"P\", nil)", v, err)
	}
	c3.Set("r", "3")
	if _, ok := c3.Get("q"); ok {
		t.Fatal("q was least recently used (p was hit last) and should have been evicted")
	}
	if reloads.Load() != 0 {
		t.Fatal("load was called on a cache hit (rule 4)")
	}
}

// TestExpiredEntriesNeverEvictLive covers rule 3's second bullet.
func TestExpiredEntriesNeverEvictLive(t *testing.T) {
	clk := newClock()
	c := ttlcache.New[string, string](ttlcache.Options{TTL: 5 * time.Minute, MaxEntries: 2, Now: clk.Now})

	c.Set("a", "1") // expires at t=5m
	clk.Advance(5 * time.Minute)
	c.Set("b", "2")              // expires at t=10m; a is expired by now
	clk.Advance(1 * time.Minute) // t=6m: a expired, b live
	c.Set("c", "3")              // must drop a, not evict live b
	if _, ok := c.Get("a"); ok {
		t.Fatal("expired entry a resurrected")
	}
	if v, ok := c.Get("b"); !ok || v != "2" {
		t.Fatal("live entry b evicted because an expired entry took the count")
	}
	if v, ok := c.Get("c"); !ok || v != "3" {
		t.Fatal("newest entry c missing")
	}
	if c.Len() != 2 {
		t.Fatalf("Len = %d, want 2", c.Len())
	}

	// Bounds still hold when the cache is full of live entries.
	c2 := ttlcache.New[int, int](ttlcache.Options{MaxEntries: 1})
	c2.Set(1, 1)
	c2.Set(2, 2)
	if _, ok := c2.Get(1); ok {
		t.Fatal("MaxEntries=1 cache holds two live entries")
	}
	if v, ok := c2.Get(2); !ok || v != 2 {
		t.Fatalf("newest entry missing: (%d, %v)", v, ok)
	}
}

// TestHitDoesNotCallLoad covers rule 4.
func TestHitDoesNotCallLoad(t *testing.T) {
	c := ttlcache.New[string, string](ttlcache.Options{TTL: time.Hour})
	c.Set("k", "cached")
	calls := 0
	boom := func(context.Context) (string, error) {
		calls++
		return "should not happen", errors.New("load must not be called on a hit")
	}
	v, err := c.GetOrLoad(context.Background(), "k", boom)
	if err != nil || v != "cached" || calls != 0 {
		t.Fatalf("hit: got (%q, %v, %d calls), want (\"cached\", nil, 0 calls)", v, err, calls)
	}
	// A second hit behaves the same.
	if v, err := c.GetOrLoad(context.Background(), "k", boom); err != nil || v != "cached" || calls != 0 {
		t.Fatalf("second hit: got (%q, %v, %d calls)", v, err, calls)
	}
}

// TestDefaults checks the documented meaning of the zero Options: entries
// never expire, the cache is unbounded, and nil Now means time.Now.
func TestDefaults(t *testing.T) {
	c := ttlcache.New[int, int](ttlcache.Options{})
	for i := 0; i < 100; i++ {
		c.Set(i, i)
	}
	if c.Len() != 100 {
		t.Fatalf("Len = %d, want 100 (zero MaxEntries means unbounded)", c.Len())
	}
	if v, ok := c.Get(42); !ok || v != 42 {
		t.Fatalf("zero TTL entry missing: (%d, %v)", v, ok)
	}
}

// TestOneLoadPerKey covers rule 5: concurrent callers for one key share
// a single load, and different keys load independently.
func TestOneLoadPerKey(t *testing.T) {
	c := ttlcache.New[string, string](ttlcache.Options{})
	var calls atomic.Int32
	release := make(chan struct{})
	started := make(chan struct{}, 17)
	load := func(context.Context) (string, error) {
		calls.Add(1)
		started <- struct{}{}
		<-release
		return "shared", nil
	}

	const n = 8
	entered := make(chan struct{}, n)
	results := make(chan string, n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			entered <- struct{}{}
			v, err := c.GetOrLoad(context.Background(), "k", load)
			results <- v
			errs <- err
		}()
	}
	<-started
	// A different key must load while "k"'s load is still blocked.
	done := make(chan error, 1)
	go func() {
		v, err := c.GetOrLoad(context.Background(), "other", func(context.Context) (string, error) {
			return "other-value", nil
		})
		if err != nil || v != "other-value" {
			done <- err
		} else {
			done <- nil
		}
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("independent key load failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("loads for different keys block each other")
	}

	for i := 0; i < n; i++ {
		<-entered
	}
	close(release)
	for i := 0; i < n; i++ {
		if v, err := <-results, <-errs; err != nil || v != "shared" {
			t.Fatalf("waiter %d got (%q, %v), want (\"shared\", nil)", i, v, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("load called %d times for one key, want 1 (rule 5)", calls.Load())
	}
}

// TestSingleKeyUnblockedContention hammers GetOrLoad on one key under
// -race; every caller must see the loaded value and only one load runs.
func TestSingleKeyUnblockedContention(t *testing.T) {
	c := ttlcache.New[string, int](ttlcache.Options{})
	var calls atomic.Int32
	load := func(context.Context) (int, error) {
		calls.Add(1)
		time.Sleep(2 * time.Millisecond)
		return 42, nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := c.GetOrLoad(context.Background(), "k", load)
			if err != nil || v != 42 {
				t.Errorf("caller got (%d, %v)", v, err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("load called %d times, want 1", calls.Load())
	}
}

// TestLoadErrorSharedAndRetried covers rule 6.
func TestLoadErrorSharedAndRetried(t *testing.T) {
	c := ttlcache.New[string, string](ttlcache.Options{})
	errBoom := errors.New("boom")
	var calls atomic.Int32
	started := make(chan struct{}, 9)
	release := make(chan struct{})
	load := func(context.Context) (string, error) {
		n := calls.Add(1)
		started <- struct{}{}
		<-release
		if n == 1 {
			return "", errBoom
		}
		return "recovered", nil
	}

	const n = 5
	entered := make(chan struct{}, n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			entered <- struct{}{}
			_, err := c.GetOrLoad(context.Background(), "k", load)
			errs <- err
		}()
	}
	<-started
	for i := 0; i < n; i++ {
		<-entered
	}
	close(release)
	for i := 0; i < n; i++ {
		if err := <-errs; err != errBoom {
			t.Fatalf("waiting caller %d got %v, want the shared load error %v", i, err, errBoom)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("load called %d times, want 1", calls.Load())
	}
	if _, ok := c.Get("k"); ok {
		t.Fatal("a failed load's key was cached")
	}
	if c.Len() != 0 {
		t.Fatalf("Len = %d, want 0 after a failed load", c.Len())
	}
	// A later GetOrLoad must call load again (rule 6).
	v, err := c.GetOrLoad(context.Background(), "k", load)
	if err != nil || v != "recovered" {
		t.Fatalf("retry: got (%q, %v), want (\"recovered\", nil)", v, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("load called %d times after retry, want 2", calls.Load())
	}
	if v, ok := c.Get("k"); !ok || v != "recovered" {
		t.Fatal("recovered value not cached")
	}
}

// TestGiveUpKeepsLoadRunningForOtherWaiters covers rules 7 and 8: a
// caller whose ctx is done returns ctx.Err() promptly while the load
// keeps running for the other waiters, and the load's context is not
// cancelled just because one of several callers gave up.
func TestGiveUpKeepsLoadRunningForOtherWaiters(t *testing.T) {
	c := ttlcache.New[string, string](ttlcache.Options{})
	errCancelledEarly := errors.New("load context was cancelled while a waiter remained")
	ctx1, cancel := context.WithCancel(context.Background())
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	load := func(loadCtx context.Context) (string, error) {
		started <- struct{}{}
		select {
		case <-release:
			if loadCtx.Err() != nil {
				return "", errCancelledEarly
			}
			return "v", nil
		case <-loadCtx.Done():
			return "", loadCtx.Err()
		}
	}

	res1 := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(ctx1, "k", load)
		res1 <- err
	}()
	<-started

	res2 := make(chan string, 1)
	err2 := make(chan error, 1)
	go func() {
		v, err := c.GetOrLoad(context.Background(), "k", load)
		res2 <- v
		err2 <- err
	}()
	time.Sleep(50 * time.Millisecond) // let the second caller join the in-flight load

	cancel()
	if err := <-res1; err != context.Canceled {
		t.Fatalf("caller that gave up got %v, want context.Canceled", err)
	}
	close(release) // the load must still be running for caller 2

	select {
	case v2 := <-res2:
		if err := <-err2; err != nil {
			t.Fatalf("remaining waiter got error %v (load ctx must not be cancelled while a waiter remains)", err)
		}
		if v2 != "v" {
			t.Fatalf("remaining waiter got %q, want \"v\"", v2)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("load did not keep running for the remaining waiter")
	}
}

// TestLoadContextCancelledWhenAllWaitersGiveUp covers rule 8: the load's
// context is cancelled exactly when every waiting caller has given up.
func TestLoadContextCancelledWhenAllWaitersGiveUp(t *testing.T) {
	c := ttlcache.New[string, string](ttlcache.Options{})
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{}, 1)
	observed := make(chan error, 1)
	load := func(loadCtx context.Context) (string, error) {
		started <- struct{}{}
		select {
		case <-loadCtx.Done():
			observed <- loadCtx.Err()
			return "", loadCtx.Err()
		case <-time.After(5 * time.Second):
			return "v", nil
		}
	}
	res := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(ctx, "k", load)
		res <- err
	}()
	<-started
	cancel()
	if err := <-res; err != context.Canceled {
		t.Fatalf("caller got %v, want context.Canceled", err)
	}
	select {
	case err := <-observed:
		if err != context.Canceled {
			t.Fatalf("load observed ctx error %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("load context was not cancelled after every waiter gave up")
	}
	if _, ok := c.Get("k"); ok {
		t.Fatal("load ctx error was stored in the cache")
	}
	// A later GetOrLoad calls load again (rule 6); nothing blocks.
	v, err := c.GetOrLoad(context.Background(), "k", load)
	if err != nil || v != "v" {
		t.Fatalf("retry after all callers gave up: got (%q, %v)", v, err)
	}
}

// TestLateResultStored covers rule 9: a successful load result is stored
// with its TTL measured from when load returned, even if every caller
// had given up.
func TestLateResultStored(t *testing.T) {
	clk := newClock()
	c := ttlcache.New[string, string](ttlcache.Options{TTL: 10 * time.Minute, Now: clk.Now})
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	load := func(context.Context) (string, error) {
		started <- struct{}{}
		<-release
		return "late", nil
	}
	res := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(ctx, "k", load)
		res <- err
	}()
	<-started
	cancel()
	if err := <-res; err != context.Canceled {
		t.Fatalf("caller got %v, want context.Canceled", err)
	}
	clk.Advance(100 * time.Minute)
	close(release)
	waitFor(t, "late load result to be stored", func() bool {
		_, ok := c.Get("k")
		return ok
	})
	if v, ok := c.Get("k"); !ok || v != "late" {
		t.Fatalf("late result missing: (%q, %v)", v, ok)
	}
	// The TTL started when load returned (t=100min), so it is live at
	// 109min and expired at 110min.
	clk.Advance(9 * time.Minute)
	if _, ok := c.Get("k"); !ok {
		t.Fatal("late entry's TTL did not start at load return")
	}
	clk.Advance(1 * time.Minute)
	if _, ok := c.Get("k"); ok {
		t.Fatal("late entry outlived the TTL measured from load return")
	}
}

// TestDelete covers rule 10.
func TestDelete(t *testing.T) {
	c := ttlcache.New[string, string](ttlcache.Options{})
	c.Set("k", "v")
	if v, ok := c.Get("k"); !ok || v != "v" {
		t.Fatalf("Get after Set: (%q, %v)", v, ok)
	}
	c.Delete("k")
	if _, ok := c.Get("k"); ok {
		t.Fatal("Get found a deleted key")
	}
	if c.Len() != 0 {
		t.Fatalf("Len = %d, want 0 after Delete", c.Len())
	}
	c.Set("k", "again")
	if v, ok := c.Get("k"); !ok || v != "again" {
		t.Fatal("Set after Delete failed")
	}
}

// TestDeleteDoesNotStopInFlightLoad covers rule 10: Delete removes the
// key but an in-flight load's result is still stored when it completes.
func TestDeleteDoesNotStopInFlightLoad(t *testing.T) {
	c := ttlcache.New[string, string](ttlcache.Options{})
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	load := func(context.Context) (string, error) {
		started <- struct{}{}
		<-release
		return "loaded", nil
	}
	res := make(chan string, 1)
	go func() {
		v, err := c.GetOrLoad(context.Background(), "k", load)
		if err != nil {
			t.Error(err)
		}
		res <- v
	}()
	<-started
	c.Delete("k")
	if _, ok := c.Get("k"); ok {
		t.Fatal("key still present after Delete")
	}
	close(release)
	if v := <-res; v != "loaded" {
		t.Fatalf("caller got %q", v)
	}
	waitFor(t, "in-flight load result to be stored after Delete", func() bool {
		v, ok := c.Get("k")
		return ok && v == "loaded"
	})
}

// TestLoadIndelibleGoroutines covers rule 11: goroutines the cache starts
// end when the loads they serve return.
func TestLoadIndelibleGoroutines(t *testing.T) {
	before := runtime.NumGoroutine()
	c := ttlcache.New[int, int](ttlcache.Options{TTL: time.Minute})
	started := make(chan struct{}, 2)
	pass := make(chan struct{})
	release := func() {}
	_ = release
	load := func(context.Context) (int, error) {
		started <- struct{}{}
		<-pass
		return 7, nil
	}
	const waiters = 8
	cancels := make([]context.CancelFunc, waiters)
	dones := make(chan error, waiters)
	for i := 0; i < waiters; i++ {
		wctx, cancel := context.WithCancel(context.Background())
		cancels[i] = cancel
		go func() {
			_, err := c.GetOrLoad(wctx, 1, load)
			dones <- err
		}()
	}
	<-started
	cancels[0]()
	for i := 1; i < waiters; i++ {
		cancels[i]()
	}
	for i := 0; i < waiters; i++ {
		if err := <-dones; err != nil && err != context.Canceled {
			t.Fatalf("waiter got %v, want context.Canceled (or a fresh result)", err)
		}
	}
	close(pass) // let the loads return
	waitFor(t, "load goroutines to finish", func() bool {
		return runtime.NumGoroutine() <= before+2
	})
}

// TestConcurrentUse covers rule 11: mixed concurrent use must be safe
// under -race, and the live count stays within MaxEntries.
func TestConcurrentUse(t *testing.T) {
	c := ttlcache.New[int, int](ttlcache.Options{TTL: 2 * time.Millisecond, MaxEntries: 16})
	var wg sync.WaitGroup
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				k := (w*7 + i*3) % 12
				switch (w + i) % 5 {
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
						return k * 10, nil
					}); err != nil {
						t.Errorf("GetOrLoad: %v", err)
					}
				}
			}
		}(w)
	}
	wg.Wait()
	if l := c.Len(); l > 16 {
		t.Fatalf("Len = %d exceeds MaxEntries", l)
	}
}
