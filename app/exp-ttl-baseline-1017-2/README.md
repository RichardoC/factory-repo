# ttlcache

An in-memory generic cache with per-entry TTL, LRU eviction, and
single-flight `GetOrLoad`. Experiment for issue #274 (model experiment,
baseline-1017 copy 2) — not for merging. Go 1.27.2, standard library only.

## Usage

```go
c := ttlcache.New[string, int](ttlcache.Options{
	TTL:        5 * time.Minute, // 0: entries never expire
	MaxEntries: 1000,            // 0: unbounded
	Now:        nil,             // nil: time.Now
})

c.Set("key", 42)
v, ok := c.Get("key") // hit refreshes LRU recency
c.Delete("key")
n := c.Len()          // counts only live (unexpired) entries

v, err := c.GetOrLoad(ctx, "key", func(ctx context.Context) (int, error) {
	return loadFromBackend(ctx, "key")
})
```

## Semantics

- **Expiry** — an entry stored at time `t` is live while `Now() < t + TTL`.
  `Get`, `GetOrLoad` never return an expired entry; `Len` counts only live
  entries (expired entries are removed lazily).
- **Replacement** — `Set` on an existing key replaces the value and restarts
  its TTL.
- **LRU eviction** — when storing an entry would leave more than
  `MaxEntries` live entries, the least recently used live entry is evicted.
  Storing and a `Get`/`GetOrLoad` hit both count as use. Expired entries are
  purged first, so they never evict a live entry.
- **Single-flight loads** — concurrent `GetOrLoad` callers for the same key
  share one `load` call; different keys load independently. A load error is
  not cached and reaches every waiter; the next call loads again.
- **Cancellation** — a caller whose context is done before the load finishes
  returns `ctx.Err()` promptly; the load keeps running for remaining
  waiters. The context passed to `load` is cancelled only when every waiting
  caller has given up. A successful result is stored even if every caller
  gave up, with its TTL starting when `load` returned.
- **Delete** — removes the key but does not stop an in-flight load; that
  load's result is still stored when it completes.
- **Concurrency** — safe for concurrent use; the only goroutine the cache
  starts runs exactly one load and exits when it returns.

## Development

```sh
gofmt -l .
go vet ./...
go build ./...
go test -race ./...
```
