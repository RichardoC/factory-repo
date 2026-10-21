# ttlcache

A small, concurrency-safe in-memory cache: per-entry **time-to-live** expiry,
**least-recently-used** eviction and a **single-flight** read-through loader.
Standard library only.

This is a model experiment (issue #271), not production code. It lives in its
own module under `app/` and is not meant to be merged; it must not be changed by
ordinary issues.

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

- `TTL` — `> 0` expires an entry `TTL` after it was stored; `0` means entries
  never expire (a negative value is treated as `0`).
- `MaxEntries` — `> 0` keeps at most this many live entries; `0` means unbounded
  (a negative value is treated as `0`).
- `Now` — the clock used to stamp stores and decide expiry; `nil` means
  `time.Now`.

The zero `Cache` is not usable — call `New`.

## Rules

The numbering is the issue's.

1. **Expiry.** An entry stored at time `t` (by `Now()`) is live while `Now()` is
   before `t + TTL`. `Get` and `GetOrLoad` never return an expired entry (an
   expired `Get` also removes it), and `Len` reaps expired entries so it counts
   only live ones.
2. **Replacement.** `Set` on an existing key replaces the value and restarts its
   TTL: the store time becomes `Now()`.
3. **LRU eviction.** When storing would leave more than `MaxEntries` live
   entries, the least recently used live entry is evicted. An entry counts as
   used when it is stored (by `Set` or a completed load) and when a `Get` or
   `GetOrLoad` hit returns it. Expired entries are dropped before any eviction
   runs, so an expired entry never costs a live one.
4. **Hits.** `GetOrLoad` on a live hit returns the cached value without calling
   `load`; the hit counts as a use.
5. **One load per key.** On a miss, concurrent `GetOrLoad` callers for a key
   join one in-flight `call` and share a single `load`. Different keys load
   independently: `load` runs with no lock held.
6. **Load errors.** A load that returns an error caches nothing; every caller
   waiting on that load gets that same error, and a later `GetOrLoad` calls
   `load` again.
7. **Callers that give up.** A caller whose `ctx` is done before the load
   finishes returns `ctx.Err()` promptly. Giving up only drops that caller's
   waiter slot; the load keeps running for the other waiters.
8. **The load's context.** The context passed to `load` is cancelled exactly
   when the last waiter gives up (the waiter count reaching zero). It is never
   cancelled while one waiter remains, and never because a single caller
   cancelled.
9. **Late results.** A successful load is stored even if every caller has given
   up, with its TTL starting when `load` returned.
10. **Delete.** `Delete` removes the key. It does not stop an in-flight load for
    that key: when that load completes successfully its value is stored anyway.
11. **Concurrency.** The cache is safe for concurrent use and `go test -race` is
    clean. Exactly one goroutine is started per load, and it exits as soon as it
    has decided and stored the result — it outlives nothing it serves.
12. **Panics.** `load` is assumed not to panic; the cache does not recover.

## Design

One `sync.Mutex` guards the entries map, the LRU list and the in-flight table.
`load` always runs with no lock held, so a slow key never blocks another. Each
load gets one goroutine and its own context derived from
`context.Background()` — never a caller's, so no caller's cancellation can reach
it; the waiter bookkeeping cancels it when the last waiter gives up. A result is
published by writing `value`/`err` under the lock, then closing `done`.

## Usage

```go
package main

import (
	"context"
	"fmt"

	"github.com/RichardoC/factory-repo/app/exp-ttl-deepseek-4"
)

func main() {
	c := ttlcache.New[string, string](ttlcache.Options{TTL: 0, MaxEntries: 100})
	_ = c

	v, err := c.GetOrLoad(context.Background(), "greeting", func(ctx context.Context) (string, error) {
		return "hello", nil
	})
	if err != nil {
		panic(err)
	}
	fmt.Println(v) // hello
}
```

## Tests

Everything runs under `go test -race`.

- `cache_test.go` — the expiry/LRU engine: `TestExpiryBoundary`,
  `TestExpiryReapsOnLenWithoutGet` (rule 1), `TestTTLZeroNeverExpires`,
  `TestNegativeTTLNeverExpires`, `TestSetReplacesAndRestartsTTL` (2),
  `TestLRUGetIsAUse`, `TestLRUSetIsAUse`, `TestLRUExpiredNeverEvictsLive`,
  `TestLRUExpiredReapedBeforeTrim` and friends (3).
- `load_test.go` — the load rules (4–10): `TestGetOrLoadLiveHitDoesNotLoad`
  (4), `TestConcurrentSameKeyOneLoad` and
  `TestConcurrentDifferentKeysLoadIndependently` (5),
  `TestLoadErrorNotCached` (6), `TestCallerGivesUpPromptly` (7),
  `TestLoadContextCancelledOnlyWhenLastWaiterGivesUp` (8),
  `TestLateResultStoredAfterEveryCallerGaveUp` (9),
  `TestDeleteDoesNotStopInFlightLoad` (10), plus `TestNoGoroutineOutlivesItsLoad`
  for the second half of rule 11.
- `race_test.go` — the race detector evidence for rules 11–12:
  `TestRaceMixedOperations`, `TestRaceStoresUnderEviction`,
  `TestNoGoroutineOutlivesLoadUnderRace` and the key-isolation race tests.
- `bench_test.go` — benchmarks (`BenchmarkGetBound`, `BenchmarkGetParallel`,
  `BenchmarkGetOrLoadHit`) used as before/after evidence for later changes; no
  numbers are quoted here.

Also clean in this module: `gofmt -l .`, `go vet ./...`, `go build ./...`.
