package ttlcache

import (
	"context"
	"testing"
	"time"
)

// tb1Base is the fixed epoch all tb1 tests start their fake clock at.
func tb1Base() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

// tb1Clock is an injectable clock for expiry tests: no sleeping, exact
// boundaries.
type tb1Clock struct{ now time.Time }

func (c *tb1Clock) Now() time.Time { return c.now }

// tb1Advance moves the clock forward by d and returns the new time.
func (c *tb1Clock) tb1Advance(d time.Duration) time.Time {
	c.now = c.now.Add(d)
	return c.now
}

// tb1NoLoad is a load function a hit must never call: it fails the test
// that accidentally invokes it.
func tb1NoLoad(t *testing.T) func(context.Context) (int, error) {
	return func(context.Context) (int, error) {
		t.Helper()
		t.Fatal("load must not be called")
		return 0, nil
	}
}

// Rule 1: an entry stored at time t is live while Now() is before t+TTL,
// dead at t+TTL; Get/GetOrLoad never return an expired entry; Len counts
// only live entries.
func TestRule1_ExpiryBoundaryExact(t *testing.T) {
	ttl := 10 * time.Second
	clk := &tb1Clock{now: tb1Base()}
	c := New[string, int](Options{TTL: ttl, Now: clk.Now})
	c.Set("k", 42)

	// One nanosecond before the boundary: still live for Get, GetOrLoad
	// (no load) and Len.
	clk.tb1Advance(ttl - time.Nanosecond)
	if v, ok := c.Get("k"); !ok || v != 42 {
		t.Fatalf("just before t+TTL: Get = %d, %v; want 42, true", v, ok)
	}
	loads := 0
	if v, err := c.GetOrLoad(context.Background(), "k", func(context.Context) (int, error) {
		loads++
		return -1, nil
	}); err != nil || v != 42 || loads != 0 {
		t.Fatalf("just before t+TTL: GetOrLoad = %d, %v (loads=%d); want 42, nil, 0 loads", v, err, loads)
	}
	if c.Len() != 1 {
		t.Fatalf("just before t+TTL: Len = %d, want 1", c.Len())
	}

	// Exactly at t+TTL: dead. Get misses, GetOrLoad must not return the
	// stale value and must call load, Len drops to 0.
	clk.tb1Advance(time.Nanosecond)
	if _, ok := c.Get("k"); ok {
		t.Fatal("at t+TTL exactly: Get must miss")
	}
	if c.Len() != 0 {
		t.Fatalf("at t+TTL: Len = %d, want 0", c.Len())
	}
	v, err := c.GetOrLoad(context.Background(), "k", func(context.Context) (int, error) {
		loads++
		return 7, nil
	})
	if err != nil || v != 7 || loads != 1 {
		t.Fatalf("at t+TTL: GetOrLoad = %d, %v (loads=%d); want fresh 7, nil, exactly 1 load", v, err, loads)
	}
}

// Rule 1, TTL=0: entries never expire, however far the clock moves.
func TestRule1_TTLZeroNeverExpires(t *testing.T) {
	clk := &tb1Clock{now: tb1Base()}
	c := New[string, int](Options{TTL: 0, Now: clk.Now})
	c.Set("k", 42)

	clk.tb1Advance(365 * 24 * time.Hour)
	if v, ok := c.Get("k"); !ok || v != 42 {
		t.Fatalf("TTL=0 after a year: Get = %d, %v; want 42, true", v, ok)
	}
	if v, err := c.GetOrLoad(context.Background(), "k", tb1NoLoad(t)); err != nil || v != 42 {
		t.Fatalf("TTL=0 hit: GetOrLoad = %d, %v; want 42, nil", v, err)
	}
	if c.Len() != 1 {
		t.Fatalf("TTL=0: Len = %d, want 1", c.Len())
	}
}

// Rule 2: Set on an existing key replaces the value and restarts its TTL.
func TestRule2_SetReplacesValue(t *testing.T) {
	clk := &tb1Clock{now: tb1Base()}
	c := New[string, int](Options{TTL: time.Minute, MaxEntries: 5, Now: clk.Now})
	c.Set("k", 1)
	clk.tb1Advance(10 * time.Second)
	c.Set("k", 2)
	if c.Len() != 1 {
		t.Fatalf("replace changed Len: got %d, want 1", c.Len())
	}
	if v, ok := c.Get("k"); !ok || v != 2 {
		t.Fatalf("after Set: Get = %d, %v; want 2, true", v, ok)
	}
	if v, err := c.GetOrLoad(context.Background(), "k", tb1NoLoad(t)); err != nil || v != 2 {
		t.Fatalf("after Set: GetOrLoad = %d, %v; want 2, nil", v, err)
	}
}

// Rule 2: the second Set's TTL wins: the first store's expiry must be
// forgotten.
func TestRule2_SetRestartsTTL(t *testing.T) {
	ttl := 10 * time.Second
	clk := &tb1Clock{now: tb1Base()}
	c := New[string, int](Options{TTL: ttl, Now: clk.Now})

	t0 := clk.now
	c.Set("k", 1)
	clk.tb1Advance(8 * time.Second) // live for 2 more seconds under the old TTL
	c.Set("k", 2)                   // stored here; expires at this time + ttl

	// t0+16s: past the first TTL (t0+10) but before the restarted one
	// (t0+18). Must be live with the new value.
	clk.tb1Advance(8 * time.Second)
	if clk.now.Equal(t0.Add(18 * time.Second)) {
		t.Fatal("clock setup drifted")
	}
	if v, ok := c.Get("k"); !ok || v != 2 {
		t.Fatalf("old TTL applied: Get = %d, %v at %v past first TTL; want 2, true (restarted TTL)", v, ok, clk.now.Sub(t0))
	}

	// Exactly at the restarted expiry (t0+18): dead.
	clk.tb1Advance(2 * time.Second)
	if _, ok := c.Get("k"); ok {
		t.Fatalf("still live at exactly restarted expiry %v", clk.now.Sub(t0))
	}
	if c.Len() != 0 {
		t.Fatalf("Len = %d after restarted expiry, want 0", c.Len())
	}
}

// Rule 3, MaxEntries=0: unbounded, nothing is ever evicted.
func TestRule3_MaxEntriesZeroUnbounded(t *testing.T) {
	clk := &tb1Clock{now: tb1Base()}
	c := New[int, int](Options{MaxEntries: 0, Now: clk.Now})
	for i := 0; i < 1000; i++ {
		c.Set(i, i)
	}
	if c.Len() != 1000 {
		t.Fatalf("Len = %d, want 1000 (MaxEntries=0 is unbounded)", c.Len())
	}
	for i := 0; i < 1000; i += 100 {
		if v, ok := c.Get(i); !ok || v != i {
			t.Fatalf("Get(%d) = %d, %v; want %d, true", i, v, ok, i)
		}
	}
}

// Rule 3: storing past MaxEntries evicts the least recently used live
// entry; a Get hit changes recency and saves the hit key instead.
func TestRule3_EvictsLRUAndGetChangesRecency(t *testing.T) {
	clk := &tb1Clock{now: tb1Base()}
	c := New[string, int](Options{MaxEntries: 2, Now: clk.Now}) // TTL 0: never expires

	c.Set("a", 1)
	c.Set("b", 2) // LRU order: b, a
	c.Get("a")    // a most recent; b is now LRU
	c.Set("c", 3) // over the limit: b must go
	if _, ok := c.Get("b"); ok {
		t.Fatal("LRU entry b should have been evicted, not a")
	}
	if v, ok := c.Get("a"); !ok || v != 1 {
		t.Fatalf("a was evicted despite the Get hit: Get = %d, %v", v, ok)
	}
	if v, ok := c.Get("c"); !ok || v != 3 {
		t.Fatalf("Get(c) = %d, %v; want 3, true", v, ok)
	}
	if c.Len() != 2 {
		t.Fatalf("Len = %d, want 2", c.Len())
	}

	// From order c (LRU), a: a plain GetOrLoad hit on c makes it most
	// recent, so the next store evicts a, not c.
	if v, err := c.GetOrLoad(context.Background(), "c", tb1NoLoad(t)); err != nil || v != 3 {
		t.Fatalf("GetOrLoad hit on c = %d, %v; want 3, nil", v, err)
	}
	c.Set("d", 4)
	if _, ok := c.Get("a"); ok {
		t.Fatal("a (now LRU) should have been evicted by Set(d)")
	}
	if v, ok := c.Get("c"); !ok || v != 3 {
		t.Fatalf("c evicted though the GetOrLoad hit made it MRU: %d, %v", v, ok)
	}
}

// Rule 3: an entry stored by a completed load counts as a use and is
// subject to the same eviction; a store and a completed load both start
// at most-recent.
func TestRule3_LoadedEntryCountsAsUse(t *testing.T) {
	clk := &tb1Clock{now: tb1Base()}
	c := New[string, int](Options{MaxEntries: 1, Now: clk.Now})

	c.Set("a", 1) // the single slot
	if v, err := c.GetOrLoad(context.Background(), "b", func(context.Context) (int, error) {
		return 2, nil
	}); err != nil || v != 2 {
		t.Fatalf("GetOrLoad(b) = %d, %v; want 2, nil", v, err)
	}
	if _, ok := c.Get("a"); ok {
		t.Fatal("storing b via load should have evicted the single older entry a")
	}
	if v, ok := c.Get("b"); !ok || v != 2 {
		t.Fatalf("Get(b) = %d, %v; want 2, true", v, ok)
	}
	if c.Len() != 1 {
		t.Fatalf("Len = %d, want 1", c.Len())
	}

	// The loaded entry is MRU: the next Set evicts it, not something older.
	c.Set("c", 3)
	if _, ok := c.Get("b"); ok {
		t.Fatal("b should have been evicted by Set(c)")
	}
	if c.Len() != 1 {
		t.Fatalf("Len = %d, want 1", c.Len())
	}
}

// Rule 3: an expired entry still sitting in the cache must never cause a
// live entry to be evicted when a new store happens.
func TestRule3_ExpiredEntryNeverEvictsLive(t *testing.T) {
	ttl := 10 * time.Second
	clk := &tb1Clock{now: tb1Base()}
	c := New[string, int](Options{TTL: ttl, MaxEntries: 2, Now: clk.Now})

	c.Set("old", 1) // expires at t0+ttl
	clk.tb1Advance(6 * time.Second)
	c.Set("live", 2) // expires at t0+6+ttl; also makes "old" LRU-adjacent
	// "live" is still live here; "old" expires.
	clk.tb1Advance(5 * time.Second) // now t0+11: "old" expired, "live" live

	c.Set("new", 3) // naive LRU eviction would look at "old" first, and might
	// count it toward the limit and drop "live" instead. It must not: only
	// live entries count, and no live entry may be evicted for "old"'s sake
	// unless the count really is over the limit -- with "old" purged the
	// count is 2 ("live", "new"), so nothing live goes.
	if _, ok := c.Get("old"); ok {
		t.Fatal("expired old still returned")
	}
	if v, ok := c.Get("live"); !ok || v != 2 {
		t.Fatalf("live entry evicted because of an expired one: Get(live) = %d, %v", v, ok)
	}
	if v, ok := c.Get("new"); !ok || v != 3 {
		t.Fatalf("Get(new) = %d, %v; want 3, true", v, ok)
	}
	if c.Len() != 2 {
		t.Fatalf("Len = %d, want 2 live entries", c.Len())
	}

	// And a fourth store does evict the true LRU live entry, which is
	// "live" (stored and never hit since "new").
	c.Set("x", 4)
	if _, ok := c.Get("live"); ok {
		t.Fatal("true LRU live entry live should have been evicted by Set(x)")
	}
	if c.Len() != 2 {
		t.Fatalf("Len = %d, want 2", c.Len())
	}
}

// Rule 4: GetOrLoad on a live hit returns the cached value without
// calling load.
func TestRule4_HitSkipsLoad(t *testing.T) {
	clk := &tb1Clock{now: tb1Base()}
	c := New[string, int](Options{TTL: time.Minute, Now: clk.Now})
	c.Set("k", 42)

	calls := 0
	v, err := c.GetOrLoad(context.Background(), "k", func(context.Context) (int, error) {
		calls++
		return 99, nil
	})
	if calls != 0 {
		t.Fatalf("load ran %d times on a live hit, want 0", calls)
	}
	if err != nil || v != 42 {
		t.Fatalf("hit = %d, %v; want the cached 42, nil", v, err)
	}

	// A Set-replaced value is equally a live hit for GetOrLoad.
	c.Set("k", 43)
	v, err = c.GetOrLoad(context.Background(), "k", func(context.Context) (int, error) {
		calls++
		return 99, nil
	})
	if calls != 0 {
		t.Fatalf("load ran %d times on a hit after Set, want 0", calls)
	}
	if err != nil || v != 43 {
		t.Fatalf("hit after Set = %d, %v; want 43, nil", v, err)
	}

	// A value that arrived via GetOrLoad stays a hit: no second load.
	v, err = c.GetOrLoad(context.Background(), "j", func(context.Context) (int, error) {
		calls++
		return 7, nil
	})
	if err != nil || v != 7 || calls != 1 {
		t.Fatalf("load of j = %d, %v, calls=%d; want 7, nil, 1", v, err, calls)
	}
	v, err = c.GetOrLoad(context.Background(), "j", func(context.Context) (int, error) {
		calls++
		return 99, nil
	})
	if calls != 1 || err != nil || v != 7 {
		t.Fatalf("second GetOrLoad on j = %d, %v, calls=%d; want 7, nil, still 1 load", v, err, calls)
	}
}
