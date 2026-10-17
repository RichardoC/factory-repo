# exp-ttl-baseline-1

Experiment (issue #81, baseline copy 1 — not for merging): a small in-memory
TTL + LRU cache with single-flight loading. Go standard library only.

```go
c := ttlcache.New[string, string](ttlcache.Options{
    TTL:        5 * time.Minute,
    MaxEntries: 1024,
})

v, err := c.GetOrLoad(ctx, "user:42", func(ctx context.Context) (string, error) {
    return fetchUser(ctx, 42)
})
```

## API

```go
type Options struct {
    TTL        time.Duration    // > 0: an entry expires TTL after it was stored; 0: entries never expire
    MaxEntries int              // > 0: at most this many live entries; 0: unbounded
    Now        func() time.Time // the clock; nil means time.Now
}

func New[K comparable, V any](opts Options) *Cache[K, V]
func (c *Cache[K, V]) Get(key K) (V, bool)
func (c *Cache[K, V]) Set(key K, value V)
func (c *Cache[K, V]) Delete(key K)
func (c *Cache[K, V]) Len() int
func (c *Cache[K, V]) GetOrLoad(ctx context.Context, key K, load func(ctx context.Context) (V, error)) (V, error)
```

## Semantics

- **Expiry.** An entry stored at time `t` (by the configured clock) is live
  while the clock is *strictly before* `t + TTL`. `Get`, `GetOrLoad` and
  `Len` never observe an expired entry; an expired entry found on lookup is
  removed. Hits do not extend an entry's TTL.
- **Replacement.** `Set` on an existing key replaces the value and restarts
  its TTL.
- **LRU eviction.** When storing an entry would leave more than `MaxEntries`
  live entries, expired entries are dropped first (they never cause a live
  entry to be evicted) and then the least recently used live entry is
  evicted. Storing an entry and a `Get`/`GetOrLoad` hit each count as a use.
- **Single-flight loads.** On a miss, concurrent `GetOrLoad` callers for the
  same key share one `load` call; different keys load independently. A failed
  load is not cached, so a later `GetOrLoad` calls `load` again.
- **Cancellation.** A caller whose context is done before the load finishes
  returns `ctx.Err()` promptly; the load keeps running for the remaining
  waiters. The context passed to `load` carries the first caller's values but
  is never cancelled by any single caller; it is cancelled exactly when every
  caller waiting on that load has given up. A value `load` returns
  successfully is stored even if every caller gave up, with its TTL starting
  when `load` returned.
- **Delete.** `Delete` removes the key but does not stop an in-flight load;
  that load's result is still stored when it completes.

### Edge cases worth knowing

- A negative `TTL` or `MaxEntries` is treated like `0` (never expire /
  unbounded).
- If every waiter on a load gives up, the load's context is cancelled but the
  load itself is still in flight. A `GetOrLoad` arriving in that window joins
  the existing call (keeping "at most one load per key at a time" true) and
  receives whatever that load returns — typically `context.Canceled` if
  `load` honors its context. Once the load returns, later callers start
  fresh.
- `load` must not panic (not handled, per the task statement).

## Development

From this directory:

```
gofmt -l .
go vet ./...
go build ./...
go test -race ./...
```

Tests use a controllable fake clock for expiry semantics and real channels
for load/cancellation interleavings; they run without a network or cluster.
