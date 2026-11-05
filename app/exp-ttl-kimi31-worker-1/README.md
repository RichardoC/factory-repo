# ttlcache

A standard-library-only, generic in-memory cache with optional per-entry TTL
expiration and least-recently-used eviction. `GetOrLoad` coalesces concurrent
loads by key while allowing different keys to load independently. Cancellation
is per caller; the shared load is cancelled only after its last caller gives up,
and a successful late result is still cached.

Create one with `ttlcache.New[K, V](ttlcache.Options{...})`. A zero TTL means no
expiration and zero `MaxEntries` means no size limit.
