package ttlcache

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Unix(100, 0)} }
func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestExpiryReplacementAndLen(t *testing.T) {
	clock := newClock()
	c := New[string, int](Options{TTL: 10 * time.Second, Now: clock.Now})
	c.Set("a", 1)
	clock.Add(5 * time.Second)
	if got, ok := c.Get("a"); !ok || got != 1 || c.Len() != 1 {
		t.Fatalf("live entry = %v, %v; Len=%d", got, ok, c.Len())
	}
	clock.Add(5 * time.Second)
	if _, ok := c.Get("a"); ok || c.Len() != 0 {
		t.Fatal("entry at TTL boundary was live")
	}

	c.Set("b", 2)
	clock.Add(6 * time.Second)
	c.Set("b", 3) // replacement restarts TTL
	clock.Add(9 * time.Second)
	if got, ok := c.Get("b"); !ok || got != 3 {
		t.Fatalf("replacement expired using old timestamp: %v, %v", got, ok)
	}
	clock.Add(time.Second)
	if c.Len() != 0 {
		t.Fatal("Len counted an expired entry")
	}
}

func TestZeroTTLNeverExpires(t *testing.T) {
	clock := newClock()
	c := New[string, int](Options{Now: clock.Now})
	c.Set("a", 1)
	clock.Add(100 * 365 * 24 * time.Hour)
	if got, ok := c.Get("a"); !ok || got != 1 {
		t.Fatalf("zero-TTL entry expired: %v, %v", got, ok)
	}
}

func TestLRUEvictionAndUsage(t *testing.T) {
	c := New[string, int](Options{MaxEntries: 2})
	c.Set("a", 1)
	c.Set("b", 2)
	if _, ok := c.Get("a"); !ok {
		t.Fatal("missing a")
	}
	c.Set("c", 3)
	if _, ok := c.Get("b"); ok {
		t.Fatal("least recently used entry b was not evicted")
	}
	if _, ok := c.Get("a"); !ok {
		t.Fatal("recently used entry a was evicted")
	}

	// A Set and a completed load also count as uses.
	if _, err := c.GetOrLoad(context.Background(), "d", func(context.Context) (int, error) { return 4, nil }); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Get("c"); ok {
		t.Fatal("oldest entry c was not evicted")
	}
}

func TestExpiredEntriesDoNotEvictLiveEntries(t *testing.T) {
	clock := newClock()
	c := New[string, int](Options{TTL: time.Second, MaxEntries: 2, Now: clock.Now})
	c.Set("expired1", 1)
	c.Set("expired2", 2)
	clock.Add(2 * time.Second)
	c.Set("live", 3)
	if c.Len() != 1 {
		t.Fatalf("Len=%d, want only the new live entry", c.Len())
	}
	if got, ok := c.Get("live"); !ok || got != 3 {
		t.Fatalf("new live entry evicted: %v, %v", got, ok)
	}
}

func TestGetOrLoadHitSkipsLoad(t *testing.T) {
	c := New[string, int](Options{})
	c.Set("a", 7)
	calls := 0
	got, err := c.GetOrLoad(context.Background(), "a", func(context.Context) (int, error) {
		calls++
		return 9, nil
	})
	if err != nil || got != 7 || calls != 0 {
		t.Fatalf("got %d, %v; calls=%d", got, err, calls)
	}
}

func TestConcurrentGetOrLoadSharesOneLoadPerKey(t *testing.T) {
	c := New[string, int](Options{})
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	load := func(context.Context) (int, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return 42, nil
	}
	const n = 12
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := c.GetOrLoad(context.Background(), "a", load)
			if err != nil || got != 42 {
				errs <- errors.New("wrong shared result")
			}
		}()
	}
	<-started
	// Let all goroutines reach the singleflight wait before releasing it.
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("load calls=%d, want 1", calls.Load())
	}
}

func TestDifferentKeysLoadIndependently(t *testing.T) {
	c := New[string, int](Options{})
	firstStarted := make(chan struct{})
	firstRelease := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(context.Background(), "first", func(context.Context) (int, error) {
			close(firstStarted)
			<-firstRelease
			return 1, nil
		})
		secondDone <- err // only avoids an unused channel; checked below
	}()
	<-firstStarted
	got, err := c.GetOrLoad(context.Background(), "second", func(context.Context) (int, error) { return 2, nil })
	if err != nil || got != 2 {
		t.Fatalf("independent key blocked or wrong: %d, %v", got, err)
	}
	close(firstRelease)
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
}

func TestLoadErrorsSharedNotCachedAndRetried(t *testing.T) {
	c := New[string, int](Options{})
	want := errors.New("load failed")
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	load := func(context.Context) (int, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return 0, want
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.GetOrLoad(context.Background(), "a", load)
			errs <- err
		}()
	}
	<-started
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if !errors.Is(err, want) {
			t.Fatalf("error=%v, want shared %v", err, want)
		}
	}
	if _, ok := c.Get("a"); ok {
		t.Fatal("failed load was cached")
	}
	got, err := c.GetOrLoad(context.Background(), "a", func(context.Context) (int, error) { return 8, nil })
	if err != nil || got != 8 {
		t.Fatalf("later call did not retry and return new value: %d, %v", got, err)
	}
}

func TestCallerCancellationAndSharedLoadContext(t *testing.T) {
	c := New[string, int](Options{})
	loadStarted := make(chan struct{})
	loadRelease := make(chan struct{})
	var loadCtx context.Context
	load := func(ctx context.Context) (int, error) {
		loadCtx = ctx
		close(loadStarted)
		<-loadRelease
		return 99, nil
	}
	ctx1, cancel1 := context.WithCancel(context.Background())
	ctx2, cancel2 := context.WithCancel(context.Background())
	result1 := make(chan error, 1)
	result2 := make(chan error, 1)
	go func() { _, err := c.GetOrLoad(ctx1, "a", load); result1 <- err }()
	<-loadStarted
	go func() { _, err := c.GetOrLoad(ctx2, "a", load); result2 <- err }()
	deadline := time.Now().Add(time.Second)
	for {
		c.mu.Lock()
		waiters := c.loads["a"].waiters
		c.mu.Unlock()
		if waiters == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second caller did not join the shared load")
		}
		time.Sleep(time.Millisecond)
	}
	cancel1()
	if err := <-result1; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled caller error=%v", err)
	}
	select {
	case <-loadCtx.Done():
		t.Fatal("load context cancelled while another waiter remained")
	default:
	}
	cancel2()
	if err := <-result2; !errors.Is(err, context.Canceled) {
		t.Fatalf("second cancelled caller error=%v", err)
	}
	select {
	case <-loadCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("load context not cancelled after final waiter gave up")
	}
	// A successful result is cached even after every caller gave up.
	close(loadRelease)
	deadline = time.Now().Add(time.Second)
	for {
		if got, ok := c.Get("a"); ok {
			if got != 99 {
				t.Fatalf("late result=%d", got)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("successful late result was not cached")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestLateResultTTLStartsWhenLoadReturns(t *testing.T) {
	clock := newClock()
	c := New[string, int](Options{TTL: 10 * time.Second, Now: clock.Now})
	started := make(chan struct{})
	release := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(ctx, "a", func(context.Context) (int, error) {
			close(started)
			<-release
			return 5, nil
		})
		done <- err
	}()
	<-started
	clock.Add(20 * time.Second)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	close(release)
	deadline := time.Now().Add(time.Second)
	for c.Len() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	clock.Add(9 * time.Second)
	if _, ok := c.Get("a"); !ok {
		t.Fatal("late result TTL started when load began, not when it returned")
	}
	clock.Add(time.Second)
	if _, ok := c.Get("a"); ok {
		t.Fatal("late result did not expire TTL after return")
	}
}

func TestDeleteDoesNotStopInflightLoad(t *testing.T) {
	c := New[string, int](Options{})
	started := make(chan struct{})
	release := make(chan struct{})
	c.Set("a", 1)
	done := make(chan error, 1)
	// Delete before the load starts, then delete again while it is in flight.
	c.Delete("a")
	go func() {
		_, err := c.GetOrLoad(context.Background(), "a", func(context.Context) (int, error) {
			close(started)
			<-release
			return 2, nil
		})
		done <- err
	}()
	<-started
	c.Delete("a")
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got, ok := c.Get("a"); !ok || got != 2 {
		t.Fatalf("in-flight result after Delete=%d, %v", got, ok)
	}
}

func TestPromptCancellation(t *testing.T) {
	c := New[string, int](Options{})
	started := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(ctx, "a", func(ctx context.Context) (int, error) {
			close(started)
			<-ctx.Done()
			return 0, ctx.Err()
		})
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled caller did not return promptly")
	}
}
