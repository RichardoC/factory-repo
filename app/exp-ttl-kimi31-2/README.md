# ttlcache

`ttlcache` is a standard-library-only, generic, concurrent cache with optional
per-entry TTL and LRU capacity limits. `Get`, `Set`, `Delete`, and `Len` provide
ordinary cache operations. `GetOrLoad` coalesces concurrent misses by key while
allowing different keys to load independently.

A caller may cancel its own wait without cancelling other waiters. The loader
context is cancelled after all waiters give up; a successful late result is
still cached. Expired values are never returned or counted by `Len`.
