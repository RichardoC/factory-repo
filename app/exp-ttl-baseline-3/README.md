# exp-ttl-baseline-3

Model experiment (issue #269, not for merging): a TTL + LRU cache with
single-flight loading, standard library only.

## Usage

```go
c := ttlcache.New[string, int](ttlcache.Options{
	TTL:        10 * time.Second, // 0 = entries never expire
	MaxEntries: 1024,             // 0 = unbounded
	Now:        nil,              // nil = time.Now; inject a clock for tests
})

c.Set("k", 42)
v, ok := c.Get("k") // (42, true); a hit marks the entry most recently used

v, err := c.GetOrLoad(ctx, "k", func(ctx context.Context) (int, error) {
	return loadFromBackend(ctx, "k")
})
```

## Semantics

- **Expiry** — an entry stored at time `t` is live while `Now() < t + TTL`.
  `Get` and `GetOrLoad` never return an expired entry; `Len` counts only
  live entries.
- **Replacement** — `Set` on an existing key replaces the value and
  restarts its TTL.
- **LRU eviction** — when storing an entry would leave more than
  `MaxEntries` live entries, the least recently used live entry is evicted.
  Storing and hit reads count as use. Expired entries never cause a live
  entry to be evicted.
- **Single flight** — on a miss, concurrent `GetOrLoad` callers for the same
  key share one `load` call; different keys load independently. A live hit
  never calls `load`.
- **Errors** — a failing `load` caches nothing and gives every waiter the
  same error; the next `GetOrLoad` calls `load` again.
- **Giving up** — a caller whose `ctx` finishes early returns `ctx.Err()`
  promptly. The `load` keeps running for the remaining waiters, and the
  context handed to `load` is cancelled exactly when every waiter has given
  up. A successful result is stored even if every caller gave up, with its
  TTL starting when `load` returned.
- **Delete** — removes the key but does not stop an in-flight load; that
  load's successful result is still stored when it completes.

`load` must not panic (not handled). The cache is safe for concurrent use;
run `go test -race ./...` to verify.

## Development

```sh
cd app/exp-ttl-baseline-3
gofmt -l . && go vet ./... && go build ./... && go test -race ./...
```
