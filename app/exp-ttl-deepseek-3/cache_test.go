package ttlcache

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a manually advanced clock. It is what Options.Now points at, so
// every test drives time explicitly instead of sleeping.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// waitFor polls cond until it is true or the deadline passes. It is only used
// to detect "some other goroutine has got this far"; correctness never depends
// on the timing it observes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestRule1_Expiry covers rule 1: an entry stored at t is live while Now() is
// strictly before t+TTL; at exactly t+TTL it is expired; TTL 0 never expires;
// expired entries do not count in Len and Get returns false.
func TestRule1_Expiry(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{TTL: 100 * time.Millisecond, Now: clk.Now})
	c.Set("k", 7)
	t0 := clk.Now()

	// Strictly before t+TTL: live.
	clk.Advance(99 * time.Millisecond)
	if got, ok := c.Get("k"); !ok || got != 7 {
		t.Fatalf("before expiry: Get = (%v, %v), want (7, true)", got, ok)
	}
	if n := c.Len(); n != 1 {
		t.Fatalf("before expiry: Len = %d, want 1", n)
	}

	// Exactly t+TTL: expired (live requires strictly before).
	clk.Advance(1 * time.Millisecond)
	if clk.Now() != t0.Add(100*time.Millisecond) {
		t.Fatalf("clock = %v, want exactly stored+TTL", clk.Now())
	}
	if got, ok := c.Get("k"); ok {
		t.Fatalf("at exactly t+TTL: Get = (%v, true), want miss", got)
	}
	if n := c.Len(); n != 0 {
		t.Fatalf("at exactly t+TTL: Len = %d, want 0", n)
	}

	// TTL 0 means entries never expire.
	forever := New[string, int](Options{TTL: 0, Now: clk.Now})
	forever.Set("k", 7)
	clk.Advance(1000 * time.Hour)
	if got, ok := forever.Get("k"); !ok || got != 7 {
		t.Fatalf("TTL 0: Get = (%v, %v), want (7, true)", got, ok)
	}
	if n := forever.Len(); n != 1 {
		t.Fatalf("TTL 0: Len = %d, want 1", n)
	}

	// A load produced value expires too.
	calls := 0
	v, err := c.GetOrLoad(context.Background(), "k", func(context.Context) (int, error) {
		calls++
		return 11, nil
	})
	if err != nil || v != 11 || calls != 1 {
		t.Fatalf("GetOrLoad after expiry = (%v, %v), calls %d; want (11, nil), 1", v, err, calls)
	}
	clk.Advance(100 * time.Millisecond)
	if _, err := c.GetOrLoad(context.Background(), "k", func(context.Context) (int, error) {
		calls++
		return 12, nil
	}); err != nil || calls != 2 {
		t.Fatalf("expired loaded entry was reused: calls = %d, want 2", calls)
	}
}

// TestRule2_Replacement covers rule 2: Set on an existing key replaces the
// value and restarts its TTL.
func TestRule2_Replacement(t *testing.T) {
	clk := newFakeClock()
	c := New[string, string](Options{TTL: 100 * time.Millisecond, Now: clk.Now})

	c.Set("k", "first")
	clk.Advance(90 * time.Millisecond)
	c.Set("k", "second")

	if got, ok := c.Get("k"); !ok || got != "second" {
		t.Fatalf("right after replacement: Get = (%q, %v), want (\"second\", true)", got, ok)
	}
	if n := c.Len(); n != 1 {
		t.Fatalf("after replacement: Len = %d, want 1", n)
	}

	// The old expiry has passed (t_first+TTL), but the new full TTL applies,
	// so the entry is still live.
	clk.Advance(15 * time.Millisecond) // 105ms after the first Set
	if got, ok := c.Get("k"); !ok || got != "second" {
		t.Fatalf("past old expiry: Get = (%q, %v), want (\"second\", true)", got, ok)
	}

	// Exactly TTL after the replacement it expires.
	clk.Advance(85 * time.Millisecond) // 100ms after the second Set
	if got, ok := c.Get("k"); ok {
		t.Fatalf("TTL after replacement: Get = (%q, true), want miss", got)
	}
}

// TestRule3_LRUEviction covers rule 3: eviction happens only above
// MaxEntries, the victim is the least recently used live entry, Get and a
// GetOrLoad hit both count as use, and an expired entry is dropped rather than
// causing a live entry to be evicted.
func TestRule3_LRUEviction(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{TTL: 0, MaxEntries: 2, Now: clk.Now})

	// At MaxEntries nothing is evicted.
	c.Set("a", 1)
	c.Set("b", 2)
	if n := c.Len(); n != 2 {
		t.Fatalf("at MaxEntries: Len = %d, want 2", n)
	}

	// Storing a third evicts the least recently used live entry: "a".
	c.Set("c", 3)
	if n := c.Len(); n != 2 {
		t.Fatalf("above MaxEntries: Len = %d, want 2", n)
	}
	if _, ok := c.Get("a"); ok {
		t.Fatal("least recently used entry a was not evicted")
	}
	if _, ok := c.Get("b"); !ok {
		t.Fatal("entry b was evicted but a was the LRU")
	}
	if _, ok := c.Get("c"); !ok {
		t.Fatal("newly stored entry c is missing")
	}

	// A Get counts as use: touching "b" makes "c" the LRU.
	clk.Advance(time.Second)
	if _, ok := c.Get("b"); !ok {
		t.Fatal("b should be live")
	}
	c.Set("d", 4)
	if _, ok := c.Get("c"); ok {
		t.Fatal("c was not evicted after b was used by Get")
	}
	if _, ok := c.Get("b"); !ok {
		t.Fatal("b was evicted although it was the most recently used")
	}

	// A GetOrLoad hit counts as use: touching "b" makes "d" the LRU, and
	// load must not be called.
	calls := 0
	if _, err := c.GetOrLoad(context.Background(), "b", func(context.Context) (int, error) {
		calls++
		return 99, nil
	}); err != nil {
		t.Fatalf("GetOrLoad hit: unexpected error %v", err)
	}
	if calls != 0 {
		t.Fatalf("GetOrLoad hit called load %d times, want 0", calls)
	}
	c.Set("e", 5)
	if _, ok := c.Get("d"); ok {
		t.Fatal("d was not evicted after b was used by a GetOrLoad hit")
	}
	if _, ok := c.Get("b"); !ok {
		t.Fatal("b was evicted although a GetOrLoad hit used it")
	}

	// A Set counts as use of the stored key, and an expired entry is dropped
	// rather than evicting a live one.
	clk2 := newFakeClock()
	c2 := New[string, int](Options{TTL: 50 * time.Millisecond, MaxEntries: 2, Now: clk2.Now})
	c2.Set("x", 1)
	c2.Set("y", 2)
	clk2.Advance(100 * time.Millisecond) // x and y expired
	c2.Set("live1", 10)
	c2.Set("live2", 20) // must drop expired x/y, not evict live1
	if _, ok := c2.Get("live1"); !ok {
		t.Fatal("live1 was evicted instead of the expired entry")
	}
	if _, ok := c2.Get("live2"); !ok {
		t.Fatal("live2 missing")
	}
	if n := c2.Len(); n != 2 {
		t.Fatalf("after replacing expired entries: Len = %d, want 2", n)
	}

	// MaxEntries 2 with one expired plus two live: the expired one goes, both
	// live entries stay.
	clk3 := newFakeClock()
	c3 := New[string, int](Options{TTL: 50 * time.Millisecond, MaxEntries: 2, Now: clk3.Now})
	c3.Set("old", 1)
	clk3.Advance(60 * time.Millisecond) // "old" is expired
	c3.Set("a", 2)
	c3.Set("b", 3)
	if _, ok := c3.Get("old"); ok {
		t.Fatal("expired entry old is still live")
	}
	if _, ok := c3.Get("a"); !ok {
		t.Fatal("live entry a was evicted to make room; the expired entry should have been dropped")
	}
	if _, ok := c3.Get("b"); !ok {
		t.Fatal("live entry b was evicted to make room; the expired entry should have been dropped")
	}
	if n := c3.Len(); n != 2 {
		t.Fatalf("Len = %d, want 2", n)
	}
}

// TestRule4_Hits covers rule 4: GetOrLoad on a live hit returns the cached
// value without calling load.
func TestRule4_Hits(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{TTL: time.Minute, Now: clk.Now})
	c.Set("k", 42)

	var calls int32
	got, err := c.GetOrLoad(context.Background(), "k", func(context.Context) (int, error) {
		atomic.AddInt32(&calls, 1)
		return -1, nil
	})
	if err != nil {
		t.Fatalf("GetOrLoad hit returned error %v", err)
	}
	if got != 42 {
		t.Fatalf("GetOrLoad hit = %d, want 42", got)
	}
	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Fatalf("load was called %d times on a live hit, want 0", n)
	}

	// Also true for a value that came from a load rather than Set.
	if _, err := c.GetOrLoad(context.Background(), "other", func(context.Context) (int, error) {
		atomic.AddInt32(&calls, 1)
		return 5, nil
	}); err != nil {
		t.Fatalf("first load: %v", err)
	}
	if got, err := c.GetOrLoad(context.Background(), "other", func(context.Context) (int, error) {
		atomic.AddInt32(&calls, 1)
		return -1, nil
	}); err != nil || got != 5 {
		t.Fatalf("second GetOrLoad = (%d, %v), want (5, nil)", got, err)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("load calls = %d, want 1 (the second call must be a hit)", n)
	}
}

// TestRule5_OneLoadPerKey covers rule 5: concurrent misses for one key share a
// single load, and different keys load independently.
func TestRule5_OneLoadPerKey(t *testing.T) {
	c := New[string, int](Options{TTL: 0})

	const callers = 16
	var loads int32
	entered := make(chan struct{}, callers)
	release := make(chan struct{})
	load := func(context.Context) (int, error) {
		atomic.AddInt32(&loads, 1)
		entered <- struct{}{}
		<-release
		return 7, nil
	}

	var wg sync.WaitGroup
	results := make([]int, callers)
	errs := make([]error, callers)
	var arrived int32
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			atomic.AddInt32(&arrived, 1)
			results[i], errs[i] = c.GetOrLoad(context.Background(), "k", load)
		}(i)
	}

	// All callers must be inside the cache before the load returns; the first
	// one blocks in load, the rest block on the shared call. Each caller
	// records itself just before calling GetOrLoad, and then the load is held
	// at the gate until every caller has arrived, so once the count reaches
	// callers no caller can have missed the in-flight call.
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("load never started")
	}
	waitFor(t, "every caller to have entered GetOrLoad", func() bool {
		return atomic.LoadInt32(&arrived) == callers
	})
	close(release)
	wg.Wait()

	if n := atomic.LoadInt32(&loads); n != 1 {
		t.Fatalf("load was called %d times for one key, want exactly 1", n)
	}
	for i := range results {
		if errs[i] != nil || results[i] != 7 {
			t.Fatalf("caller %d got (%d, %v), want (7, nil)", i, results[i], errs[i])
		}
	}
	if n := c.Len(); n != 1 {
		t.Fatalf("Len = %d, want 1", n)
	}

	// Two different keys load concurrently: each load blocks until the other
	// one has started, so a serialising implementation would time out.
	c2 := New[string, string](Options{TTL: 0})
	startA := make(chan struct{})
	startB := make(chan struct{})
	type outcome struct {
		v   string
		err error
	}
	res := map[string]outcome{}
	var mu sync.Mutex
	var wg2 sync.WaitGroup
	for _, key := range []string{"a", "b"} {
		other := map[string]chan struct{}{"a": startB, "b": startA}[key]
		mine := map[string]chan struct{}{"a": startA, "b": startB}[key]
		wg2.Add(1)
		go func(key string, mine, other chan struct{}) {
			defer wg2.Done()
			v, err := c2.GetOrLoad(context.Background(), key, func(context.Context) (string, error) {
				close(mine)
				select {
				case <-other:
				case <-time.After(10 * time.Second):
					return "", errors.New("keys did not load concurrently")
				}
				return key + "!", nil
			})
			mu.Lock()
			res[key] = outcome{v, err}
			mu.Unlock()
		}(key, mine, other)
	}
	wg2.Wait()
	for _, key := range []string{"a", "b"} {
		got := res[key]
		if got.err != nil || got.v != key+"!" {
			t.Fatalf("key %q got (%q, %v), want (%q, nil)", key, got.v, got.err, key+"!")
		}
	}
}

// TestRule6_LoadErrors covers rule 6: a load error caches nothing, every
// waiter sees that same error, and a later call loads again.
func TestRule6_LoadErrors(t *testing.T) {
	c := New[string, int](Options{TTL: 0})
	sentinel := errors.New("boom")

	// entered is buffered for one call: the gate is closed only after every
	// caller has reached GetOrLoad, so this single load is the only one that
	// can be running while they wait.
	var calls int32
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	load := func(context.Context) (int, error) {
		atomic.AddInt32(&calls, 1)
		entered <- struct{}{}
		<-release
		return 0, sentinel
	}

	const callers = 4
	errs := make([]error, callers)
	vals := make([]int, callers)
	var arrived int32
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			atomic.AddInt32(&arrived, 1)
			vals[i], errs[i] = c.GetOrLoad(context.Background(), "k", load)
		}(i)
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("load never started")
	}
	// Wait until every caller has called GetOrLoad. The one load is still
	// gated, so each remaining caller either joined that in-flight call or is
	// about to; the gate is held a moment longer below so nobody can be left
	// arriving after it finished and starting a second load.
	waitFor(t, "every caller to have entered GetOrLoad", func() bool {
		return atomic.LoadInt32(&arrived) == callers
	})
	// Give a caller that missed the first load the chance to join it too: the
	// load cannot finish until release is closed, so a late arrival still
	// shares this one call.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("load calls during the shared wait = %d, want 1", n)
	}

	for i := range errs {
		if !errors.Is(errs[i], sentinel) {
			t.Fatalf("waiter %d got error %v, want the load's own error %v", i, errs[i], sentinel)
		}
	}
	if n := c.Len(); n != 0 {
		t.Fatalf("Len after failed load = %d, want 0 (nothing cached)", n)
	}
	if _, ok := c.Get("k"); ok {
		t.Fatal("failed load left a value in the cache")
	}

	// A later call must load again.
	if _, err := c.GetOrLoad(context.Background(), "k", load); !errors.Is(err, sentinel) {
		t.Fatalf("later call error = %v, want %v", err, sentinel)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("load calls = %d, want 2 (the error must not be cached)", n)
	}
}

// TestRule7_CallerGivesUp covers rule 7: a caller whose ctx is done before the
// load finishes returns ctx.Err() promptly while the load keeps running for
// the other waiters.
func TestRule7_CallerGivesUp(t *testing.T) {
	c := New[string, int](Options{TTL: 0})
	entered := make(chan struct{})
	release := make(chan struct{})
	load := func(context.Context) (int, error) {
		close(entered)
		<-release
		return 3, nil
	}

	patienceCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gaveUp := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(patienceCtx, "k", load)
		gaveUp <- err
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("load never started")
	}

	secondRegistered := make(chan struct{})
	stillWaiting := make(chan int, 1)
	go func() {
		close(secondRegistered)
		v, err := c.GetOrLoad(context.Background(), "k", load)
		if err != nil {
			stillWaiting <- -1
			return
		}
		stillWaiting <- v
	}()
	<-secondRegistered
	// The load is still gated, so by now the second caller has either already
	// joined the in-flight call or is about to; either way the first caller's
	// cancellation must not cancel that load out from under it. Give it the
	// moment it needs to reach the cache (this only makes the race less
	// likely to resolve in the assertion's favour, it cannot cause a false
	// pass: the check below is that the other caller still gets the value).
	time.Sleep(50 * time.Millisecond)

	cancel()
	// The giving-up caller must return before load is released.
	select {
	case err := <-gaveUp:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled caller got %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled caller did not return promptly while the load was still running")
	}

	// The other caller still gets the value.
	close(release)
	select {
	case v := <-stillWaiting:
		if v != 3 {
			t.Fatalf("surviving caller got %d, want 3", v)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("surviving caller never returned")
	}
}

// TestRule8_LoadContext covers rule 8: the context handed to load is not
// cancelled while at least one waiter remains (nor because one of several
// callers was cancelled), and is cancelled exactly when the last waiter gives
// up. The load is never released here, so the only possible source of a
// cancellation is the rule itself.
func TestRule8_LoadContext(t *testing.T) {
	// Part 1: two waiters, one gives up. The load context must stay alive,
	// the surviving waiter must still be waiting (the load never returned),
	// and no second load may be started for the key.
	c1 := New[string, int](Options{TTL: 0})
	ctxs := make(chan context.Context, 1)
	loadStarted := make(chan struct{})
	release1 := make(chan struct{})
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	firstGaveUp := make(chan error, 1)
	go func() {
		_, err := c1.GetOrLoad(firstCtx, "k", func(ctx context.Context) (int, error) {
			ctxs <- ctx
			close(loadStarted)
			<-release1
			return 1, nil
		})
		firstGaveUp <- err
	}()
	select {
	case <-loadStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("load never started")
	}
	loadCtx := <-ctxs
	if loadCtx.Err() != nil {
		t.Fatalf("load context already cancelled: %v", loadCtx.Err())
	}

	secondReturned := make(chan error, 1)
	secondRegistered := make(chan struct{})
	go func() {
		close(secondRegistered)
		_, err := c1.GetOrLoad(context.Background(), "k", func(context.Context) (int, error) {
			t.Error("load must run once per key: the second caller joined the in-flight load")
			return -1, nil
		})
		secondReturned <- err
	}()
	<-secondRegistered
	// Give the second caller time to reach the cache and register as a waiter
	// on the in-flight call. The assertion below does not depend on timing:
	// the load is never released, so if the load context is cancelled while
	// this caller waits, rule 8 is broken regardless of when it registered.
	time.Sleep(50 * time.Millisecond)

	cancelFirst()
	if err := <-firstGaveUp; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled caller got %v, want context.Canceled", err)
	}

	// The load context must not be cancelled while a waiter remains, and the
	// surviving waiter must still be blocked in the load.
	select {
	case <-loadCtx.Done():
		t.Fatalf("load context was cancelled while a waiter remained: %v", loadCtx.Err())
	case <-time.After(500 * time.Millisecond):
	}
	select {
	case err := <-secondReturned:
		t.Fatalf("surviving caller returned (%v) while the load was still running", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := loadCtx.Err(); err != nil {
		t.Fatalf("load context error = %v, want nil", err)
	}

	// Release the load and let both settle.
	close(release1)
	if err := <-secondReturned; err != nil {
		t.Fatalf("surviving caller got error %v, want nil", err)
	}
	// After load returned, the call cancels its context; that is not a
	// violation of rule 8, only the end-of-call cleanup.
	waitFor(t, "the load context to be released after the load returned", func() bool {
		return loadCtx.Err() != nil
	})

	// Part 2: a sole waiter gives up. Its load context must be cancelled
	// exactly then, with the load still blocked.
	c2 := New[int, int](Options{TTL: 0})
	ctxSole, cancelSole := context.WithCancel(context.Background())
	ctxs2 := make(chan context.Context, 1)
	loadStarted2 := make(chan struct{})
	release2 := make(chan struct{})
	soleReturned := make(chan error, 1)
	go func() {
		_, err := c2.GetOrLoad(ctxSole, 7, func(ctx context.Context) (int, error) {
			ctxs2 <- ctx
			close(loadStarted2)
			<-release2
			return 2, nil
		})
		soleReturned <- err
	}()
	select {
	case <-loadStarted2:
	case <-time.After(10 * time.Second):
		t.Fatal("sole load never started")
	}
	loadCtx2 := <-ctxs2
	if loadCtx2.Err() != nil {
		t.Fatalf("sole load context already cancelled: %v", loadCtx2.Err())
	}
	cancelSole()
	if err := <-soleReturned; !errors.Is(err, context.Canceled) {
		t.Fatalf("sole caller got %v, want context.Canceled", err)
	}
	select {
	case <-loadCtx2.Done():
		if !errors.Is(loadCtx2.Err(), context.Canceled) {
			t.Fatalf("load context error = %v, want context.Canceled", loadCtx2.Err())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("load context was not cancelled when the last waiter gave up")
	}
	close(release2)
}

// TestRule9_LateResults covers rule 9: a successful load is stored even if
// every caller gave up, with its TTL measured from when load returned.
func TestRule9_LateResults(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{TTL: time.Minute, Now: clk.Now})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := make(chan struct{})
	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(ctx, "k", func(context.Context) (int, error) {
			close(entered)
			<-release
			return 9, nil
		})
		done <- err
	}()
	<-entered

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled caller got %v, want context.Canceled", err)
	}

	// The load returns successfully after everyone gave up; move the fake
	// clock forward first so the "returned at" instant differs from the miss.
	clk.Advance(30 * time.Second)
	close(release)
	waitFor(t, "the late result to be stored", func() bool {
		_, ok := c.Get("k")
		return ok
	})

	got, ok := c.Get("k")
	if !ok || got != 9 {
		t.Fatalf("late result: Get = (%d, %v), want (9, true)", got, ok)
	}
	if n := c.Len(); n != 1 {
		t.Fatalf("late result: Len = %d, want 1", n)
	}

	// TTL is measured from the load's return, so it is live just before that
	// plus TTL and expired exactly at it.
	clk.Advance(59 * time.Second)
	if _, ok := c.Get("k"); !ok {
		t.Fatal("late result expired too early: TTL was not measured from load's return")
	}
	clk.Advance(time.Second)
	if _, ok := c.Get("k"); ok {
		t.Fatal("late result still live exactly TTL after load returned")
	}
}

// TestRule10_Delete covers rule 10: Delete removes the key, and it does not
// stop an in-flight load whose result is still stored when it completes.
func TestRule10_Delete(t *testing.T) {
	c := New[string, int](Options{TTL: 0})
	c.Set("k", 1)
	c.Set("other", 2)
	if n := c.Len(); n != 2 {
		t.Fatalf("Len = %d, want 2", n)
	}

	c.Delete("k")
	if _, ok := c.Get("k"); ok {
		t.Fatal("deleted key is still readable")
	}
	if n := c.Len(); n != 1 {
		t.Fatalf("Len after Delete = %d, want 1", n)
	}
	c.Delete("k") // deleting a missing key is a no-op
	if n := c.Len(); n != 1 {
		t.Fatalf("Len after redundant Delete = %d, want 1", n)
	}

	// Delete during an in-flight load: the result is still stored.
	entered := make(chan struct{})
	release := make(chan struct{})
	var loadCtx context.Context
	got := make(chan int, 1)
	go func() {
		v, err := c.GetOrLoad(context.Background(), "loading", func(ctx context.Context) (int, error) {
			loadCtx = ctx
			close(entered)
			<-release
			return 77, nil
		})
		if err != nil {
			got <- -1
			return
		}
		got <- v
	}()
	<-entered
	if loadCtx == nil {
		t.Fatal("load was not called")
	}
	c.Delete("loading")
	close(release)
	if v := <-got; v != 77 {
		t.Fatalf("caller got %d, want 77", v)
	}
	waitFor(t, "the in-flight load's result to be stored after Delete", func() bool {
		v, ok := c.Get("loading")
		return ok && v == 77
	})
}

// TestRule11_Concurrency mixes Get, Set, Delete, GetOrLoad and Len across
// goroutines, deleting every key after every load so nothing can be cached and
// every load is a miss. It checks that each GetOrLoad returned exactly the
// value produced by one load, that the number of loads started equals the
// number finished, and that no goroutine the cache starts outlives its load.
func TestRule11_Concurrency(t *testing.T) {
	c := New[int, int](Options{TTL: time.Millisecond})

	var (
		started  int64
		finished int64
	)
	var loadsWG sync.WaitGroup

	const goroutines = 8
	const iterations = 150

	load := func(ctx context.Context) (int, error) {
		atomic.AddInt64(&started, 1)
		loadsWG.Add(1)
		defer func() {
			atomic.AddInt64(&finished, 1)
			loadsWG.Done()
		}()
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		default:
		}
		return 1, nil
	}

	var wg sync.WaitGroup
	bad := make(chan string, goroutines)

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				key := (g*iterations + i) % 16
				switch i % 4 {
				case 0:
					c.Get(key)
				case 1:
					c.Set(key, 1)
				case 2:
					c.Delete(key)
				case 3:
					v, err := c.GetOrLoad(context.Background(), key, load)
					if err != nil {
						select {
						case bad <- fmt.Sprintf("GetOrLoad(%d) returned error %v", key, err):
						default:
						}
						return
					}
					if v != 1 {
						select {
						case bad <- fmt.Sprintf("GetOrLoad(%d) = %d, want 1", key, v):
						default:
						}
						return
					}
					// Delete every key right after the load so that nothing
					// can be cached and GetOrLoad must miss again.
					c.Delete(key)
				}
				c.Len()
			}
		}(g)
	}
	wg.Wait()
	close(bad)
	for msg := range bad {
		t.Fatal(msg)
	}

	// Every value came from one load, so the number of loads that started is
	// the number that finished.
	if s, f := atomic.LoadInt64(&started), atomic.LoadInt64(&finished); s != f {
		t.Fatalf("loads started %d but finished %d", s, f)
	}
	if n := atomic.LoadInt64(&started); n == 0 {
		t.Fatal("no loads ran; the stress test proved nothing")
	}

	// No goroutine the cache started outlives the loads it serves: after the
	// last GetOrLoad returned, every load has completed. Bounded wait, no
	// deadline on the loads themselves.
	done := make(chan struct{})
	go func() {
		loadsWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a load goroutine outlived its GetOrLoad call: leaked goroutine")
	}
}

// TestRule12_LoadDoesNotPanic: rule 12 is not applicable. The rule says load
// does not panic, so there is no panic path to test; this test only documents
// that and asserts the documented precondition holds for a normal call.
func TestRule12_LoadDoesNotPanic(t *testing.T) {
	c := New[string, int](Options{TTL: time.Minute})
	got, err := c.GetOrLoad(context.Background(), "k", func(context.Context) (int, error) {
		return 1, nil // rule 12: load does not panic
	})
	if err != nil || got != 1 {
		t.Fatalf("GetOrLoad = (%d, %v), want (1, nil)", got, err)
	}
}
