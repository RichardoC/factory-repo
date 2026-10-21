package ttlcache

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a deterministic clock for tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Unix(1_000_000, 0)}
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

// gate lets a test block a load until it chooses to release it.
type gate struct {
	ch     chan struct{}
	closed atomic.Bool
}

func newGate() *gate { return &gate{ch: make(chan struct{})} }

func (g *gate) release() {
	if g.closed.CompareAndSwap(false, true) {
		close(g.ch)
	}
}

func (g *gate) wait() { <-g.ch }

// Rule 1: expiry — an entry is live while Now() < stored + TTL.
func TestTTLExpiry(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{TTL: 10 * time.Second, Now: clk.Now})

	c.Set("a", 1)
	if v, ok := c.Get("a"); !ok || v != 1 {
		t.Fatalf("live entry miss: got (%v, %v)", v, ok)
	}

	clk.advance(9 * time.Second) // still before t+TTL
	if v, ok := c.Get("a"); !ok || v != 1 {
		t.Fatalf("entry expired early: got (%v, %v)", v, ok)
	}

	clk.advance(1 * time.Second) // exactly at t+TTL: expired
	if _, ok := c.Get("a"); ok {
		t.Fatal("entry returned at exactly t+TTL; must be expired")
	}
	if got := c.Len(); got != 0 {
		t.Fatalf("Len counts expired entries: got %d, want 0", got)
	}
	if _, ok := c.Get("a"); ok {
		t.Fatal("expired entry came back")
	}
}

// Rule 1: TTL 0 means entries never expire.
func TestZeroTTLNeverExpires(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{Now: clk.Now})
	c.Set("a", 1)
	clk.advance(100 * time.Hour)
	if v, ok := c.Get("a"); !ok || v != 1 {
		t.Fatalf("TTL=0 entry expired: got (%v, %v)", v, ok)
	}
	if c.Len() != 1 {
		t.Fatalf("Len = %d, want 1", c.Len())
	}
}

// Rule 2: Set on an existing key replaces the value and restarts the TTL.
func TestSetReplacesAndRestartsTTL(t *testing.T) {
	clk := newFakeClock()
	c := New[string, string](Options{TTL: 10 * time.Second, Now: clk.Now})

	c.Set("k", "v1")
	clk.advance(6 * time.Second)
	c.Set("k", "v2") // replacement restarts the TTL from here
	clk.advance(6 * time.Second)
	if v, ok := c.Get("k"); !ok || v != "v2" {
		t.Fatalf("replacement lost or TTL not restarted: got (%v, %v)", v, ok)
	}
	clk.advance(5 * time.Second) // 11s after the replacement
	if _, ok := c.Get("k"); ok {
		t.Fatal("entry survived past restarted TTL")
	}
}

// Rule 3: LRU eviction of live entries when MaxEntries would be exceeded.
func TestLRUEviction(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{MaxEntries: 2, Now: clk.Now})

	c.Set("a", 1)
	c.Set("b", 2)
	c.Get("a") // touch a; b is now least recently used
	c.Set("c", 3)

	if _, ok := c.Get("b"); ok {
		t.Fatal("LRU entry b was not evicted")
	}
	if v, ok := c.Get("a"); !ok || v != 1 {
		t.Fatalf("recently used a was evicted: got (%v, %v)", v, ok)
	}
	if v, ok := c.Get("c"); !ok || v != 3 {
		t.Fatalf("new entry c missing: got (%v, %v)", v, ok)
	}
	if c.Len() != 2 {
		t.Fatalf("Len = %d, want 2", c.Len())
	}
}

// Rule 3: expired entries never cause a live entry to be evicted.
func TestExpiredEntriesDoNotEvictLiveOnes(t *testing.T) {
	clk := newFakeClock()
	c := New[string, int](Options{TTL: 10 * time.Second, MaxEntries: 2, Now: clk.Now})

	c.Set("old1", 1)
	c.Set("old2", 2)
	clk.advance(11 * time.Second) // both old entries are now expired
	c.Set("new1", 3)
	c.Set("new2", 4) // cap is 2: the expired ones must not push out new1

	if v, ok := c.Get("new1"); !ok || v != 3 {
		t.Fatalf("live entry new1 evicted by expired ones: got (%v, %v)", v, ok)
	}
	if v, ok := c.Get("new2"); !ok || v != 4 {
		t.Fatalf("live entry new2 missing: got (%v, %v)", v, ok)
	}
	if _, ok := c.Get("old1"); ok {
		t.Fatal("expired entry old1 still present")
	}
	if c.Len() != 2 {
		t.Fatalf("Len = %d, want 2", c.Len())
	}
}

// Rule 3: MaxEntries 0 means unbounded.
func TestUnboundedWhenMaxEntriesZero(t *testing.T) {
	c := New[int, int](Options{})
	for i := 0; i < 1000; i++ {
		c.Set(i, i)
	}
	if c.Len() != 1000 {
		t.Fatalf("Len = %d, want 1000", c.Len())
	}
}

// Rule 4: GetOrLoad on a live hit never calls load.
func TestGetOrLoadHitDoesNotLoad(t *testing.T) {
	c := New[string, int](Options{})
	c.Set("k", 7)
	loaded := 0
	v, err := c.GetOrLoad(context.Background(), "k", func(context.Context) (int, error) {
		loaded++
		return 99, nil
	})
	if err != nil {
		t.Fatalf("GetOrLoad error: %v", err)
	}
	if v != 7 || loaded != 0 {
		t.Fatalf("hit called load: got v=%d, loads=%d", v, loaded)
	}
}

// Rule 5: concurrent GetOrLoad callers for the same key share one load.
func TestSingleFlightSameKey(t *testing.T) {
	var calls atomic.Int32
	g := newGate()
	c := New[string, int](Options{})

	const n = 20
	var wg sync.WaitGroup
	results := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, err := c.GetOrLoad(context.Background(), "k", func(context.Context) (int, error) {
				calls.Add(1)
				g.wait()
				return 42, nil
			})
			if err != nil {
				t.Errorf("caller %d: %v", i, err)
				return
			}
			results[i] = v
		}(i)
	}

	// Wait until the load has started and every caller is waiting, then
	// let it finish.
	waitFor(t, func() bool { return calls.Load() == 1 })
	g.release()
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("load ran %d times, want 1", got)
	}
	for i, v := range results {
		if v != 42 {
			t.Fatalf("caller %d got %d, want 42", i, v)
		}
	}
	if v, ok := c.Get("k"); !ok || v != 42 {
		t.Fatalf("value not cached: got (%v, %v)", v, ok)
	}
}

// Rule 5: different keys load independently, without blocking each other.
func TestDifferentKeysLoadIndependently(t *testing.T) {
	g1 := newGate()
	g2 := newGate()
	c := New[string, int](Options{})
	ctx := context.Background()

	var v2 int
	var err2 error
	done2 := make(chan struct{})
	go func() {
		defer close(done2)
		v2, err2 = c.GetOrLoad(ctx, "k2", func(context.Context) (int, error) {
			g2.wait()
			return 2, nil
		})
	}()

	// k1's load stays blocked; k2 must still complete.
	go c.GetOrLoad(ctx, "k1", func(context.Context) (int, error) {
		g1.wait()
		return 1, nil
	})

	g2.release()
	<-done2
	if err2 != nil {
		t.Fatalf("k2 blocked by k1's load: %v", err2)
	}
	if v2 != 2 {
		t.Fatalf("k2 got %d, want 2", v2)
	}
	g1.release()
}

// Rule 6: load errors are not cached, all waiters get the same error, and a
// later GetOrLoad calls load again.
func TestLoadError(t *testing.T) {
	var calls atomic.Int32
	g := newGate()
	c := New[string, int](Options{})
	ctx := context.Background()

	errBoom := context.DeadlineExceeded
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, err := c.GetOrLoad(ctx, "k", func(context.Context) (int, error) {
				calls.Add(1)
				g.wait()
				return 0, errBoom
			})
			errs <- err
		}()
	}
	waitFor(t, func() bool { return calls.Load() == 1 })
	g.release()
	for i := 0; i < 2; i++ {
		if err := <-errs; err != errBoom {
			t.Fatalf("waiter got %v, want %v", err, errBoom)
		}
	}
	if _, ok := c.Get("k"); ok {
		t.Fatal("failed load was cached")
	}

	// A later GetOrLoad calls load again.
	v, err := c.GetOrLoad(ctx, "k", func(context.Context) (int, error) {
		return 5, nil
	})
	if err != nil || v != 5 {
		t.Fatalf("retry after error: got (%v, %v)", v, err)
	}
}

// helper: a load that records the context it was given.
type ctxRecorder struct {
	mu  sync.Mutex
	ctx context.Context
}

func (r *ctxRecorder) load(ctx context.Context) (int, error) {
	r.mu.Lock()
	r.ctx = ctx
	r.mu.Unlock()
	return 1, nil
}

func (r *ctxRecorder) got() context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ctx
}

// Rule 7: a caller whose ctx is done before the load finishes returns
// ctx.Err() promptly while the load keeps running for other waiters.
func TestCallerGivesUpPromptly(t *testing.T) {
	g := newGate()
	c := New[string, int](Options{})
	ctx := context.Background()

	ctxA, cancelA := context.WithCancel(ctx)
	defer cancelA()
	resA := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(ctxA, "k", func(context.Context) (int, error) {
			g.wait()
			return 1, nil
		})
		resA <- err
	}()
	resB := make(chan int, 1)
	errB := make(chan error, 1)
	go func() {
		v, err := c.GetOrLoad(ctx, "k", func(context.Context) (int, error) {
			g.wait()
			return 1, nil
		})
		resB <- v
		errB <- err
	}()

	waitFor(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		call, ok := c.inflight["k"]
		return ok && call.waiters == 2
	})

	cancelA() // A gives up while the load is still blocked
	select {
	case err := <-resA:
		if err != context.Canceled {
			t.Fatalf("giving-up caller got %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("caller A did not return promptly after its ctx was cancelled")
	}
	if g.closed.Load() {
		t.Fatal("the load was stopped by A giving up; it must keep running for B")
	}

	// The load keeps running for B: release it and B must get the value.
	g.release()
	if v := <-resB; v != 1 {
		t.Fatalf("B got %d, want 1", v)
	}
	if err := <-errB; err != nil {
		t.Fatalf("B got error %v", err)
	}
}

// Rule 8: the load's context is cancelled exactly when every waiting caller
// has given up — not when one of several callers is cancelled.
func TestLoadContextCancellation(t *testing.T) {
	g := newGate()
	rec := &ctxRecorder{}
	c := New[string, int](Options{})
	ctx := context.Background()

	ctxA, cancelA := context.WithCancel(ctx)
	defer cancelA()
	ctxB, cancelB := context.WithCancel(ctx)
	defer cancelB()
	doneA := make(chan error, 1)
	doneB := make(chan error, 1)
	load := func(ctx context.Context) (int, error) {
		rec.load(ctx)
		g.wait()
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		default:
			return 1, nil
		}
	}
	go func() { _, err := c.GetOrLoad(ctxA, "k", load); doneA <- err }()
	go func() { _, err := c.GetOrLoad(ctxB, "k", load); doneB <- err }()

	waitFor(t, func() bool { return rec.got() != nil })

	cancelA() // one of two callers gives up
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if rec.got().Err() != nil {
			t.Fatal("load context cancelled while a waiter remained")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := <-doneA; err != context.Canceled {
		t.Fatalf("A got %v, want context.Canceled", err)
	}

	cancelB() // the last waiter gives up: the load context must cancel
	deadline = time.Now().Add(2 * time.Second)
	for {
		if rec.got().Err() != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("load context not cancelled after every waiter gave up")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := <-doneB; err != context.Canceled {
		t.Fatalf("B got %v, want context.Canceled", err)
	}
}

// Rule 8, other side: a load that finishes normally has its context
// cancelled once done.
func TestLoadContextCancelledAfterCompletion(t *testing.T) {
	rec := &ctxRecorder{}
	c := New[string, int](Options{})
	_, err := c.GetOrLoad(context.Background(), "k", rec.load)
	if err != nil {
		t.Fatalf("GetOrLoad: %v", err)
	}
	waitFor(t, func() bool { return rec.got().Err() != nil })
}

// Rule 9: a value from a successful load is stored even if every caller
// gave up, with the TTL starting when load returned.
func TestLateResultStillStored(t *testing.T) {
	clk := newFakeClock()
	g := newGate()
	c := New[string, int](Options{TTL: 10 * time.Second, Now: clk.Now})

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(ctx, "k", func(loadCtx context.Context) (int, error) {
			<-g.ch // ignore loadCtx cancellation: the load runs to completion
			return 9, nil
		})
		errCh <- err
	}()

	waitFor(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		_, ok := c.inflight["k"]
		return ok
	})
	cancel()
	if err := <-errCh; err != context.Canceled {
		t.Fatalf("giving-up caller got %v, want context.Canceled", err)
	}
	if _, ok := c.Get("k"); ok {
		t.Fatal("value present before load returned")
	}

	clk.advance(4 * time.Second) // time passes while nobody is waiting
	g.release()                  // the load returns 9 now

	waitFor(t, func() bool {
		_, ok := c.Get("k")
		return ok
	})
	if v, _ := c.Get("k"); v != 9 {
		t.Fatalf("late result not stored: got %d", v)
	}

	// TTL started when load returned: 6 more seconds must still be live,
	// and 11 must not be.
	clk.advance(6 * time.Second)
	if _, ok := c.Get("k"); !ok {
		t.Fatal("entry expired before t(load return)+TTL")
	}
	clk.advance(5 * time.Second)
	if _, ok := c.Get("k"); ok {
		t.Fatal("entry live past t(load return)+TTL")
	}
}

// Rule 10: Delete removes the key but does not stop an in-flight load; the
// load's result is still stored when it completes.
func TestDeleteDoesNotStopInFlightLoad(t *testing.T) {
	clk := newFakeClock()
	g := newGate()
	c := New[string, int](Options{TTL: 10 * time.Second, Now: clk.Now})
	ctx := context.Background()

	c.Set("k", 1)
	c.Delete("k")
	if _, ok := c.Get("k"); ok {
		t.Fatal("Delete did not remove the key")
	}

	res := make(chan int, 1)
	go func() {
		v, err := c.GetOrLoad(ctx, "k", func(context.Context) (int, error) {
			g.wait()
			return 2, nil
		})
		if err != nil {
			t.Errorf("GetOrLoad: %v", err)
		}
		res <- v
	}()
	waitFor(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		_, ok := c.inflight["k"]
		return ok
	})

	c.Delete("k") // while the load is in flight
	g.release()
	if v := <-res; v != 2 {
		t.Fatalf("caller got %d, want 2", v)
	}
	waitFor(t, func() bool {
		_, ok := c.Get("k")
		return ok
	})
	if v, _ := c.Get("k"); v != 2 {
		t.Fatalf("deleted in-flight load result not stored: got %d", v)
	}
}

// Rule 11: goroutines started by the cache do not outlive the loads they
// serve (checked together with go test -race).
func TestNoLeakedGoroutines(t *testing.T) {
	c := New[string, int](Options{})
	ctx := context.Background()
	before := runtime.NumGoroutine()

	for i := 0; i < 50; i++ {
		if _, err := c.GetOrLoad(ctx, keyFor(i), func(context.Context) (int, error) {
			return i, nil
		}); err != nil {
			t.Fatalf("GetOrLoad: %v", err)
		}
	}
	// Also give-up paths: those callers return early while their loads run.
	for i := 0; i < 50; i++ {
		gctx, cancel := context.WithCancel(ctx)
		g := newGate()
		go func() {
			_, _ = c.GetOrLoad(gctx, keyFor(1000+i), func(context.Context) (int, error) {
				g.wait()
				return i, nil
			})
		}()
		waitFor(t, func() bool {
			c.mu.Lock()
			defer c.mu.Unlock()
			_, ok := c.inflight[keyFor(1000+i)]
			return ok
		})
		cancel()
		g.release()
	}

	waitFor(t, func() bool { return runtime.NumGoroutine() <= before+2 })
}

func keyFor(i int) string { return "k" + itoa(i) }

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for condition")
		}
		time.Sleep(2 * time.Millisecond)
	}
}
