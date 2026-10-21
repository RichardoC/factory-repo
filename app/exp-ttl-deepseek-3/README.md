# ttlcache (model experiment)

A small generic, in-memory TTL cache with single-flight loading
(`GetOrLoad`).

**This is an experiment, not for merging.** It was written for a CodeAF model
experiment (issue #267) to compare how different models implement the same
specified package. It is not intended to be a dependency of any application in
this repository and is not published as one.

## Requirements

- Standard library only: no module dependencies, no `go.sum`.
- Go 1.27.2 (`go 1.27.2` in `go.mod`).

## API

```go
type Options struct {
	TTL        time.Duration    // > 0: an entry expires TTL after it was stored; 0: entries never expire
	MaxEntries int              // > 0: at most this many live entries; 0: unbounded
	Now        func() time.Time // the clock; nil means time.Now
}

type Cache[K comparable, V any] struct{ /* unexported fields */ }

func New[K comparable, V any](opts Options) *Cache[K, V]
func (c *Cache[K, V]) Get(key K) (V, bool)
func (c *Cache[K, V]) Set(key K, value V)
func (c *Cache[K, V]) Delete(key K)
func (c *Cache[K, V]) Len() int
func (c *Cache[K, V]) GetOrLoad(ctx context.Context, key K, load func(ctx context.Context) (V, error)) (V, error)
```

## Options

- `TTL` — `> 0`: an entry expires `TTL` after it was stored; `0`: entries never
  expire.
- `MaxEntries` — `> 0`: at most this many live entries; `0`: unbounded.
- `Now` — the clock; `nil` means `time.Now`.

## Behaviour

The package guarantees the following:

1. **Expiry.** An entry stored at time `t` (by `Now()`) is live while `Now()`
   is before `t + TTL`. `Get` and `GetOrLoad` never return an expired entry,
   and `Len` counts only live entries.
2. **Replacement.** `Set` on an existing key replaces the value and restarts
   its TTL.
3. **LRU eviction.** When storing an entry would leave more than `MaxEntries`
   live entries, the least recently used live entry is evicted. An entry counts
   as used when it is stored (by `Set` or a completed load) and when a `Get` or
   `GetOrLoad` hit returns it. Expired entries never cause a live entry to be
   evicted.
4. **Hits.** `GetOrLoad` on a live hit returns the cached value without calling
   `load`.
5. **One load per key.** On a miss, concurrent `GetOrLoad` callers for the same
   key share a single call of `load`: at most one load per key runs at a time.
   Different keys load independently, without blocking each other.
6. **Load errors.** If `load` returns an error, nothing is cached, and every
   caller waiting on that load returns that same error. A later `GetOrLoad`
   calls `load` again.
7. **Callers that give up.** If a caller's `ctx` is done before the load
   finishes, that caller returns `ctx.Err()` promptly. The load keeps running
   for any other waiting callers.
8. **The load's context.** The context passed to `load` is cancelled exactly
   when every caller waiting on that load has given up. It is not cancelled
   while at least one waiter remains, and never just because one of several
   callers was cancelled.
9. **Late results.** If `load` returns a value successfully, the value is
   stored, with its TTL starting when `load` returned, even if every caller has
   given up.
10. **Delete.** `Delete` removes the key. It doesn't stop an in-flight load:
    that load's result is still stored when it completes.
11. **Concurrency.** The cache is safe for concurrent use. `go test -race` is
    clean, and no goroutine the cache starts outlives the loads it serves.
12. **Panics.** `load` does not panic; the package does not handle that case.

## Checks

Run from this module directory (`app/exp-ttl-deepseek-3`):

```sh
gofmt -l .
go vet ./...
go build ./...
go test -race ./...
```

Each is expected to be clean: `gofmt -l .` prints nothing, and the other three
exit 0.
