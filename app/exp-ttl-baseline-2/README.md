# exp-ttl-baseline-2

A generic in-memory TTL + LRU cache with singleflight loads. Model
experiment for issue #82 (baseline, copy 2) — not for merging.

Standard library only, Go 1.27.2, package `ttlcache` at the module root.

## API

```go
New[K comparable, V any](opts Options) *Cache[K, V]
(c) Get(key K) (V, bool)
(c) Set(key K, value V)
(c) Delete(key K)
(c) Len() int
(c) GetOrLoad(ctx context.Context, key K, load func(ctx context.Context) (V, error)) (V, error)
```

`Options`:

| field        | meaning                                                              |
|--------------|----------------------------------------------------------------------|
| `TTL`        | `> 0`: entry expires `TTL` after it was stored; `0`: never expires   |
| `MaxEntries` | `> 0`: at most this many live entries; `0`: unbounded                |
| `Now`        | the clock; `nil` means `time.Now` (inject a fake clock in tests)      |

## Behaviour

- **Expiry.** An entry stored at `t` is live while `Now() < t + TTL`. Expired
  entries are dropped lazily: `Get`/`GetOrLoad` never return one, `Len` never
  counts one. `Set` on an existing key replaces the value and restarts the TTL.
- **LRU eviction.** Storing more than `MaxEntries` live entries evicts the
  least recently used live one. A `Get` or `GetOrLoad` hit refreshes recency.
  Expired entries never cause a live entry to be evicted.
- **Singleflight.** Concurrent `GetOrLoad` callers for one key share a single
  `load` call; different keys load independently. A failed load caches
  nothing and every waiter gets that same error; the next `GetOrLoad` loads
  again.
- **Cancellation.** A caller whose `ctx` ends before the load finishes
  returns `ctx.Err()` promptly; the load keeps running for the remaining
  waiters. The context handed to `load` carries the first caller's values but
  not its cancellation: it is cancelled exactly when every waiter has given
  up (or the load has returned). A successful result is stored even if every
  caller gave up, with its TTL starting when `load` returned.
- **Delete** removes the key but does not stop an in-flight load for it; that
  load's result is still stored when it completes.

The cache is safe for concurrent use; no goroutine it starts outlives the
load it serves.

## Checks

```sh
gofmt -l .
go vet ./...
go build ./...
go test -race ./...
```
