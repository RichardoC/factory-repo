# exp-ttl-glm53-2 — TTL cache

A self-contained Go experiment (not for merging): a generic, concurrency-safe
in-memory cache with TTL expiry, LRU eviction, and single-flight loading.
Standard library only; module `github.com/RichardoC/factory-repo/app/exp-ttl-glm53-2`.

## API

```go
type Options struct {
    TTL        time.Duration    // > 0: entry expires TTL after it was stored; 0: never expires
    MaxEntries int              // > 0: at most this many live entries; 0: unbounded
    Now        func() time.Time // the clock; nil means time.Now
}

func New[K comparable, V any](opts Options) *Cache[K, V]
func (c *Cache[K, V]) Get(key K) (V, bool)
func (c *Cache[K, V]) Set(key K, value V)
func (c *Cache[K, V]) Delete(key K)
func (c *Cache[K, V]) Len() int // counts only live entries
func (c *Cache[K, V]) GetOrLoad(ctx context.Context, key K, load func(ctx context.Context) (V, error)) (V, error)
```

## Semantics

- **Expiry.** An entry stored at time `t` (by `Now()`) is live while `Now()`
  is before `t+TTL`. `Get`/`GetOrLoad` never return an expired entry; `Len`
  counts only live entries.
- **Replacement.** `Set` on an existing key replaces the value and restarts
  the TTL.
- **LRU eviction.** Storing a new entry when `MaxEntries` live entries exist
  evicts the least recently used live entry. A use is a store (`Set` or a
  completed load) or a `Get`/`GetOrLoad` hit. Expired entries never evict a
  live entry.
- **Single flight.** Concurrent `GetOrLoad` calls for the same key share one
  call of `load`; loads for different keys run independently.
- **Errors.** A failing `load` caches nothing; all waiters get that error; a
  later call loads again.
- **Giving up.** A caller whose `ctx` is done returns `ctx.Err()` promptly
  while the load continues for the others. The context passed to `load` is
  cancelled exactly when every waiter for that load has given up.
- **Late results.** A successful result is stored even if all callers gave
  up, TTL starting when `load` returned.
- **Delete.** Removes the key but never stops an in-flight load; the load's
  result is still stored when it completes.
- **Concurrency.** Safe for concurrent use; `go test -race` clean; the only
  goroutine the cache starts is the one running a load, which ends when the
  load returns.

## Usage

```go
c := ttlcache.New[string, []byte](ttlcache.Options{TTL: time.Minute, MaxEntries: 1024})
v, err := c.GetOrLoad(ctx, "key", func(ctx context.Context) ([]byte, error) {
    return fetch(ctx, "key")
})
```

## Checks

From this directory: `gofmt -l .`, `go vet ./...`, `go build ./...`,
`go test -race ./...`.
