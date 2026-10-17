# ttlcache

A small, self-contained, concurrency-safe in-memory cache with per-entry TTL
expiry, optional least-recently-used (LRU) eviction and single-flight loading.

This module is a **model experiment** and is not intended to be merged. It is
standard library only, with no dependencies.

## Usage

```go
package main

import (
	"context"
	"time"

	ttlcache "github.com/RichardoC/factory-repo/app/exp-ttl-deepseek-1"
)

func main() {
	c := ttlcache.New[string, int](ttlcache.Options{
		TTL:        30 * time.Second, // entries expire 30s after they are stored
		MaxEntries: 1024,             // keep at most 1024 live entries
		Now:        time.Now,         // the clock; nil means time.Now
	})

	c.Set("answer", 42)

	if v, ok := c.Get("answer"); ok {
		_ = v
	}

	v, err := c.GetOrLoad(context.Background(), "expensive", func(ctx context.Context) (int, error) {
		// Runs at most once at a time per key; its result is shared with any
		// other caller that missed on the same key.
		return 7, nil
	})
	_, _ = v, err

	c.Delete("answer")
	_ = c.Len() // number of live entries
}
```

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

### Options semantics

- **TTL** — an entry stored at time `t` (measured with `Now`) is live while
  `Now()` is before `t + TTL`. `Get`, `GetOrLoad` and `Len` never return or count
  an expired entry. A `TTL` of `0` (or less) disables expiry entirely.
- **MaxEntries** — while more than `MaxEntries` live entries would remain,
  storing evicts the least recently used live entry. A `MaxEntries` of `0` (or
  less) leaves the cache unbounded.
- **Now** — the clock used for every time decision, which makes the time-based
  rules testable with a fake clock. `nil` means `time.Now`.

### TTL and expiry

Expiry is lazy: an expired entry is noticed when it is looked up, counted by
`Len`, or purged while room is being made for a new entry. `Set` on an existing
key replaces the value and restarts the TTL; it does not create a second entry.

### LRU eviction

Recency is tracked with `container/list`, most recently used first. An entry
counts as used when it is stored — by `Set` or by a completed load — and when a
`Get` or `GetOrLoad` hit returns it. Expired entries are purged *before* room is
made, so an expired entry is never the reason a live entry is evicted.

### Concurrency and single-flight loading

A `Cache` is safe for concurrent use. One mutex guards the map and the LRU list,
and it is never held across a caller-supplied `load` function or across a wait
on a caller's context.

On a miss, concurrent `GetOrLoad` callers for the same key share exactly one
call of `load`; different keys load independently and never block each other.
`load` receives a context derived from `context.Background()` that is cancelled
exactly when the last caller waiting on that load gives up. That context is not
cancelled merely because one of several callers was cancelled.

A caller whose own context is done before the load finishes returns `ctx.Err()`
promptly; the load keeps running for the remaining waiters. If `load` returns an
error, nothing is cached and every waiter sees that error; a later `GetOrLoad`
calls `load` again. If `load` succeeds, the value is stored with its TTL starting
when `load` returned — even if every caller gave up first — and a result landing
after a `Delete` is still stored, because `Delete` does not stop an in-flight
load.

Each in-flight load owns a small state value (a mutex, a `sync.Cond`, a
cancellable context and a waiter count) and exactly one goroutine, which exits as
soon as `load` returns. No goroutine the cache starts outlives the load it
serves.

## Running the tests

From this directory:

```sh
gofmt -l .
go vet ./...
go build ./...
go test -race ./...
```

The suite covers every rule with its own named test and uses a fake clock for the
time-based ones. `TestR11Concurrency` and `TestR11CancelRacesLoadCompletion`
exercise the concurrency rules; run them under `-race` as above.

## Licence

MIT — see `LICENSE`.
