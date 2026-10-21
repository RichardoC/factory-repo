# ttlcache

A generic in-memory TTL + LRU cache with single-flight `GetOrLoad`, standard
library only.

```go
c := ttlcache.New[string, int](ttlcache.Options{
    TTL:        5 * time.Minute, // 0: entries never expire
    MaxEntries: 1000,            // 0: unbounded
    Now:        nil,             // nil: time.Now
})

c.Set("k", 42)
v, ok := c.Get("k")       // hit; marks the entry recently used
v, err := c.GetOrLoad(ctx, "k", func(ctx context.Context) (int, error) {
    return compute(ctx, "k")
})
c.Delete("k")
n := c.Len()              // live entries only
```

## Behaviour

- **Expiry** — an entry stored at `t` is live while `Now()` is before
  `t + TTL`. `Get`, `GetOrLoad` and `Len` never see expired entries.
- **Replacement** — `Set` on an existing key replaces the value and restarts
  its TTL.
- **LRU eviction** — with `MaxEntries > 0`, storing beyond the cap drops
  expired entries first, then evicts the least recently used live entry.
  Expired entries never cause a live entry to be evicted.
- **Single-flight `GetOrLoad`** — on a miss, concurrent callers for the same
  key share one `load` call; different keys load independently. A load error
  is returned to every waiter and nothing is cached; the next call retries.
- **Giving up** — a caller whose context is done before the load finishes
  returns `ctx.Err()` promptly. The load keeps running for remaining waiters,
  and the context handed to `load` is cancelled exactly when the last waiter
  gives up (or the load finishes). A successful value is always stored — even
  if every caller gave up — with its TTL starting when `load` returned.
- **`Delete`** — removes the key but does not stop an in-flight load; that
  load's result is still stored when it completes.
- **Concurrency** — safe for concurrent use; `go test -race` is clean and no
  goroutine the cache starts outlives the load it serves.

`load` is assumed not to panic.

## Checks

```
gofmt -l .
go vet ./...
go build ./...
go test -race ./...
```
