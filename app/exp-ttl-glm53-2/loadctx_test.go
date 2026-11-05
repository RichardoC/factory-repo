package ttlcache

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// cx6errLoadFailed is the sentinel error handed back by the failing loads
// used in these tests, so waiters can be checked for errors.Is identity.
var cx6errLoadFailed = errors.New("cx6: load failed")

// cx6timeout bounds every channel wait in these tests so a stuck test
// fails in bounded time instead of hanging.
const cx6timeout = 5 * time.Second

// cx6recv waits for one value from ch, failing the test if nothing arrives
// within cx6timeout.
func cx6recv[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(cx6timeout):
		t.Fatalf("cx6recv: timed out after %v waiting for a value", cx6timeout)
	}
	var zero T
	return zero
}

// cx6grabErr waits for one error from ch, failing the test on timeout, and
// returns it.
func cx6grabErr(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(cx6timeout):
		t.Fatalf("cx6grabErr: timed out after %v waiting for an error", cx6timeout)
	}
	return nil
}

// Rule 6: a load error is shared by every concurrent waiter (same
// errors.Is identity), nothing is cached, and a later GetOrLoad calls
// load again.
func TestRule6_ErrorSharedByConcurrentWaiters(t *testing.T) {
	c := New[string, int](Options{TTL: time.Minute})

	release := make(chan struct{})
	loadStarted := make(chan struct{})
	var calls int32
	load := func(ctx context.Context) (int, error) {
		atomic.AddInt32(&calls, 1)
		close(loadStarted)
		<-release
		return 0, cx6errLoadFailed
	}

	const waiters = 3
	ready := make(chan struct{}, waiters)
	errCh := make(chan error, waiters)
	for i := 0; i < waiters; i++ {
		go func() {
			ready <- struct{}{}
			_, err := c.GetOrLoad(context.Background(), "k", load)
			errCh <- err
		}()
	}
	for i := 0; i < waiters; i++ {
		cx6recv(t, ready)
	}
	// The load has started; give the other waiters time to join it. If any
	// of them had started a second load, calls would already exceed 1.
	<-loadStarted
	time.Sleep(100 * time.Millisecond)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("%d concurrent waiters started %d loads, want a single shared load", waiters, got)
	}

	close(release)
	for i := 0; i < waiters; i++ {
		err := cx6grabErr(t, errCh)
		if !errors.Is(err, cx6errLoadFailed) {
			t.Fatalf("waiter %d got error %v, want the very same load error", i, err)
		}
	}

	// Nothing is cached after a failed load.
	if _, ok := c.Get("k"); ok {
		t.Fatal("Get found a value after a failed load, want the cache empty")
	}
	if n := c.Len(); n != 0 {
		t.Fatalf("Len() = %d after a failed load, want 0", n)
	}
}

// Rule 6: a later GetOrLoad calls load again after an error (count the
// calls) and then stores the successful value.
func TestRule6_LaterGetOrLoadCallsLoadAgain(t *testing.T) {
	c := New[string, int](Options{TTL: time.Minute})
	var calls int32
	load := func(ctx context.Context) (int, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			return 0, cx6errLoadFailed
		}
		return 42, nil
	}

	_, err := c.GetOrLoad(context.Background(), "k", load)
	if !errors.Is(err, cx6errLoadFailed) {
		t.Fatalf("first GetOrLoad error = %v, want the load error", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("load called %d times by the first GetOrLoad, want 1", got)
	}

	v, err := c.GetOrLoad(context.Background(), "k", load)
	if err != nil {
		t.Fatalf("second GetOrLoad error = %v, want nil", err)
	}
	if v != 42 {
		t.Fatalf("second GetOrLoad value = %d, want 42", v)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("load called %d times in total, want a second call after the error", got)
	}
	if got, ok := c.Get("k"); !ok || got != 42 {
		t.Fatalf("Get after the successful reload = (%d, %v), want (42, true)", got, ok)
	}
}

// Rule 7: a caller whose ctx is cancelled mid-load returns ctx.Err()
// promptly while the load is still running, and the load keeps running
// for the remaining caller, which still gets the value.
func TestRule7_CancelledCallerReturnsPromptlyWhileLoadRunning(t *testing.T) {
	c := New[string, int](Options{TTL: time.Minute})

	release := make(chan struct{})
	loadCtxCh := make(chan context.Context, 1)
	load := func(ctx context.Context) (int, error) {
		loadCtxCh <- ctx
		<-release
		return 99, nil
	}

	ctxCancel, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(ctxCancel, "k", load)
		errCh <- err
	}()
	loadCtx := cx6recv(t, loadCtxCh)

	// A second caller waits on the same load and does not give up.
	remValCh := make(chan int, 1)
	remErrCh := make(chan error, 1)
	go func() {
		v, err := c.GetOrLoad(context.Background(), "k", load)
		remValCh <- v
		remErrCh <- err
	}()
	// Let the second caller join the in-flight load before cancelling the
	// first one.
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled caller error = %v, want context.Canceled", err)
		}
	case <-time.After(cx6timeout):
		t.Fatal("cancelled caller did not return within the timeout")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("cancelled caller took %v to return, want prompt", d)
	}

	// The returned caller left while the load was still running, and the
	// load's context was not cancelled because one waiter remains.
	select {
	case <-loadCtx.Done():
		t.Fatal("load context cancelled while one waiter still remains")
	default:
	}

	close(release)
	if err := cx6grabErr(t, remErrCh); err != nil {
		t.Fatalf("remaining caller error = %v, want nil", err)
	}
	if v := cx6recv(t, remValCh); v != 99 {
		t.Fatalf("remaining caller value = %d, want 99 from the still-running load", v)
	}
	if got, ok := c.Get("k"); !ok || got != 99 {
		t.Fatalf("Get after the load = (%d, %v), want (99, true)", got, ok)
	}
}

// Rule 7: a caller whose ctx deadline expires while the load is still
// running returns context.DeadlineExceeded promptly; the load keeps
// running for the remaining caller, which still gets the value.
func TestRule7_DeadlineCallerReturnsPromptlyWhileLoadRunning(t *testing.T) {
	c := New[string, int](Options{TTL: time.Minute})

	release := make(chan struct{})
	loadCtxCh := make(chan context.Context, 1)
	load := func(ctx context.Context) (int, error) {
		loadCtxCh <- ctx
		<-release
		return 77, nil
	}

	ctxDeadline, cancelDeadline := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelDeadline()
	errCh := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(ctxDeadline, "k", load)
		errCh <- err
	}()
	loadCtx := cx6recv(t, loadCtxCh)

	remValCh := make(chan int, 1)
	remErrCh := make(chan error, 1)
	go func() {
		v, err := c.GetOrLoad(context.Background(), "k", load)
		remValCh <- v
		remErrCh <- err
	}()
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("deadline caller error = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(cx6timeout):
		t.Fatal("deadline caller did not return within the timeout")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("deadline caller took %v past its deadline to return, want prompt", d)
	}
	select {
	case <-loadCtx.Done():
		t.Fatal("load context cancelled while one waiter still remains")
	default:
	}

	// The load is still running; let it finish for the remaining caller.
	close(release)
	if err := cx6grabErr(t, remErrCh); err != nil {
		t.Fatalf("remaining caller error = %v, want nil", err)
	}
	if v := cx6recv(t, remValCh); v != 77 {
		t.Fatalf("remaining caller value = %d, want 77", v)
	}
}

// Rule 7: a caller that arrives with an already-done ctx returns ctx.Err()
// promptly; its giving up cancels the load's context since it is the only
// waiter (rule 8).
func TestRule7_PreCancelledCtxReturnsPromptly(t *testing.T) {
	c := New[string, int](Options{TTL: time.Minute})

	loadEntered := make(chan context.Context, 1)
	load := func(ctx context.Context) (int, error) {
		loadEntered <- ctx
		<-ctx.Done()
		return 0, ctx.Err()
	}

	ctxCancel, cancel := context.WithCancel(context.Background())
	cancel()

	errCh := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(ctxCancel, "k", load)
		errCh <- err
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("pre-cancelled caller error = %v, want context.Canceled", err)
		}
	case <-time.After(cx6timeout):
		t.Fatal("pre-cancelled caller did not return within the timeout")
	}
	// The lone waiter gave up, so the load's context is cancelled.
	loadCtx := cx6recv(t, loadEntered)
	select {
	case <-loadCtx.Done():
	case <-time.After(cx6timeout):
		t.Fatal("load context not cancelled after its only waiter gave up")
	}
	if _, ok := c.Get("k"); ok {
		t.Fatal("Get found a value, want the cache empty")
	}
}

// Rule 8: the load's context is cancelled only once every waiter has given
// up; cancelling one of two callers leaves it live.
func TestRule8_LoadCtxCancelledWhenAllGiveUp(t *testing.T) {
	c := New[string, int](Options{TTL: time.Minute})

	release := make(chan struct{})
	loadCtxCh := make(chan context.Context, 1)
	load := func(ctx context.Context) (int, error) {
		loadCtxCh <- ctx
		<-release
		// Return a value (rule 9 territory) so the started goroutine ends.
		return 5, nil
	}

	ctxA, cancelA := context.WithCancel(context.Background())
	ctxB, cancelB := context.WithCancel(context.Background())
	errA := make(chan error, 1)
	errB := make(chan error, 1)
	ready := make(chan struct{}, 2)
	go func() {
		ready <- struct{}{}
		_, err := c.GetOrLoad(ctxA, "k", load)
		errA <- err
	}()
	go func() {
		ready <- struct{}{}
		_, err := c.GetOrLoad(ctxB, "k", load)
		errB <- err
	}()
	cx6recv(t, ready)
	cx6recv(t, ready)
	time.Sleep(100 * time.Millisecond) // let both join the shared load
	loadCtx := cx6recv(t, loadCtxCh)

	// One of two callers gives up: the load ctx must stay live.
	cancelA()
	if err := cx6grabErr(t, errA); !errors.Is(err, context.Canceled) {
		t.Fatalf("caller A error = %v, want context.Canceled", err)
	}
	select {
	case <-loadCtx.Done():
		t.Fatal("load context cancelled after only one of two callers gave up")
	default:
	}

	// The last caller gives up too: the load ctx must now be cancelled.
	cancelB()
	if err := cx6grabErr(t, errB); !errors.Is(err, context.Canceled) {
		t.Fatalf("caller B error = %v, want context.Canceled", err)
	}
	select {
	case <-loadCtx.Done():
	case <-time.After(cx6timeout):
		t.Fatal("load context not cancelled after every waiter gave up")
	}

	// Let the load return so the cache's goroutine ends.
	close(release)
}

// Rule 8: the load's context is not cancelled while at least one waiter
// remains, even though another waiter of the same load was cancelled
// (one of two callers cancelled -> load still serves the survivor).
func TestRule8_LoadCtxSurvivesOneWaiterLeaving(t *testing.T) {
	c := New[string, int](Options{TTL: time.Minute})

	release := make(chan struct{})
	loadCtxCh := make(chan context.Context, 1)
	var calls int32
	load := func(ctx context.Context) (int, error) {
		atomic.AddInt32(&calls, 1)
		loadCtxCh <- ctx
		<-release
		return 55, nil
	}

	ctxA, cancelA := context.WithCancel(context.Background())
	errA := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(ctxA, "k", load)
		errA <- err
	}()
	loadCtx := cx6recv(t, loadCtxCh)

	remValCh := make(chan int, 1)
	remErrCh := make(chan error, 1)
	go func() {
		v, err := c.GetOrLoad(context.Background(), "k", load)
		remValCh <- v
		remErrCh <- err
	}()
	time.Sleep(100 * time.Millisecond) // let the second caller join

	cancelA()
	if err := cx6grabErr(t, errA); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled caller error = %v, want context.Canceled", err)
	}
	select {
	case <-loadCtx.Done():
		t.Fatal("load context cancelled although one waiter remains")
	default:
	}

	close(release)
	if err := cx6grabErr(t, remErrCh); err != nil {
		t.Fatalf("remaining caller error = %v, want nil", err)
	}
	if v := cx6recv(t, remValCh); v != 55 {
		t.Fatalf("remaining caller value = %d, want 55", v)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("load called %d times, want a single shared load", got)
	}
}

// Rule 8: a load that ignores its ctx and returns a value anyway still
// delivers it to the waiters that remain after another caller gave up.
func TestRule8_LoadIgnoringCtxDeliversToRemainingWaiters(t *testing.T) {
	c := New[string, int](Options{TTL: time.Minute})

	release := make(chan struct{})
	loadCtxCh := make(chan context.Context, 1)
	load := func(ctx context.Context) (int, error) {
		loadCtxCh <- ctx // capture it, then deliberately ignore it
		<-release
		return 123, nil
	}

	ctxA, cancelA := context.WithCancel(context.Background())
	errA := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(ctxA, "k", load)
		errA <- err
	}()
	loadCtx := cx6recv(t, loadCtxCh)

	remValCh := make(chan int, 1)
	remErrCh := make(chan error, 1)
	go func() {
		v, err := c.GetOrLoad(context.Background(), "k", load)
		remValCh <- v
		remErrCh <- err
	}()
	time.Sleep(100 * time.Millisecond) // let the second caller join

	cancelA()
	if err := cx6grabErr(t, errA); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled caller error = %v, want context.Canceled", err)
	}
	select {
	case <-loadCtx.Done():
		t.Fatal("load context cancelled although one waiter remains")
	default:
	}

	// The load ignores its cancelled-never ctx and returns a value; the
	// surviving waiter must still get it.
	close(release)
	if err := cx6grabErr(t, remErrCh); err != nil {
		t.Fatalf("remaining caller error = %v, want nil", err)
	}
	if v := cx6recv(t, remValCh); v != 123 {
		t.Fatalf("remaining caller value = %d, want 123", v)
	}
	if got, ok := c.Get("k"); !ok || got != 123 {
		t.Fatalf("Get after the load = (%d, %v), want (123, true)", got, ok)
	}
}
