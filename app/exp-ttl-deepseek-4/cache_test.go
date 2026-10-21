package ttlcache

// Tests for the expiry/LRU engine in cache.go. Every time-dependent test uses a
// scripted clock passed as Options.Now: there are no sleeps and no dependence
// on the wall clock, so the expiry boundary is exercised exactly rather than
// approximately. The single-flight load path is covered by load_test.go; this
// file covers the engine's own rules (the issue's rules 1-3).

import (
	"slices"
	"sync"
	"testing"
	"time"
)

// epoch is a fixed origin for the scripted clock.
var epoch = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// scriptedClock is a deterministic clock. It is safe for concurrent use so it
// can also back caches exercised from several goroutines.
type scriptedClock struct {
	mu sync.Mutex
	t  time.Time
}

func newScriptedClock() *scriptedClock {
	return &scriptedClock{t: epoch}
}

// Now returns the clock's current time.
func (c *scriptedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Advance moves the clock forward by d.
func (c *scriptedClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// Set moves the clock to t.
func (c *scriptedClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// mustGet asserts that key is a live hit and returns its value.
func mustGet[V any](t *testing.T, c *Cache[string, V], key string) V {
	t.Helper()
	v, ok := c.Get(key)
	if !ok {
		t.Fatalf("Get(%q) = _, false; want a hit", key)
	}
	return v
}

// mustMiss asserts that key is a miss.
func mustMiss[V any](t *testing.T, c *Cache[string, V], key string) {
	t.Helper()
	if v, ok := c.Get(key); ok {
		t.Fatalf("Get(%q) = %v, true; want a miss", key, v)
	}
}

// liveKeys returns the keys of all live entries, sorted, for order-independent
// set comparisons.
func liveKeys[V any](t *testing.T, c *Cache[string, V]) []string {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	keys := make([]string, 0, len(c.entries))
	for k, e := range c.entries {
		if c.liveLocked(e, now) {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys
}

// TestNewDefaults covers New with a nil Now (the real clock): it must not
// panic, Len must work, and a Set/Get round trip must succeed. It also pins the
// documented defaults for TTL and MaxEntries.
func TestNewDefaults(t *testing.T) {
	c := New[string, int](Options{})
	if c == nil {
		t.Fatal("New returned nil")
	}
	if c.now == nil {
		t.Fatal("New left now nil; want time.Now")
	}
	if c.ttl != 0 {
		t.Fatalf("ttl = %v; want 0 (never expires)", c.ttl)
	}
	if c.maxEntries != 0 {
		t.Fatalf("maxEntries = %d; want 0 (unbounded)", c.maxEntries)
	}
	if c.entries == nil || c.refs == nil || c.calls == nil {
		t.Fatal("New left a map nil; want all three initialised")
	}
	if got := c.Len(); got != 0 {
		t.Fatalf("Len() on a fresh cache = %d; want 0", got)
	}

	c.Set("k", 7)
	if got := mustGet(t, c, "k"); got != 7 {
		t.Fatalf("Get(k) = %d; want 7", got)
	}
	if got := c.Len(); got != 1 {
		t.Fatalf("Len() = %d; want 1", got)
	}
}

// TestExpiryBoundary covers rule 1 for Get and for Len: an entry stored at t is
// live strictly before t+TTL, and already expired at exactly t+TTL.
func TestExpiryBoundary(t *testing.T) {
	clock := newScriptedClock()
	c := New[string, int](Options{TTL: time.Minute, Now: clock.Now})

	c.Set("k", 1)

	// One nanosecond before the boundary: still live, for Get and for Len.
	clock.Set(epoch.Add(time.Minute - time.Nanosecond))
	if got := mustGet(t, c, "k"); got != 1 {
		t.Fatalf("Get(k) = %d just before expiry; want 1", got)
	}
	if got := c.Len(); got != 1 {
		t.Fatalf("Len() = %d just before expiry; want 1", got)
	}

	// Exactly stored+TTL is expired: the live test is Now() < stored+TTL.
	clock.Set(epoch.Add(time.Minute))
	mustMiss(t, c, "k")
	if got := c.Len(); got != 0 {
		t.Fatalf("Len() = %d at the exact expiry instant; want 0", got)
	}
	// The expired entry was removed, not merely skipped.
	if _, ok := c.entries["k"]; ok {
		t.Fatal("Get left the expired entry in the map; want it removed")
	}
	if _, ok := c.refs["k"]; ok {
		t.Fatal("Get left the expired entry in the LRU index; want it removed")
	}
}

// TestExpiryReapsOnLenWithoutGet covers Len's obligation to reap: no Get
// happens between storing and expiry.
func TestExpiryReapsOnLenWithoutGet(t *testing.T) {
	clock := newScriptedClock()
	c := New[string, int](Options{TTL: time.Second, Now: clock.Now})
	c.Set("a", 1)
	c.Set("b", 2)
	clock.Advance(time.Second)
	if got := c.Len(); got != 0 {
		t.Fatalf("Len() = %d after expiry; want 0 (Len must reap)", got)
	}
	if len(c.entries) != 0 {
		t.Fatalf("entries left behind after Len: %d; want 0", len(c.entries))
	}
	if len(c.refs) != 0 {
		t.Fatalf("lru refs left behind after Len: %d; want 0", len(c.refs))
	}
}

// TestTTLZeroNeverExpires covers TTL == 0.
func TestTTLZeroNeverExpires(t *testing.T) {
	clock := newScriptedClock()
	c := New[string, int](Options{TTL: 0, Now: clock.Now})
	c.Set("k", 1)
	clock.Advance(1000 * time.Hour)
	if got := mustGet(t, c, "k"); got != 1 {
		t.Fatalf("Get(k) = %d after a long time with TTL 0; want 1", got)
	}
	if got := c.Len(); got != 1 {
		t.Fatalf("Len() = %d with TTL 0; want 1", got)
	}
}

// TestNegativeTTLNeverExpires pins the documented choice that only TTL > 0
// expires entries, so a negative TTL means "never expires".
func TestNegativeTTLNeverExpires(t *testing.T) {
	clock := newScriptedClock()
	c := New[string, int](Options{TTL: -time.Hour, Now: clock.Now})
	c.Set("k", 1)
	clock.Advance(10000 * time.Hour)
	if got := mustGet(t, c, "k"); got != 1 {
		t.Fatalf("Get(k) = %d with a negative TTL; want 1 (never expires)", got)
	}
	if got := c.Len(); got != 1 {
		t.Fatalf("Len() = %d with a negative TTL; want 1", got)
	}
}

// TestSetReplacesAndRestartsTTL covers rule 2: the value is replaced, the entry
// is still a single entry, and the TTL window restarts from the second store.
func TestSetReplacesAndRestartsTTL(t *testing.T) {
	clock := newScriptedClock()
	c := New[string, string](Options{TTL: time.Minute, Now: clock.Now})

	c.Set("k", "first")
	clock.Advance(40 * time.Second)
	c.Set("k", "second")
	if got := c.Len(); got != 1 {
		t.Fatalf("Len() = %d after replacing; want 1", got)
	}

	// The original window ended at epoch+60s; the restart moved it to
	// epoch+100s, so the entry is still live at epoch+70s.
	clock.Set(epoch.Add(70 * time.Second))
	if got := mustGet(t, c, "k"); got != "second" {
		t.Fatalf("Get(k) = %q after TTL restart; want %q", got, "second")
	}

	clock.Set(epoch.Add(100 * time.Second))
	mustMiss(t, c, "k")
}

// TestLRUGetIsAUse covers rule 3(a): with MaxEntries=2 a Get hit counts as a
// use, so the untouched key is the victim.
func TestLRUGetIsAUse(t *testing.T) {
	clock := newScriptedClock()
	c := New[string, int](Options{MaxEntries: 2, Now: clock.Now})

	c.Set("A", 1)
	c.Set("B", 2)
	mustGet(t, c, "A") // A becomes most recently used; B is the victim
	c.Set("C", 3)

	if got := c.Len(); got != 2 {
		t.Fatalf("Len() = %d; want 2", got)
	}
	if got := mustGet(t, c, "A"); got != 1 {
		t.Fatalf("Get(A) = %d; want 1 (A was used)", got)
	}
	mustMiss(t, c, "B")
	if got := mustGet(t, c, "C"); got != 3 {
		t.Fatalf("Get(C) = %d; want 3", got)
	}
}

// TestLRUSetIsAUse covers rule 3(b): a store counts as a use, so re-storing A
// makes B the victim.
func TestLRUSetIsAUse(t *testing.T) {
	clock := newScriptedClock()
	c := New[string, int](Options{MaxEntries: 2, Now: clock.Now})

	c.Set("A", 1)
	c.Set("B", 2)
	c.Set("A", 10) // storing A is a use; B is now the victim
	c.Set("C", 3)

	if got := c.Len(); got != 2 {
		t.Fatalf("Len() = %d; want 2", got)
	}
	if got := mustGet(t, c, "A"); got != 10 {
		t.Fatalf("Get(A) = %d; want 10 (replaced value)", got)
	}
	mustMiss(t, c, "B")
	if got := mustGet(t, c, "C"); got != 3 {
		t.Fatalf("Get(C) = %d; want 3", got)
	}
}

// TestLRUExpiredNeverEvictsLive covers rule 3(c): with MaxEntries=2, MaxEntries
// is not exceeded by a live entry, and an expired entry is never kept at the
// cost of a live one. A is stored first and is dead by the time C arrives; B,
// stored later and still live, must survive.
func TestLRUExpiredNeverEvictsLive(t *testing.T) {
	clock := newScriptedClock()
	c := New[string, int](Options{TTL: time.Minute, MaxEntries: 2, Now: clock.Now})

	c.Set("A", 1) // stored at epoch+0s, expires at epoch+60s
	clock.Advance(30 * time.Second)
	c.Set("B", 2) // stored at epoch+30s, expires at epoch+90s

	clock.Advance(31 * time.Second) // epoch+61s: A is expired, B is live
	c.Set("C", 3)

	if got := c.Len(); got != 2 {
		t.Fatalf("Len() = %d; want 2 (A dropped as expired, B and C live)", got)
	}
	if got := mustGet(t, c, "B"); got != 2 {
		t.Fatalf("Get(B) = %d; want 2 (the live B must not be evicted while A is expired)", got)
	}
	if got := mustGet(t, c, "C"); got != 3 {
		t.Fatalf("Get(C) = %d; want 3", got)
	}
	mustMiss(t, c, "A")
}

// TestLRUExpiredReapedBeforeTrim pins the ordering inside evictLocked: expiry
// is reaped before the trim, so the trim only ever removes a live entry when
// the live count genuinely exceeds MaxEntries, and it removes the least
// recently used live one.
func TestLRUExpiredReapedBeforeTrim(t *testing.T) {
	clock := newScriptedClock()
	c := New[string, int](Options{TTL: time.Minute, MaxEntries: 2, Now: clock.Now})

	c.Set("A", 1) // expires epoch+60s
	clock.Advance(10 * time.Second)
	c.Set("B", 2) // expires epoch+70s
	c.Set("C", 3) // expires epoch+70s; LRU order (front .. back) = C, B, A

	clock.Advance(51 * time.Second) // epoch+61s: A expired, B and C live
	c.Set("D", 4)
	// Reaping A is not enough for MaxEntries=2: three live entries want two
	// slots, so the least recently used live entry (B) goes and C stays.
	keys := liveKeys(t, c)
	if !slices.Equal(keys, []string{"C", "D"}) {
		t.Fatalf("live keys = %v; want [C D] (expired A reaped, LRU live B evicted, C kept)", keys)
	}
	if got := c.Len(); got != 2 {
		t.Fatalf("Len() = %d; want 2", got)
	}
}

// TestLRUMaxEntriesOne covers rule 3(d): eviction at MaxEntries=1, including a
// Get hit protecting the survivor.
func TestLRUMaxEntriesOne(t *testing.T) {
	clock := newScriptedClock()
	c := New[string, int](Options{MaxEntries: 1, Now: clock.Now})

	c.Set("A", 1)
	c.Set("B", 2)
	if got := c.Len(); got != 1 {
		t.Fatalf("Len() = %d; want 1", got)
	}
	mustMiss(t, c, "A")
	if got := mustGet(t, c, "B"); got != 2 {
		t.Fatalf("Get(B) = %d; want 2", got)
	}

	// The Get hit made B most recently used, so B survives the next store.
	c.Set("C", 3)
	mustMiss(t, c, "B")
	if got := mustGet(t, c, "C"); got != 3 {
		t.Fatalf("Get(C) = %d; want 3", got)
	}
}

// TestUnbounded covers rule 3(e): MaxEntries == 0 keeps everything.
func TestUnbounded(t *testing.T) {
	clock := newScriptedClock()
	c := New[string, int](Options{MaxEntries: 0, Now: clock.Now})

	const n = 100
	for i := range n {
		c.Set(string(rune('a'+i%26))+string(rune('a'+(i/26)%26)), i)
	}
	want := len(c.entries)
	if want == 0 {
		t.Fatal("unbounded cache stored nothing")
	}
	if got := c.Len(); got != want {
		t.Fatalf("Len() = %d; want %d (MaxEntries 0 is unbounded)", got, want)
	}
	if _, ok := c.Get("aa"); !ok {
		t.Fatal("Get(aa) missed in an unbounded cache with entries present")
	}
}

// TestMaxEntriesNegativeIsUnbounded pins the documented choice that a negative
// MaxEntries is treated like 0.
func TestMaxEntriesNegativeIsUnbounded(t *testing.T) {
	clock := newScriptedClock()
	c := New[string, int](Options{MaxEntries: -5, Now: clock.Now})
	for _, k := range []string{"A", "B", "C"} {
		c.Set(k, len(k))
	}
	if got := c.Len(); got != 3 {
		t.Fatalf("Len() = %d; want 3 (a negative MaxEntries is unbounded)", got)
	}
}

// TestDelete covers Delete: the key misses afterwards, Len drops, the LRU index
// is cleaned up, and deleting an absent key (or deleting twice) is a no-op.
func TestDelete(t *testing.T) {
	clock := newScriptedClock()
	c := New[string, int](Options{TTL: time.Minute, Now: clock.Now})

	c.Set("k", 1)
	c.Set("other", 2)
	c.Delete("k")

	mustMiss(t, c, "k")
	if got := c.Len(); got != 1 {
		t.Fatalf("Len() = %d after Delete; want 1", got)
	}
	if _, ok := c.refs["k"]; ok {
		t.Fatal("Delete left an lru element behind")
	}
	if got := mustGet(t, c, "other"); got != 2 {
		t.Fatalf("Get(other) = %d; want 2", got)
	}

	// Deleting an absent key, and deleting twice, are no-ops.
	c.Delete("nope")
	c.Delete("k")
	if got := c.Len(); got != 1 {
		t.Fatalf("Len() = %d after no-op deletes; want 1", got)
	}
	// A deleted key can be stored again as a fresh live entry.
	c.Set("k", 9)
	if got := mustGet(t, c, "k"); got != 9 {
		t.Fatalf("Get(k) = %d after re-storing; want 9", got)
	}
}

// TestLenEmpty covers Len on a cache that never held anything and on one that
// was emptied.
func TestLenEmpty(t *testing.T) {
	clock := newScriptedClock()
	c := New[string, int](Options{TTL: time.Minute, MaxEntries: 3, Now: clock.Now})
	if got := c.Len(); got != 0 {
		t.Fatalf("Len() = %d on an empty cache; want 0", got)
	}
	c.Set("k", 1)
	c.Delete("k")
	c.Delete("absent")
	if got := c.Len(); got != 0 {
		t.Fatalf("Len() = %d after emptying the cache; want 0", got)
	}
}
