# exp-ttl-deepseek-2

A small concurrent cache with per-entry TTL, LRU eviction and single-flight
loading. **This is a model experiment (issue #84), not for merging:** it exists
to measure what a model produces from one written spec, so it deliberately
stands alone — standard library only, no dependency on any other app in the
repository, and no consumers.

Module path `github.com/RichardoC/factory-repo/app/exp-ttl-deepseek-2`, Go
1.27.2, package `ttlcache` at the module root.

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

## Behaviour

- **Expiry.** An entry stored at `t` (by `Now()`) is live while `Now()` is
  before `t + TTL`. `Get` and `GetOrLoad` never return an expired entry; expired
  entries are removed as they are met, and `Len` counts only live entries.
- **Replacement.** `Set` on an existing key replaces the value and restarts the
  TTL.
- **LRU eviction.** With `MaxEntries > 0` the cache keeps at most that many live
  entries and evicts the least recently used one. Storing an entry and a hit on
  `Get`/`GetOrLoad` both count as use. An expired entry is dropped on the way
  out rather than displacing a live one.
- **One load per key.** Concurrent `GetOrLoad` misses on the same key share a
  single call of `load`; different keys load concurrently, without blocking one
  another. A live hit never calls `load`.
- **Errors.** A failed load caches nothing: every waiter gets that same error,
  and a later `GetOrLoad` calls `load` again.
- **Giving up.** A caller whose context is done before its load finishes returns
  `ctx.Err()` promptly; the load keeps running for the callers still waiting.
  The context handed to `load` is cancelled exactly when the **last** waiter has
  given up — never because one of several was cancelled.
- **Late results.** A load that succeeds still has its value stored, with the
  TTL starting when `load` returned, even if every caller had given up. `Delete`
  does not stop an in-flight load, and that load's result is stored too.
- **Concurrency.** Every method is safe for concurrent use, and the package is
  clean under `go test -race`. The cache starts at most one goroutine per load,
  and each ends with the load it serves, so nothing it starts outlives it.

`load` is assumed not to panic.

## Shape of the implementation

`cache.go` holds the map plus an intrusive doubly linked list of live entries,
most recently used at the head, so eviction is the tail and a hit is a splice.
`load.go` holds the per-key flight: at most one exists per key, it is created
under the cache mutex on the first miss and removed when the load returns. The
result fields are published by closing the flight's `done` channel, and the
waiter count carries its own mutex so a caller that gives up can decrement it
without holding, or waiting for, the cache mutex.

## Building and testing

```sh
cd app/exp-ttl-deepseek-2
gofmt -l .
go vet ./...
go build ./...
go test -race ./...
```

`cache_test.go` covers every rule above; expiry tests drive a hand-moved
`Now` so they do not depend on wall-clock timing, and the concurrency tests use
`-race` plus goroutine-count checks rather than sleeps to prove ordering.
