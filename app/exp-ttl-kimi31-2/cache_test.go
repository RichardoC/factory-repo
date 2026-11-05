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

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition did not become true")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestExpiryAndLen(t *testing.T) {
	now := time.Unix(100, 0)
	c := New[string, int](Options{TTL: 10 * time.Second, Now: func() time.Time { return now }})
	c.Set("a", 1)
	now = now.Add(9 * time.Second)
	if got, ok := c.Get("a"); !ok || got != 1 {
		t.Fatalf("before TTL: got %v, %v", got, ok)
	}
	now = now.Add(time.Second) // live strictly before t+TTL, not at it
	if _, ok := c.Get("a"); ok {
		t.Fatal("Get returned entry at its expiry time")
	}
	c.Set("b", 2)
	now = now.Add(20 * time.Second)
	if got := c.Len(); got != 0 {
		t.Fatalf("Len counted expired entry: %d", got)
	}

	forever := New[string, int](Options{Now: func() time.Time { return now }})
	forever.Set("a", 1)
	now = now.Add(100 * 365 * 24 * time.Hour)
	if _, ok := forever.Get("a"); !ok {
		t.Fatal("zero TTL entry expired")
	}
}

func TestReplacementRestartsTTL(t *testing.T) {
	now := time.Unix(0, 0)
	c := New[string, int](Options{TTL: 10 * time.Second, Now: func() time.Time { return now }})
	c.Set("k", 1)
	now = now.Add(8 * time.Second)
	c.Set("k", 2)
	now = now.Add(8 * time.Second)
	if got, ok := c.Get("k"); !ok || got != 2 {
		t.Fatalf("replacement did not restart TTL: %v, %v", got, ok)
	}
}

func TestLRUEvictionAndUses(t *testing.T) {
	t.Run("Set and Get hits", func(t *testing.T) {
		c := New[string, int](Options{MaxEntries: 2})
		c.Set("a", 1)
		c.Set("b", 2)
		if _, ok := c.Get("a"); !ok {
			t.Fatal("missing a")
		}
		c.Set("c", 3)
		if _, ok := c.Get("b"); ok {
			t.Fatal("least recently used b was not evicted")
		}
		if c.Len() != 2 {
			t.Fatalf("Len=%d, want 2", c.Len())
		}
	})
	t.Run("GetOrLoad hits and completed loads", func(t *testing.T) {
		c := New[string, int](Options{MaxEntries: 2})
		load := func(v int) func(context.Context) (int, error) {
			return func(context.Context) (int, error) { return v, nil }
		}
		if _, err := c.GetOrLoad(context.Background(), "a", load(1)); err != nil {
			t.Fatal(err)
		}
		if _, err := c.GetOrLoad(context.Background(), "b", load(2)); err != nil {
			t.Fatal(err)
		}
		calls := 0
		if _, err := c.GetOrLoad(context.Background(), "a", func(context.Context) (int, error) {
			calls++
			return 0, nil
		}); err != nil || calls != 0 {
			t.Fatalf("hit err=%v load calls=%d", err, calls)
		}
		if _, err := c.GetOrLoad(context.Background(), "c", load(3)); err != nil {
			t.Fatal(err)
		}
		if _, ok := c.Get("b"); ok {
			t.Fatal("GetOrLoad usage did not protect a from LRU eviction")
		}
	})
}

func TestExpiredEntriesNeverEvictLiveEntries(t *testing.T) {
	now := time.Unix(0, 0)
	c := New[string, int](Options{TTL: time.Second, MaxEntries: 2, Now: func() time.Time { return now }})
	c.Set("expired", 1)
	now = now.Add(2 * time.Second)
	c.Set("a", 2)
	c.Set("b", 3)
	if c.Len() != 2 {
		t.Fatalf("Len=%d, want 2", c.Len())
	}
	if _, ok := c.Get("a"); !ok {
		t.Fatal("expired entry caused live a to be evicted")
	}
	if _, ok := c.Get("b"); !ok {
		t.Fatal("live b was evicted")
	}
}

func TestGetOrLoadHitDoesNotCallLoad(t *testing.T) {
	c := New[string, int](Options{})
	c.Set("k", 7)
	called := false
	got, err := c.GetOrLoad(context.Background(), "k", func(context.Context) (int, error) {
		called = true
		return 0, nil
	})
	if err != nil || got != 7 || called {
		t.Fatalf("got=%d err=%v called=%v", got, err, called)
	}
}

func TestOneLoadPerKey(t *testing.T) {
	c := New[string, int](Options{})
	const callers = 20
	start := make(chan struct{})
	release := make(chan struct{})
	var loads atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			v, err := c.GetOrLoad(context.Background(), "k", func(context.Context) (int, error) {
				loads.Add(1)
				<-release
				return 42, nil
			})
			if err != nil || v != 42 {
				errs <- fmt.Errorf("value=%d err=%v", v, err)
			}
		}()
	}
	close(start)
	waitFor(t, func() bool { return loads.Load() == 1 })
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if got := loads.Load(); got != 1 {
		t.Fatalf("load ran %d times, want 1", got)
	}
}

func TestDifferentKeysLoadIndependently(t *testing.T) {
	c := New[string, int](Options{})
	bothStarted := make(chan struct{})
	release := make(chan struct{})
	var started atomic.Int32
	load := func(context.Context) (int, error) {
		if started.Add(1) == 2 {
			close(bothStarted)
		}
		<-release
		return 1, nil
	}
	errCh := make(chan error, 2)
	for _, key := range []string{"a", "b"} {
		go func() {
			_, err := c.GetOrLoad(context.Background(), key, load)
			errCh <- err
		}()
	}
	select {
	case <-bothStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("one key's load blocked the other")
	}
	close(release)
	for range 2 {
		if err := <-errCh; err != nil {
			t.Fatal(err)
		}
	}
}

func TestLoadErrorsSharedAndRetried(t *testing.T) {
	c := New[string, int](Options{})
	want := errors.New("load failed")
	release := make(chan struct{})
	started := make(chan struct{})
	load := func(context.Context) (int, error) {
		close(started)
		<-release
		return 0, want
	}
	errs := make(chan error, 2)
	go func() { _, err := c.GetOrLoad(context.Background(), "k", load); errs <- err }()
	<-started
	go func() {
		_, err := c.GetOrLoad(context.Background(), "k", func(context.Context) (int, error) {
			t.Error("second caller started another load")
			return 0, nil
		})
		errs <- err
	}()
	waitFor(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.loads["k"].waiters == 2
	})
	close(release)
	for range 2 {
		if err := <-errs; !errors.Is(err, want) {
			t.Fatalf("err=%v, want shared %v", err, want)
		}
	}
	if c.Len() != 0 {
		t.Fatal("failed load was cached")
	}
	got, err := c.GetOrLoad(context.Background(), "k", func(context.Context) (int, error) { return 9, nil })
	if err != nil || got != 9 {
		t.Fatalf("retry got=%d err=%v", got, err)
	}
}

func TestCallerCancellationAndLoadContext(t *testing.T) {
	c := New[string, int](Options{})
	loadStarted := make(chan context.Context, 1)
	finish := make(chan struct{})
	load := func(ctx context.Context) (int, error) {
		loadStarted <- ctx
		<-finish
		return 5, nil
	}
	ctx1, cancel1 := context.WithCancel(context.Background())
	ctx2, cancel2 := context.WithCancel(context.Background())
	result1 := make(chan error, 1)
	result2 := make(chan error, 1)
	go func() { _, err := c.GetOrLoad(ctx1, "k", load); result1 <- err }()
	loadCtx := <-loadStarted
	go func() { _, err := c.GetOrLoad(ctx2, "k", load); result2 <- err }()
	waitFor(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.loads["k"].waiters == 2
	})
	cancel1()
	if err := <-result1; !errors.Is(err, context.Canceled) {
		t.Fatalf("first caller err=%v", err)
	}
	select {
	case <-loadCtx.Done():
		t.Fatal("load context cancelled while a waiter remained")
	default:
	}
	cancel2()
	if err := <-result2; !errors.Is(err, context.Canceled) {
		t.Fatalf("second caller err=%v", err)
	}
	select {
	case <-loadCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("load context not cancelled after final waiter gave up")
	}
	close(finish) // no cache goroutine remains beyond the load
	waitFor(t, func() bool {
		_, ok := c.Get("k")
		return ok
	})
}

func TestLateResultStoredAfterAllCallersGiveUp(t *testing.T) {
	c := New[string, int](Options{TTL: time.Hour})
	started := make(chan struct{})
	result := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_, err := c.GetOrLoad(ctx, "k", func(loadCtx context.Context) (int, error) {
			close(started)
			<-loadCtx.Done() // deliberately finish after cancellation
			return 11, nil
		})
		result <- err
	}()
	<-started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	waitFor(t, func() bool {
		v, ok := c.Get("k")
		return ok && v == 11
	})
}

func TestDeleteDoesNotStopInflightLoad(t *testing.T) {
	c := New[string, int](Options{})
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(context.Background(), "k", func(context.Context) (int, error) {
			close(started)
			<-release
			return 8, nil
		})
		done <- err
	}()
	<-started
	c.Delete("k")
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if v, ok := c.Get("k"); !ok || v != 8 {
		t.Fatalf("late result after Delete: %d, %v", v, ok)
	}
}

func TestCompletedLoadEvictionIgnoresExpiredEntries(t *testing.T) {
	now := time.Unix(0, 0)
	c := New[string, int](Options{TTL: time.Second, MaxEntries: 2, Now: func() time.Time { return now }})
	c.Set("old", 1)
	now = now.Add(500 * time.Millisecond)
	c.Set("live", 2)
	if _, ok := c.Get("old"); !ok { // old is MRU and expires first
		t.Fatal("old missing")
	}
	now = now.Add(600 * time.Millisecond)
	if _, err := c.GetOrLoad(context.Background(), "new", func(context.Context) (int, error) { return 3, nil }); err != nil {
		t.Fatal(err)
	}
	if c.Len() != 2 {
		t.Fatalf("Len=%d", c.Len())
	}
	if _, ok := c.Get("new"); !ok {
		t.Fatal("new live value evicted while expired values existed")
	}
}
