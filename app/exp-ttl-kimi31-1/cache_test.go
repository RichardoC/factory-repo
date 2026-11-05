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

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func waitForWaiters[K comparable, V any](t *testing.T, cache *Cache[K, V], key K, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		cache.mu.Lock()
		shared := cache.loads[key]
		got := 0
		if shared != nil {
			got = shared.waiters
		}
		cache.mu.Unlock()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiters = %d, want %d", got, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestExpiryAndLen(t *testing.T) {
	clock := newTestClock()
	cache := New[string, int](Options{TTL: time.Second, Now: clock.Now})
	cache.Set("a", 1)
	if got, ok := cache.Get("a"); !ok || got != 1 {
		t.Fatalf("new entry = %v, %v", got, ok)
	}
	clock.Advance(999 * time.Millisecond)
	if cache.Len() != 1 {
		t.Fatal("entry expired before its TTL")
	}
	clock.Advance(time.Millisecond)
	if _, ok := cache.Get("a"); ok {
		t.Fatal("Get returned an entry at its expiration instant")
	}
	cache.Set("b", 2)
	clock.Advance(time.Second)
	if _, err := cache.GetOrLoad(context.Background(), "b", func(context.Context) (int, error) {
		return 3, nil
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := cache.Get("b"); got != 3 {
		t.Fatal("GetOrLoad returned an expired entry instead of loading")
	}
	cache.Set("c", 4)
	clock.Advance(time.Second)
	if cache.Len() != 0 {
		t.Fatal("Len counted an expired entry")
	}
}

func TestZeroTTLNeverExpires(t *testing.T) {
	clock := newTestClock()
	cache := New[string, int](Options{Now: clock.Now})
	cache.Set("a", 1)
	clock.Advance(1000 * time.Hour)
	if got, ok := cache.Get("a"); !ok || got != 1 || cache.Len() != 1 {
		t.Fatal("zero-TTL entry expired")
	}
}

func TestSetReplacementRestartsTTL(t *testing.T) {
	clock := newTestClock()
	cache := New[string, int](Options{TTL: 10 * time.Second, Now: clock.Now})
	cache.Set("a", 1)
	clock.Advance(9 * time.Second)
	cache.Set("a", 2)
	clock.Advance(9 * time.Second)
	if got, ok := cache.Get("a"); !ok || got != 2 {
		t.Fatalf("replacement = %v, %v", got, ok)
	}
	clock.Advance(time.Second)
	if _, ok := cache.Get("a"); ok {
		t.Fatal("replacement did not restart TTL")
	}
}

func TestLRUEvictionAndHits(t *testing.T) {
	cache := New[string, int](Options{MaxEntries: 2})
	cache.Set("a", 1)
	cache.Set("b", 2)
	if _, ok := cache.Get("a"); !ok {
		t.Fatal("missing a")
	}
	cache.Set("c", 3)
	if _, ok := cache.Get("b"); ok {
		t.Fatal("least recently used entry was not evicted")
	}
	if _, err := cache.GetOrLoad(context.Background(), "a", func(context.Context) (int, error) {
		t.Fatal("load called on hit")
		return 0, nil
	}); err != nil {
		t.Fatal(err)
	}
	cache.Set("d", 4)
	if _, ok := cache.Get("c"); ok {
		t.Fatal("GetOrLoad hit did not refresh LRU position of a")
	}
}

func TestExpiredEntriesDoNotEvictLiveEntries(t *testing.T) {
	clock := newTestClock()
	cache := New[string, int](Options{TTL: time.Second, MaxEntries: 2, Now: clock.Now})
	cache.Set("expired", 1)
	clock.Advance(time.Second)
	cache.Set("live", 2)
	cache.Set("new", 3)
	if _, ok := cache.Get("live"); !ok {
		t.Fatal("expired entry caused a live entry to be evicted")
	}
	if cache.Len() != 2 {
		t.Fatalf("Len = %d, want 2", cache.Len())
	}
}

func TestGetOrLoadHitDoesNotCallLoad(t *testing.T) {
	cache := New[string, int](Options{})
	cache.Set("a", 7)
	called := false
	got, err := cache.GetOrLoad(context.Background(), "a", func(context.Context) (int, error) {
		called = true
		return 8, nil
	})
	if err != nil || got != 7 || called {
		t.Fatalf("got %v, %v, called = %v", got, err, called)
	}
}

func TestConcurrentMissesShareOneLoadPerKey(t *testing.T) {
	cache := New[string, int](Options{})
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	load := func(context.Context) (int, error) {
		calls.Add(1)
		close(started)
		<-release
		return 42, nil
	}
	const callers = 20
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := cache.GetOrLoad(context.Background(), "key", load)
			if err != nil || got != 42 {
				errs <- fmt.Errorf("got %d, %v", got, err)
			}
		}()
	}
	<-started
	waitForWaiters(t, cache, "key", callers)
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("load calls = %d, want 1", got)
	}
}

func TestDifferentKeysLoadIndependently(t *testing.T) {
	cache := New[string, int](Options{})
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		_, _ = cache.GetOrLoad(context.Background(), "first", func(context.Context) (int, error) {
			close(firstStarted)
			<-releaseFirst
			return 1, nil
		})
	}()
	<-firstStarted
	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		got, err := cache.GetOrLoad(context.Background(), "second", func(context.Context) (int, error) {
			return 2, nil
		})
		if err != nil || got != 2 {
			t.Errorf("second got %d, %v", got, err)
		}
	}()
	select {
	case <-secondDone:
	case <-time.After(time.Second):
		t.Fatal("different key was blocked by another load")
	}
	close(releaseFirst)
	<-firstDone
}

func TestLoadErrorSharedAndRetried(t *testing.T) {
	cache := New[string, int](Options{})
	wantErr := errors.New("load failed")
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	load := func(context.Context) (int, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
			return 0, wantErr
		}
		return 9, nil
	}
	const callers = 8
	results := make(chan error, callers)
	for range callers {
		go func() {
			_, err := cache.GetOrLoad(context.Background(), "key", load)
			results <- err
		}()
	}
	<-started
	waitForWaiters(t, cache, "key", callers)
	close(release)
	for range callers {
		if err := <-results; !errors.Is(err, wantErr) {
			t.Errorf("waiter error = %v, want %v", err, wantErr)
		}
	}
	if cache.Len() != 0 {
		t.Fatal("failed load was cached")
	}
	got, err := cache.GetOrLoad(context.Background(), "key", load)
	if err != nil || got != 9 || calls.Load() != 2 {
		t.Fatalf("retry got %d, %v, calls = %d", got, err, calls.Load())
	}
}

func TestCancelledCallerReturnsPromptlyAndOtherWaiterContinues(t *testing.T) {
	cache := New[string, int](Options{})
	started := make(chan struct{})
	release := make(chan struct{})
	loadCtx := make(chan context.Context, 1)
	load := func(ctx context.Context) (int, error) {
		loadCtx <- ctx
		close(started)
		<-release
		return 5, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancelledResult := make(chan error, 1)
	go func() {
		_, err := cache.GetOrLoad(ctx, "key", load)
		cancelledResult <- err
	}()
	<-started
	otherResult := make(chan error, 1)
	go func() {
		got, err := cache.GetOrLoad(context.Background(), "key", load)
		if err == nil && got != 5 {
			err = fmt.Errorf("got %d", got)
		}
		otherResult <- err
	}()
	waitForWaiters(t, cache, "key", 2)
	cancel()
	select {
	case err := <-cancelledResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled caller error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled caller did not return promptly")
	}
	sharedCtx := <-loadCtx
	select {
	case <-sharedCtx.Done():
		t.Fatal("one caller's cancellation cancelled the shared load")
	default:
	}
	close(release)
	if err := <-otherResult; err != nil {
		t.Fatal(err)
	}
}

func TestLoadContextCancelledOnlyAfterAllWaitersGiveUp(t *testing.T) {
	cache := New[string, int](Options{})
	started := make(chan struct{})
	observedCtx := make(chan context.Context, 1)
	finished := make(chan struct{})
	load := func(ctx context.Context) (int, error) {
		observedCtx <- ctx
		close(started)
		<-finished
		return 1, nil
	}
	ctx1, cancel1 := context.WithCancel(context.Background())
	ctx2, cancel2 := context.WithCancel(context.Background())
	done := make(chan struct{}, 2)
	go func() { _, _ = cache.GetOrLoad(ctx1, "key", load); done <- struct{}{} }()
	<-started
	go func() { _, _ = cache.GetOrLoad(ctx2, "key", load); done <- struct{}{} }()
	sharedCtx := <-observedCtx
	waitForWaiters(t, cache, "key", 2)
	cancel1()
	<-done
	select {
	case <-sharedCtx.Done():
		t.Fatal("load context cancelled while a waiter remained")
	default:
	}
	cancel2()
	<-done
	select {
	case <-sharedCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("load context not cancelled after every waiter gave up")
	}
	close(finished)
}

func TestSuccessfulLateResultStoredWithCompletionTTL(t *testing.T) {
	clock := newTestClock()
	cache := New[string, int](Options{TTL: 10 * time.Second, Now: clock.Now})
	started := make(chan struct{})
	release := make(chan struct{})
	loadDone := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer close(loadDone)
		_, _ = cache.GetOrLoad(ctx, "key", func(context.Context) (int, error) {
			close(started)
			<-release
			return 11, nil
		})
	}()
	<-started
	cancel()
	clock.Advance(8 * time.Second)
	close(release)
	<-loadDone
	deadline := time.Now().Add(time.Second)
	for {
		got, ok := cache.Get("key")
		if ok && got == 11 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("late result = %d, %v", got, ok)
		}
		time.Sleep(time.Millisecond)
	}
	clock.Advance(9 * time.Second)
	if got, ok := cache.Get("key"); !ok || got != 11 {
		t.Fatalf("late result before expiry = %d, %v", got, ok)
	}
	clock.Advance(time.Second)
	if _, ok := cache.Get("key"); ok {
		t.Fatal("late result TTL did not start when load returned")
	}
}

func TestDeleteDoesNotStopInflightLoad(t *testing.T) {
	cache := New[string, int](Options{})
	cache.Set("key", 1)
	cache.Delete("key")
	if _, ok := cache.Get("key"); ok {
		t.Fatal("Delete did not remove key")
	}
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = cache.GetOrLoad(context.Background(), "key", func(context.Context) (int, error) {
			close(started)
			<-release
			return 2, nil
		})
	}()
	// Delete while the miss load is in flight.
	<-started
	cache.Delete("key")
	close(release)
	<-done
	if got, ok := cache.Get("key"); !ok || got != 2 {
		t.Fatalf("in-flight result after Delete = %d, %v", got, ok)
	}
}

func TestConcurrentUseAndLoadGoroutinesFinish(t *testing.T) {
	cache := New[int, int](Options{TTL: time.Millisecond, MaxEntries: 16})
	var wg sync.WaitGroup
	for worker := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 100 {
				key := (worker + i) % 20
				cache.Set(key, i)
				cache.Get(key)
				cache.Len()
				if i%7 == 0 {
					cache.Delete(key)
				}
				_, _ = cache.GetOrLoad(context.Background(), key, func(context.Context) (int, error) {
					return i, nil
				})
			}
		}()
	}
	wg.Wait()
	cache.mu.Lock()
	loads := len(cache.loads)
	cache.mu.Unlock()
	if loads != 0 {
		t.Fatalf("%d cache load goroutines/state remained after loads", loads)
	}
}
