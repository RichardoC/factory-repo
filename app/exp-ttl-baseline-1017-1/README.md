# ttlcache

A generic in-memory cache with per-entry TTL expiry, LRU eviction and
single-flight loading. Standard library only; Go 1.27.

```go
c := ttlcache.New[string, *Thing](ttlcache.Options{
	TTL:        5 * time.Minute, // 0: entries never expire
	MaxEntries: 1024,            // 0: unbounded
	Now:        nil,             // nil: time.Now
})

c.Set("key", thing)
v, ok := c.Get("key")
v, err := c.GetOrLoad(ctx, "key", load) // load func(ctx) (*Thing, error)
c.Delete("key")
n := c.Len() // live entries only
```

## Semantics

- **Expiry.** An entry stored at `Now() == t` is live while `Now() < t + TTL`.
  `Get` and `GetOrLoad` never return an expired entry; `Len` counts only live
  entries.
- **Replacement.** `Set` on an existing key replaces the value and restarts
  its TTL.
- **LRU eviction.** Storing an entry beyond `MaxEntries` evicts the least
  recently used live entry. An entry counts as used when stored and when a
  `Get`/`GetOrLoad` hit returns it. Expired entries are removed instead and
  never cause a live entry to be evicted.
- **Single-flight loads.** `GetOrLoad` on a live hit never calls `load`. On a
  miss, concurrent callers for the same key share one call of `load`; at most
  one load per key runs at a time. Different keys load independently.
- **Errors.** A failed load caches nothing; every caller waiting on it gets
  the same error, and a later `GetOrLoad` calls `load` again.
- **Giving up.** A caller whose context is done returns `ctx.Err()` promptly
  while the load keeps running for remaining waiters. The context passed to
  `load` is cancelled exactly when every waiting caller has given up — it is
  not derived from any single caller's context.
- **Late results.** A value from a successful load is always cached, with its
  TTL starting when `load` returned, even if every caller had given up.
- **Delete.** Removes the key but does not stop an in-flight load: that
  load's result is still stored when it completes.

The cache is safe for concurrent use; every load runs in a goroutine that
ends when the load returns.
