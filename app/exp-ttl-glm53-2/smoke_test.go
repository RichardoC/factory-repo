package ttlcache

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type smokeClock struct{ t int64 }

func (c *smokeClock) Now() time.Time { return time.Unix(c.t, 0) }

func TestSmokeExpiryAndLRU(t *testing.T) {
	clk := &smokeClock{}
	c := New[string, int](Options{TTL: 10 * time.Second, MaxEntries: 2, Now: clk.Now})
	c.Set("a", 1)
	c.Set("b", 2)
	c.Get("a") // a most recent, b LRU
	c.Set("c", 3)
	if _, ok := c.Get("b"); ok {
		t.Fatal("b should have been evicted")
	}
	if c.Len() != 2 {
		t.Fatalf("Len = %d, want 2", c.Len())
	}
	c.Set("d", 4) // a is LRU live
	if _, ok := c.Get("a"); ok {
		t.Fatal("a (LRU) should have been evicted, not the expired")
	}
	// expiry: a entry stored at t=0 with TTL 10 expired by t=10
	clk.t = 10
	if _, ok := c.Get("c"); ok {
		t.Fatal("c should be expired")
	}
	if c.Len() != 0 {
		t.Fatalf("Len after expiry = %d, want 0", c.Len())
	}
}

func TestSmokeSingleflight(t *testing.T) {
	c := New[string, string](Options{TTL: time.Minute})
	var calls int32
	release := make(chan struct{})
	var wg sync.WaitGroup
	var results sync.Map
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			v, err := c.GetOrLoad(ctx, "k", func(ctx context.Context) (string, error) {
				atomic.AddInt32(&calls, 1)
				<-release
				return "v", nil
			})
			results.Store(v, err)
		}()
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("load ran %d times, want 1", calls)
	}
	if v, err := c.GetOrLoad(context.Background(), "k", func(context.Context) (string, error) { t.Fatal("load should not run"); return "", nil }); err != nil || v != "v" {
		t.Fatalf("hit = %q, %v", v, err)
	}
}

func TestSmokeGiveUp(t *testing.T) {
	c := New[string, int](Options{})
	giveUp, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	v, err := c.GetOrLoad(giveUp, "k", func(ctx context.Context) (int, error) {
		<-ctx.Done()
		return 42, nil
	})
	if err != context.Canceled {
		t.Fatalf("err = %v, want canceled", err)
	}
	_ = v
	time.Sleep(50 * time.Millisecond)
	if got, ok := c.Get("k"); !ok || got != 42 {
		t.Fatalf("late value not stored: %v %v", got, ok)
	}
}
