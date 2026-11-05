package ttlcache

import (
	"context"
	"math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// lc9BaseTime is the epoch of the fake clocks used by these tests.
func lc9BaseTime() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

// lc9Clock is a deterministic injectable clock (Options.Now).
type lc9Clock struct {
	mu  sync.Mutex
	now time.Time
}

// lc9Now returns the current fake time.
func (m *lc9Clock) lc9Now() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.now
}

// lc9Set advances/rewinds the fake clock.
func (m *lc9Clock) lc9Set(t time.Time) {
	m.mu.Lock()
	m.now = t
	m.mu.Unlock()
}

// lc9Result carries a GetOrLoad outcome over a channel.
type lc9Result struct {
	v   string
	err error
}

// lc9WaitFor polls cond until it holds or timeout elapses. It reports
// whether cond ever held.
func lc9WaitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return cond()
}

// TestRule9_LateResultStoredWithTTLFromLoadFinish covers rule 9: a load
// that succeeds after every waiter has given up is still stored, and its
// TTL runs from when load returned, not from when GetOrLoad was called.
func TestRule9_LateResultStoredWithTTLFromLoadFinish(t *testing.T) {
	const ttl = time.Minute
	const callTime = 0 * time.Second    // when GetOrLoad is called
	const finishTime = 10 * time.Second // when load returns
	clk := &lc9Clock{now: lc9BaseTime()}
	c := New[string, int](Options{TTL: ttl, Now: clk.lc9Now})

	loadStarted := make(chan struct{})
	release := make(chan struct{})
	loadReturned := make(chan struct{})
	load := func(ctx context.Context) (int, error) {
		close(loadStarted)
		<-release
		defer close(loadReturned)
		return 42, nil
	}

	// Two waiters share the one in-flight load; both give up before it
	// finishes, so by the time the load returns no waiter is left.
	const waiters = 2
	var wg sync.WaitGroup
	errs := make(chan error, waiters)
	cancels := make([]context.CancelFunc, 0, waiters)
	for i := 0; i < waiters; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancels = append(cancels, cancel)
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := c.GetOrLoad(ctx, "k", load)
			if v != 0 {
				t.Errorf("gave-up waiter should get the zero value, got %d", v)
			}
			errs <- err
		}()
	}
	<-loadStarted
	for _, cancel := range cancels {
		cancel()
	}
	giveUpDeadline := time.After(5 * time.Second)
	for i := 0; i < waiters; i++ {
		select {
		case err := <-errs:
			if err != context.Canceled {
				t.Fatalf("gave-up waiter should get context.Canceled, got %v", err)
			}
		case <-giveUpDeadline:
			t.Fatal("waiter did not give up promptly after cancellation")
		}
	}
	wg.Wait()

	// All GetOrLoad waiters are now gone. Let the load finish at
	// finishTime (10s after the call): the value must still be stored,
	// with its TTL measured from finishTime.
	clk.lc9Set(lc9BaseTime().Add(finishTime))
	close(release)
	<-loadReturned
	if !lc9WaitFor(t, time.Second, func() bool {
		v, ok := c.Get("k")
		return ok && v == 42
	}) {
		t.Fatal("load result was not stored although no waiter was left")
	}

	// Still live at finishTime+ttl-1ms. Had the TTL been measured from
	// callTime it would already have expired 10 seconds earlier.
	clk.lc9Set(lc9BaseTime().Add(finishTime + ttl - time.Millisecond))
	if v, ok := c.Get("k"); !ok || v != 42 {
		t.Fatalf("entry should live until finishTime+TTL (finish-time based), ok=%v v=%d", ok, v)
	}

	// Exactly at finishTime+TTL it is expired (rule 1: live while now < t+TTL).
	clk.lc9Set(lc9BaseTime().Add(finishTime + ttl))
	if _, ok := c.Get("k"); ok {
		t.Fatal("entry should be expired at loadFinishTime+TTL")
	}
	if c.Len() != 0 {
		t.Fatalf("Len should not count the expired entry, got %d", c.Len())
	}
}

// TestRule10_DeleteDoesNotStopInflightLoad covers rule 10: Delete removes
// the key but a load already in flight is unaffected — its result is still
// stored when it completes, so the value comes back.
func TestRule10_DeleteDoesNotStopInflightLoad(t *testing.T) {
	c := New[string, string](Options{TTL: time.Minute})

	// Delete removes a stored key.
	c.Set("k", "old")
	if _, ok := c.Get("k"); !ok {
		t.Fatal("Set then Get should hit")
	}
	c.Delete("k")
	if _, ok := c.Get("k"); ok {
		t.Fatal("Get after Delete should miss")
	}
	if c.Len() != 0 {
		t.Fatalf("Len after Delete should be 0, got %d", c.Len())
	}
	c.Delete("absent") // deleting a missing key is a harmless no-op

	loadStarted := make(chan struct{})
	release := make(chan struct{})
	var loadsRun atomic.Int32
	load := func(ctx context.Context) (string, error) {
		loadsRun.Add(1)
		close(loadStarted)
		<-release
		return "fresh", nil
	}
	startedLoad := func(ctx context.Context) (string, error) {
		return c.GetOrLoad(ctx, "k", load)
	}

	// First caller starts the single in-flight load on "k".
	got := make(chan lc9Result, 2)
	go func() {
		v, err := startedLoad(context.Background())
		got <- lc9Result{v, err}
	}()
	<-loadStarted

	// Delete while the load runs: the entry stays absent...
	c.Delete("k")
	if _, ok := c.Get("k"); ok {
		t.Fatal("Get after Delete should miss while load is in flight")
	}
	if c.Len() != 0 {
		t.Fatalf("Len should be 0 while the load is in flight, got %d", c.Len())
	}

	// ...but the in-flight load is NOT stopped: a caller arriving after
	// the Delete joins the same running load rather than starting a new
	// one (rule 10: Delete does not stop an in-flight load).
	secondLoadRan := make(chan struct{}, 1)
	go func() {
		v, err := c.GetOrLoad(context.Background(), "k", func(ctx context.Context) (string, error) {
			secondLoadRan <- struct{}{}
			return "second", nil
		})
		got <- lc9Result{v, err}
	}()
	time.Sleep(50 * time.Millisecond) // let the joining caller reach the load
	close(release)
	for i := 0; i < 2; i++ {
		select {
		case r := <-got:
			if r.err != nil || r.v != "fresh" {
				t.Fatalf("caller got (%q, %v), want (fresh, nil)", r.v, r.err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("callee did not return after the load finished")
		}
	}
	select {
	case <-secondLoadRan:
		t.Fatal("the post-Delete caller started a second load; Delete stopped or bypassed the in-flight load")
	default:
	}
	if n := loadsRun.Load(); n != 1 {
		t.Fatalf("want exactly 1 load, got %d", n)
	}

	// The load has completed: its result was stored although Delete ran
	// mid-flight, so the value is back.
	if v, ok := c.Get("k"); !ok || v != "fresh" {
		t.Fatalf("loaded value should be stored after Delete-during-load, ok=%v v=%q", ok, v)
	}
	// And Delete still removes it afterwards.
	c.Delete("k")
	if _, ok := c.Get("k"); ok {
		t.Fatal("Get after Delete should miss")
	}
	if c.Len() != 0 {
		t.Fatalf("Len after final Delete should be 0, got %d", c.Len())
	}
}

// TestRule11_ConcurrentStressDrainsAllLoadGoroutines covers rule 11: many
// goroutines doing Set/Get/Delete/Len plus GetOrLoad on shared and distinct
// keys, some callers cancelled mid-load; every load completes and no
// goroutine the cache started outlives the loads it serves.
func TestRule11_ConcurrentStressDrainsAllLoadGoroutines(t *testing.T) {
	const (
		workers    = 24
		iters      = 200
		sharedKeys = 20
		maxEntries = 32
	)
	c := New[int, int](Options{TTL: 20 * time.Millisecond, MaxEntries: maxEntries})
	baseGoroutines := runtime.NumGoroutine()

	// loads counts load invocations. The initial extra Add(1) keeps the
	// counter above zero while loads can still start, so loads.Done()
	// followed by loads.Wait() below really waits for every load.
	var loads sync.WaitGroup
	loads.Add(1)
	var loadsStarted, cancelledMidLoad atomic.Int32

	// Deterministic phase: eight distinct keys, one caller each, whose load
	// blocks longer than the caller waits — every one of them is cancelled
	// mid-load.
	var giversUp sync.WaitGroup
	for i := 0; i < 8; i++ {
		key := 5000 + i
		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		giversUp.Add(1)
		go func() {
			defer giversUp.Done()
			defer cancel()
			c.GetOrLoad(ctx, key, func(ctx context.Context) (int, error) {
				loads.Add(1)
				defer loads.Done()
				loadsStarted.Add(1)
				select {
				case <-ctx.Done():
					cancelledMidLoad.Add(1)
					return 0, ctx.Err()
				case <-time.After(30 * time.Millisecond):
					return 1, nil
				}
			})
		}()
	}
	giversUp.Wait()

	// Stress phase: mixed Set/Get/Delete/Len plus GetOrLoad on shared and
	// per-worker-distinct keys, with some callers giving up mid-load.
	var workerWG sync.WaitGroup
	for w := 0; w < workers; w++ {
		workerWG.Add(1)
		go func(w int) {
			defer workerWG.Done()
			r := rand.New(rand.NewSource(time.Now().UnixNano() + int64(w+1)*1000003))
			for i := 0; i < iters; i++ {
				key := r.Intn(sharedKeys)
				distinct := 100000*(w+1) + r.Intn(50)
				k := key
				if r.Intn(2) == 0 {
					k = distinct
				}
				switch r.Intn(6) {
				case 0:
					c.Set(k, r.Intn(1000))
				case 1:
					c.Get(k)
				case 2:
					c.Delete(k)
				case 3:
					if n := c.Len(); n < 0 || n > maxEntries {
						t.Errorf("Len()=%d out of range [0,%d]", n, maxEntries)
					}
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Duration(r.Intn(16))*time.Millisecond)
				block := time.Duration(r.Intn(25)) * time.Millisecond
				c.GetOrLoad(ctx, k, func(ctx context.Context) (int, error) {
					loads.Add(1)
					defer loads.Done()
					loadsStarted.Add(1)
					select {
					case <-ctx.Done():
						cancelledMidLoad.Add(1)
						return 0, ctx.Err()
					case <-time.After(block):
						return 1, nil
					}
				})
				cancel()
			}
		}(w)
	}
	workerWG.Wait()

	// All callers are done; no new load can start. Release the counter's
	// dummy hold and require every load to finish within a few seconds.
	loads.Done()
	drained := make(chan struct{})
	go func() {
		loads.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("loads started by the cache did not drain within 5s of test end")
	}

	if n := loadsStarted.Load(); n == 0 {
		t.Fatal("stress ran no loads at all")
	}
	if n := cancelledMidLoad.Load(); n < 8 {
		t.Fatalf("want at least 8 loads cancelled mid-load, got %d", n)
	}
	if n := c.Len(); n < 0 || n > maxEntries {
		t.Errorf("final Len()=%d out of range [0,%d]", n, maxEntries)
	}

	// No goroutine the cache started outlives the loads it serves: with
	// every load finished, the goroutine count returns to the baseline.
	if !lc9WaitFor(t, 3*time.Second, func() bool {
		return runtime.NumGoroutine() <= baseGoroutines+2
	}) {
		t.Fatalf("goroutines linger after all loads drained: %d > baseline %d",
			runtime.NumGoroutine(), baseGoroutines)
	}
}
