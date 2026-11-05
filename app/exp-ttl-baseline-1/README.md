# exp-ttl-baseline-1

`package ttlcache` — an in-memory, generic cache with per-entry TTL, LRU
eviction under a live-entry cap, and single-flight loading. Standard
library only. Experiment for issue #543 (baseline, copy 1); not for merging.

## Usage

```go
c := ttlcache.New[string, int](ttlcache.Options{
	TTL:        10 * time.Second, // 0 = never expire
	MaxEntries: 1000,             // 0 = unbounded
	Now:        nil,              // nil = time.Now; injectable for tests
})

c.Set("k", 1)
v, ok := c.Get("k")
c.Delete("k")
n := c.Len() // live entries only

v, err := c.GetOrLoad(ctx, "k", func(ctx context.Context) (int, error) {
	return loadFromSource(ctx, "k")
})
```

## Semantics

- An entry stored at time `Now()` is live while `Now()` is before
  `t + TTL`. `Get`, `GetOrLoad` and `Len` never see expired entries.
- `Set` on an existing key replaces the value and restarts its TTL.
- `MaxEntries` caps *live* entries. When a store would exceed the cap,
  expired entries are dropped first; if still over, the least recently
  used live entry is evicted. Storing and hit-reads (`Get`, `GetOrLoad`)
  both count as use.
- `GetOrLoad` is single-flight per key: concurrent callers for the same
  key share one `load` call; different keys load independently.
- If `load` errors, nothing is cached and every waiter gets that same
  error; a later `GetOrLoad` retries.
- A caller whose `ctx` is done returns `ctx.Err()` promptly without
  disturbing the load for remaining waiters. The context handed to
  `load` is derived from no single caller: it is cancelled exactly when
  every waiting caller has given up. A load that succeeds anyway is
  still cached, its TTL starting when `load` returned.
- `Delete` removes the key but does not stop an in-flight load; that
  load's successful result is still stored when it completes.

## Implementation notes

- One mutex guards the entry map and the per-key in-flight table. Loads
  run outside the lock in a goroutine the cache starts; it ends when
  `load` returns, so no cache goroutine outlives the load it serves.
- LRU order uses a logical clock stamp per entry rather than an
  intrusive list, which keeps expiry and eviction simple; finding the
  eviction victim is O(n) over the map and only happens when the cache
  is over its cap. Fine for the sizes this experiment targets.
- The load's context is `context.WithCancelCause(context.WithoutCancel(ctx))`
  of the first caller: it keeps that caller's values but none of its
  cancellation, and is cancelled (cause `context.Canceled`) when the
  waiter count drops to zero.

## Checks

```
gofmt -l .        # clean
go vet ./...      # clean
go build ./...    # clean
go test -race ./...   # clean
```
