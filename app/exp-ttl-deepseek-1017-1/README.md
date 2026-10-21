# ttlcache

A small, concurrency-safe cache with per-entry TTLs, LRU eviction and
single-flight loads. Standard library only.

This module is a CodeAF model experiment (issue #268): a TTL cache written from
scratch. It is a fixture and is not for merging.

```go
c := ttlcache.New[string, []byte](ttlcache.Options{
	TTL:        time.Minute, // 0: entries never expire
	MaxEntries: 1024,        // 0: unbounded
	Now:        nil,         // nil: time.Now
})

v, err := c.GetOrLoad(ctx, "key", func(ctx context.Context) ([]byte, error) {
	return fetch(ctx)
})
```

## Behaviour

- **Expiry** is lazy and checked against `Now()`: an entry stored at `t` is live
  while `Now()` is before `t+TTL`. `Get`, `GetOrLoad` and `Len` ignore expired
  entries. `Set` on an existing key replaces the value and restarts its TTL.
- **LRU eviction** keeps at most `MaxEntries` live entries: expired entries are
  swept first, then the least recently used live entry is evicted. Storing,
  a completed load, and a `Get`/`GetOrLoad` hit all count as use.
- **Single flight**: concurrent `GetOrLoad` misses for one key share a single
  `load`; different keys load independently. `load` runs in its own goroutine,
  so a caller whose `ctx` is done returns `ctx.Err()` promptly while the load
  continues for the others.
  - The context passed to `load` is cancelled only when every waiting caller has
    given up (or the load has returned); one cancelled caller never cancels it.
  - A successful late result is still stored, with its TTL starting when `load`
    returned, even if nobody is waiting by then.
- **Errors** from `load` are returned to every waiter and nothing is cached; the
  next `GetOrLoad` retries. `Delete` does not stop an in-flight load.

The cache starts at most one goroutine per in-flight load and that goroutine
ends when `load` returns, so no goroutine outlives the load it serves.
