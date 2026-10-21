# ttlcache

A small, concurrency-safe TTL cache with LRU eviction and single-flight
loads. Standard library only, package `ttlcache` at the module root.

```go
c := ttlcache.New[string, []byte](ttlcache.Options{
	TTL:        time.Minute,
	MaxEntries: 1024,
})
c.Set("k", []byte("v"))
v, ok := c.Get("k")
data, err := c.GetOrLoad(ctx, "remote", func(ctx context.Context) ([]byte, error) {
	return fetch(ctx, "remote")
})
```

## Behaviour

- `Options.TTL > 0` expires an entry `TTL` after it was stored (`Set`, or the
  moment a load returned); `0` means entries never expire.
  `Options.MaxEntries > 0` bounds the live entries; `0` is unbounded.
  `Options.Now` is the clock, `time.Now` when nil.
- `Set` replaces a value and restarts its TTL. A `Get` or `GetOrLoad` hit
  counts as use, as does a store; when a store would leave more than
  `MaxEntries` live entries, the least recently used live entry is evicted.
  Expired entries are dropped first, so they never evict a live one.
- `Get` and `GetOrLoad` never return an expired entry; `Len` counts only live
  entries.
- Concurrent `GetOrLoad` misses for one key share a single `load` call. A
  failed load caches nothing and returns the same error to every waiter.
- A caller whose `ctx` is done returns `ctx.Err()` at once; the load keeps
  running, its context is cancelled only when no caller waits on it any more,
  and its result is still stored when it returns. `load` never sees the
  caller's own cancellation, so one caller giving up does not disturb the
  others. (A caller already cancelled on entry neither loads nor waits.)
- `Delete` removes a key but does not stop its in-flight load; that result is
  still stored.
- The cache starts one goroutine per in-flight load and it ends with the load,
  so nothing outlives the loads it serves. Safe for concurrent use; tests run
  under `-race`.

## Tests

`go test -race ./...`. Each rule of the specification has its own test in
`ttlcache_test.go` (`TestRule1Expiry` … `TestRule11NoGoroutineOutlivesLoads`).
