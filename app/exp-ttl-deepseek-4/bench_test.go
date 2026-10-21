package ttlcache

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// benchKeys is the working set of the benchmarks: small enough to stay in the
// cache (TTL 0, unbounded) and to keep each run cheap.
const benchKeys = 1024

// BenchmarkGetBound reports the cost of a serial Get hit on a warm,
// int-keyed, never-expiring cache. Before/after evidence for changes to
// Cache.Get and the LRU touch it performs.
func BenchmarkGetBound(b *testing.B) {
	c := New[int, int](Options{TTL: 0})
	for i := 0; i < benchKeys; i++ {
		c.Set(i, i)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if v, ok := c.Get(i % benchKeys); !ok || v != i%benchKeys {
			b.Fatalf("Get(%d) = %d, %v; want %d, true", i%benchKeys, v, ok, i%benchKeys)
		}
	}
}

// BenchmarkGetParallel reports the same operation with NumCPU goroutines, so a
// change to the locking or the LRU list shows up as contention.
func BenchmarkGetParallel(b *testing.B) {
	c := New[int, int](Options{TTL: 0})
	for i := 0; i < benchKeys; i++ {
		c.Set(i, i)
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := i % benchKeys
			if v, ok := c.Get(key); !ok || v != key {
				b.Errorf("Get(%d) = %d, %v; want %d, true", key, v, ok, key)
				return
			}
			i++
		}
	})
}

// BenchmarkGetOrLoadHit reports the cost of GetOrLoad when every key is already
// live. The loader must never run here: if it does, the benchmark fails rather
// than silently measuring the load path.
func BenchmarkGetOrLoadHit(b *testing.B) {
	c := New[string, string](Options{TTL: time.Minute, Now: time.Now})
	for i := 0; i < benchKeys; i++ {
		c.Set(fmt.Sprintf("k%d", i), "v")
	}
	load := func(ctx context.Context) (string, error) {
		return "", fmt.Errorf("benchmark: loader called on a hit")
	}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := fmt.Sprintf("k%d", i%benchKeys)
		if _, err := c.GetOrLoad(ctx, key, load); err != nil {
			b.Fatalf("GetOrLoad(%q) = %v; want a hit", key, err)
		}
	}
}
