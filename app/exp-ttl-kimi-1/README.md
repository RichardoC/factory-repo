# ttlcache

Package `ttlcache` is a generic, concurrency-safe in-memory cache with:

- **Per-entry TTL** (`Options.TTL`): an entry stored at `t` is live while the
  cache clock is before `t + TTL`. With `TTL == 0` entries never expire.
  The clock comes from `Options.Now` (defaults to `time.Now`), which makes
  expiry fully deterministic in tests.
- **LRU eviction** (`Options.MaxEntries`): when storing would leave more
  than the maximum number of *live* entries, the least recently used live
  entry is evicted. Both stores and hits count as use; expired entries are
  purged first and never cause a live entry to be evicted.
- **Coordinated loading** (`GetOrLoad`): concurrent misses for one key share
  a single call of `load` (singleflight), while different keys load
  independently. Each caller may give up through its own context and returns
  `ctx.Err()` promptly; the load keeps running for the remaining callers.
  The context passed to `load` is cancelled exactly when the last waiter
  gives up — never just because one of several callers was cancelled. A
  failed load caches nothing and is retried by the next call; a successful
  result is stored even if every caller gave up, with its TTL starting when
  the load returned. `Delete` removes an entry but does not cancel an
  in-flight load.

## Usage

```go
c := ttlcache.New[string, int](ttlcache.Options{
    TTL:        time.Minute,
    MaxEntries: 1000,
})

c.Set("answer", 42)
v, ok := c.Get("answer")

v, err := c.GetOrLoad(ctx, "answer", func(ctx context.Context) (int, error) {
    return fetchFromOrigin(ctx)
})
```

## Development

Standard library only. From this directory:

```
gofmt -l . && go vet ./... && go build ./... && go test -race ./...
```
