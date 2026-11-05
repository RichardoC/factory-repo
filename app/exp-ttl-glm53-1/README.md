# exp-ttl-glm53-1 — TTL cache experiment

A model experiment (not for merging). A small, standard-library-only Go
package providing an in-memory cache with per-entry TTL, optional
least-recently-used size bounding, and single-flight loading.

```go
c := ttlcache.New[string, string](ttlcache.Options{TTL: 5 * time.Minute, MaxEntries: 1000})
v, err := c.GetOrLoad(ctx, "key", func(ctx context.Context) (string, error) {
    return loadFromSomewhere(ctx, "key")
})
```

## API

- `type Options struct { TTL time.Duration; MaxEntries int; Now func() time.Time }`
  — `TTL` 0 (or negative) means entries never expire; `MaxEntries` 0 (or
  negative) means unbounded; `Now` nil means `time.Now`.
- `New[K comparable, V any](opts Options) *Cache[K, V]`
- `Get(key K) (V, bool)` — live entries only; a hit marks the entry used.
- `Set(key K, value V)` — replaces and restarts the TTL of an existing key.
- `Delete(key K)` — removes the key; does not stop an in-flight load.
- `Len() int` — live entries only (expired entries are discarded on the way).
- `GetOrLoad(ctx, key, load) (V, error)` — single-flight loading:
  concurrent callers for one key share one `load` call; different keys load
  independently.

## Semantics

- An entry stored at time `t` is live while `Now()` is before `t + TTL`.
- Storing past a `MaxEntries` bound evicts the least recently *used* live
  entry (used = stored, returned by a `Get`/`GetOrLoad` hit, or stored by a
  completed load). Expired entries are dropped first and never cause a
  live entry to be evicted.
- A load runs on a goroutine the cache starts; that goroutine ends when the
  load returns, so no goroutine the cache starts outlives the loads it
  serves.
- A caller whose `ctx` is done before the load finishes returns `ctx.Err()`
  promptly; the load keeps running for the remaining callers.
- The `load` context carries the values of the first caller's `ctx`
  (`context.WithoutCancel`) but is cancelled by the cache exactly when the
  last caller waiting on that load stops waiting, and never before.
- A successful load result is stored with its TTL measured from when `load`
  returned, even if every caller gave up in the meantime. A load error is
  never cached, is returned to every waiting caller, and the next
  `GetOrLoad` calls `load` again.

The package uses the Go standard library only. Tests cover each rule
above; run them with `go test -race ./...` inside this directory.
