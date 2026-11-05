# ttlcache

`ttlcache` is a generic, concurrency-safe in-memory cache. Entries can expire
after a fixed TTL and live entries can be bounded with least-recently-used
eviction. A zero TTL disables expiry and a zero `MaxEntries` disables the size
limit. `Options.Now` can supply a test clock.

`GetOrLoad` coalesces concurrent misses by key while allowing different keys to
load independently. Callers may stop waiting through their own contexts; the
shared load is cancelled only after every waiter has done so. Successful late
results are still cached. No background goroutines remain after a load returns.
