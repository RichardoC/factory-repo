package ttlcache

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// sf5Timeout bounds every wait in these tests so a broken single-flight
// implementation fails fast instead of hanging the test binary.
const sf5Timeout = 5 * time.Second

// sf5Recv receives one value from ch, failing the test (from the test
// goroutine only) if nothing arrives within sf5Timeout.
func sf5Recv[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(sf5Timeout):
		t.Fatal("timed out waiting for a GetOrLoad caller")
		var zero T
		return zero
	}
}

// TestRule5_ConcurrentCallersShareOneLoad: many concurrent GetOrLoad
// callers for one key with a slow load cause load to run exactly once,
// and every caller receives the value that single load produced.
func TestRule5_ConcurrentCallersShareOneLoad(t *testing.T) {
	c := New[string, int](Options{})
	const callers = 16

	var calls atomic.Int32 // how many times load was entered
	var startOnce sync.Once
	started := make(chan struct{}) // closed when the first load begins
	gate := make(chan struct{})    // closed to let the slow load finish

	load := func(ctx context.Context) (int, error) {
		calls.Add(1)
		startOnce.Do(func() { close(started) })
		<-gate // stay "slow" until every caller is parked on this load
		return 42, nil
	}

	type result struct {
		val int
		err error
	}
	results := make(chan result, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := c.GetOrLoad(context.Background(), "k", load)
			results <- result{v, err}
		}()
	}

	// The load must have been entered exactly once, even after all callers
	// have had time to pile up behind it.
	select {
	case <-started:
	case <-time.After(sf5Timeout):
		t.Fatal("load was not called within 5s")
	}
	time.Sleep(100 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("load called %d times for one key, want 1", got)
	}

	close(gate)
	wg.Wait()

	for i := 0; i < callers; i++ {
		r := sf5Recv(t, results)
		if r.err != nil {
			t.Fatalf("caller %d got error %v, want nil", i, r.err)
		}
		if r.val != 42 {
			t.Fatalf("caller %d got %d, want the loaded value 42", i, r.val)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("load called %d times for one key, want exactly 1", got)
	}
}

// TestRule5_DifferentKeysDoNotBlockEachOther: a load for key "a" parked on
// a channel gate must not stop GetOrLoad for key "b" from completing in
// the meantime.
func TestRule5_DifferentKeysDoNotBlockEachOther(t *testing.T) {
	c := New[string, string](Options{})

	gateA := make(chan struct{})
	var callsA atomic.Int32
	startedA := make(chan struct{})
	var startOnceA sync.Once
	loadA := func(ctx context.Context) (string, error) {
		callsA.Add(1)
		startOnceA.Do(func() { close(startedA) })
		<-gateA
		return "A", nil
	}

	type result struct {
		val string
		err error
	}
	chA := make(chan result, 1)
	go func() {
		v, err := c.GetOrLoad(context.Background(), "a", loadA)
		chA <- result{v, err}
	}()

	select {
	case <-startedA:
	case <-time.After(sf5Timeout):
		t.Fatal("load for key a was not called within 5s")
	}

	// Key "a" is now blocked inside its load. Key "b" must still load and
	// return while a's gate stays closed.
	chB := make(chan result, 1)
	go func() {
		v, err := c.GetOrLoad(context.Background(), "b", func(ctx context.Context) (string, error) {
			return "B", nil
		})
		chB <- result{v, err}
	}()
	rb := sf5Recv(t, chB)
	if rb.err != nil {
		t.Fatalf("GetOrLoad for key b returned error %v, want nil", rb.err)
	}
	if rb.val != "B" {
		t.Fatalf("GetOrLoad for key b returned %q, want %q", rb.val, "B")
	}
	if callsA.Load() != 1 {
		t.Fatalf("load for key a ran %d times, want 1", callsA.Load())
	}

	// Now let a finish; its caller gets its own value.
	close(gateA)
	ra := sf5Recv(t, chA)
	if ra.err != nil {
		t.Fatalf("GetOrLoad for key a returned error %v, want nil", ra.err)
	}
	if ra.val != "A" {
		t.Fatalf("GetOrLoad for key a returned %q, want %q", ra.val, "A")
	}
	if c.Len() != 2 {
		t.Fatalf("Len() = %d after loading two keys, want 2", c.Len())
	}
}

// TestRule5_SetDuringLoadStartsNoSecondLoad: Set while a load for the same
// key is in flight must not start a second concurrent load. Callers already
// waiting keep sharing the first load; a later caller may be served by the
// Set value or join the first load, but load is never entered a second
// time while the first is running.
func TestRule5_SetDuringLoadStartsNoSecondLoad(t *testing.T) {
	c := New[string, string](Options{})

	var calls atomic.Int32
	var startOnce sync.Once
	started := make(chan struct{})
	gate := make(chan struct{})
	load := func(ctx context.Context) (string, error) {
		calls.Add(1) // counts before blocking: a concurrent second load shows up here
		startOnce.Do(func() { close(started) })
		<-gate
		return "loaded", nil
	}

	type result struct {
		val string
		err error
	}
	chWaiter := make(chan result, 1)
	go func() {
		v, err := c.GetOrLoad(context.Background(), "k", load)
		chWaiter <- result{v, err}
	}()

	select {
	case <-started:
	case <-time.After(sf5Timeout):
		t.Fatal("load was not called within 5s")
	}

	// Overwrite the key while its load is still parked in the gate.
	c.Set("k", "manual")

	// A caller arriving after the Set must not start a second load for k.
	chLate := make(chan result, 1)
	go func() {
		v, err := c.GetOrLoad(context.Background(), "k", load)
		chLate <- result{v, err}
	}()
	time.Sleep(100 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("Set started a second concurrent load for the key: load called %d times, want 1", got)
	}

	// Let the first load finish; both callers must complete.
	close(gate)
	rWaiter := sf5Recv(t, chWaiter)
	if rWaiter.err != nil {
		t.Fatalf("original waiter got error %v, want nil", rWaiter.err)
	}
	if rWaiter.val != "loaded" {
		t.Fatalf("original waiter got %q, want the loaded value %q", rWaiter.val, "loaded")
	}
	rLate := sf5Recv(t, chLate)
	if rLate.err != nil {
		t.Fatalf("late caller got error %v, want nil", rLate.err)
	}
	// The late caller may have been served by the Set value or by the
	// shared load; both honour "one load per key".
	if rLate.val != "manual" && rLate.val != "loaded" {
		t.Fatalf("late caller got %q, want %q or %q", rLate.val, "manual", "loaded")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("load called %d times for one key, want exactly 1", got)
	}
}
