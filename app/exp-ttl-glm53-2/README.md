# exp-ttl-glm53-2

A model experiment for issue #529 (`RichardoC/factory-repo`): a small, generic,
standard-library-only TTL + LRU cache with singleflight loading. Not for merging.

## Package `ttlcache`

```go
type Options struct {
    TTL        time.Duration    // > 0: entry expires TTL after it was stored; 0: never
    MaxEntries int              // > 0: at most this many live entries; 0: unbounded
    Now        func() time.Time // clock; nil means time.Now
}

func New[K comparable, V any](opts Options) *Cache[K, V]
func (c *Cache[K, V]) Get(key K) (V, bool)
func (c *Cache[K, V]) Set(key K, value V)
func (c *Cache[K, V]) Delete(key K)
func (c *Cache[K, V]) Len() int
func (c *Cache[K, V]) GetOrLoad(ctx context.Context, key K, load func(ctx context.Context) (V, error)) (V, error)
```

## Behaviour

- **Expiry.** An entry stored at `t` (by `Now()`) is live while `Now() < t + TTL`.
  `Get`/`GetOrLoad` never return an expired entry; `Len` counts only live entries.
- **Replacement.** `Set` on an existing key replaces the value and restarts the TTL.
- **LRU eviction.** Exceeding `MaxEntries` evicts the least recently used *live*
  entry; storing (by `Set` or a completed load) and a `Get`/`GetOrLoad` hit count as
  uses. Expired entries are cleaned up instead and never evict a live entry.
- **Singleflight.** Concurrent `GetOrLoad` misses for one key share a single `load`
  call; different keys load independently. A `load` error is shared by all waiters
  and nothing is cached; a caller whose `ctx` is done returns `ctx.Err()` while the
  load keeps running for the others. The context handed to `load` is cancelled
  exactly when its last waiter gives up, but a successful late result is still
  stored, with its TTL starting when `load` returned.
- **Delete.** Removes the key but does not stop an in-flight load, whose result is
  stored when it completes.
- **Concurrency.** Safe for concurrent use (`go test -race` clean); the cache
  starts no goroutine that outlives the loads it serves.

## Running

```
cd app/exp-ttl-glm53-2
gofmt -l . && go vet ./... && go build ./... && go test -race ./...
```
