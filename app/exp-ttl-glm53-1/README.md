# exp-ttl-glm53-1

A model experiment (not for merging): a small, concurrency-safe cache with
per-entry TTL expiry, LRU eviction under an optional entry cap, and
single-flight loading. Standard library only, Go 1.27.2.

```go
c := ttlcache.New[string, string](ttlcache.Options{
    TTL:        time.Minute,
    MaxEntries: 1000, // 0: unbounded
    Now:        nil,  // nil: time.Now
})

c.Set("k", "v")            // insert or replace; restarts the TTL
v, ok := c.Get("k")        // hit counts as a use; expired entries miss
n := c.Len()               // counts only live entries
c.Delete("k")

// On a miss, load runs once per key per moment in time: concurrent callers
// share the in-flight load. The context given to load is cancelled exactly
// when every waiter has given up; a caller whose own context ends returns
// ctx.Err() without waiting. A successful result is stored even if all
// callers gave up, with its TTL starting when load returned.
v, err := c.GetOrLoad(ctx, "k", func(ctx context.Context) (string, error) {
    return expensiveLookup(ctx)
})
```

Semantics:

- **Expiry.** An entry stored at `t` is live while `Now()` is before
  `t + TTL` (`TTL = 0` means never expire). `Get`, `GetOrLoad` and `Len`
  ignore/remove expired entries.
- **Replacement.** `Set` on an existing key replaces the value and restarts
  the TTL.
- **Eviction.** When a store would leave more than `MaxEntries` live
  entries, the least recently used *live* entry is evicted. Expired entries
  are purged first so they never cause a live eviction. Storing and hits
  count as uses.
- **Loading.** `GetOrLoad` hits return the cached value without calling
  `load`. Misses share at most one load per key; different keys never
  block each other. Load errors propagate to every waiter and nothing is
  cached; the next `GetOrLoad` tries again. A load error with a `nil` load
  function is reported, nothing stored.
- **Deletion.** `Delete` removes the entry. An in-flight load is not
  stopped; its result is stored when it completes.

Run `go test -race ./...` from this directory.
