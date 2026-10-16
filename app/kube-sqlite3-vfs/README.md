# kube-sqlite3-vfs
sqlite3 using kubernetes as the backing storage

## How it works

Creates a set of configmaps per file requested, which is named a base32 encoding of the filename
the namespace contains
a configmap called "lockfile" which contains the lock information
a series of configmaps named which contain up to 64kB of data each

namespaces all labelled with "kube-sqlite3-vfs": "used" to ease cleanup

Configmap names can be 253 characters long after encoding

## WARNINGS

Using an in-memory journal (`_journal=MEMORY`, as `main.go` and the cmd tools do) still means a crash in the middle of a transaction cannot be rolled back, so it is very likely to corrupt your data. That warning stands.

**Concurrent writers from several processes are not safe.** The lock ConfigMap gives no real mutual exclusion: it stores one bare lock level, has no owner, and every transition is an unconditional Update with no resourceVersion. Nothing refuses a transition to a level another process already holds — a second process's `Lock` simply overwrites the level the first one wrote — and sector writes are never checked against the lock, so a writer's EXCLUSIVE lock does not actually stop another process from writing (or from serving reads from its own cache while that write happens). Use one writer at a time. This is true of the code as imported too; fixing it would need owner tracking plus compare-and-swap (resourceVersion) updates on the lock ConfigMap and is deliberately out of scope here.

The VFS is no longer "really very slow" at the Kubernetes API, though: reads and writes no longer LIST every sector ConfigMap of the file, `FileSize` is O(1) from memory while a SHARED-or-better lock is held (and otherwise costs one LIST, which also revalidates and warms the sector cache), each changed sector is written exactly once per write (UPDATE or CREATE chosen from what is known), reads never create missing sectors, and the lock level is tracked locally instead of GETting the lock ConfigMap before every transition. See the benchmarks below.

## TODO

* ~~have filesize be O(1) rather than O(n)~~ — done: `FileSize` is O(1) while a SHARED-or-better lock is held, and otherwise costs one LIST that revalidates and warms the sector cache.

## On-cluster data layout (unchanged)

The optimisation did not change the on-cluster data layout, so databases written by the old code remain readable by the new code and vice versa:

* sector ConfigMaps are still named `<base32 filename>-<index>`, labelled `data: sector` and `relevant-file: <base32 filename>`, and hold the sector bytes in the `sector` binary key plus the `filename` data key;
* the lockfile ConfigMap is still named `<base32 filename>-lockfile`, labelled `data: lockfile` and `relevant-file`, and holds the lock level in the `lock` data key;
* namespaces are still labelled `kube-sqlite3-vfs: used`.

## Benchmarks (issue #8: fewer API calls per operation)

Measured on the fake clientset with `go test ./pkg/vfs -run ZZZ -bench . -benchtime 10x`, two runs each. ns/op varies from run to run; api-calls/op is the number that matters and is stable, counted from the fake clientset's recorded actions (`fake.ClearActions()` around the operation, `len(fake.Actions())` after).

| Benchmark                                                               | ns/op before (run 1 / run 2) | ns/op after (run 1 / run 2) | API calls/op before | API calls/op after |
| ----------------------------------------------------------------------- | ---------------------------- | --------------------------- | ------------------- | ------------------ |
| BenchmarkSequentialRead                                                 | 320759 / 298050              | 287236 / 211234             | 3.00                | 2.00               |
| BenchmarkSequentialReadLocked (held SHARED lock)                        | —                            | 150826 / 91810              | —                   | 0.00               |
| BenchmarkRandomPageRead                                                 | 765906 / 705638              | 582095 / 647806             | 3.00                | 2.00               |
| BenchmarkSequentialWrite                                                | 334660 / 410730              | 214296 / 240557             | 3.00                | 2.00               |
| BenchmarkSQLiteInsert1000Rows (one 64 KiB sector, 4 KiB pages — superseded workload) | 3026310 / 2981848 | 2509512 / 2829036         | 39.00               | 14.00              |
| BenchmarkSQLiteInsert1000Rows (64 KiB pages, transaction spans 7 sectors) | —                          | 2453079 / 2389578           | —                   | 17.00              |

What changed per operation: an unlocked read used to pay a LIST + a GET of the last sector (in `FileSize`) plus a GET for the sector read, and now pays one LIST + one GET (nothing at all while a lock is held — sectors come from the cache); a sector overwrite used to try CREATE first and fall back to UPDATE (two calls) and now writes the sector once, choosing UPDATE or CREATE from what is known; `Lock`, `Unlock` and `CheckReservedLock` used to GET the lock ConfigMap before every transition and now use the locally tracked state.

BenchmarkSequentialReadLocked is new: it measures the under-lock steady state directly. After one warm-up read pays the revalidating LIST, a read under a held SHARED lock costs zero API calls — `FileSize` is served from memory and every sector comes from the cache.

The insert benchmark now runs at 64 KiB pages (the `main.go`/cmd-tools configuration: one VFS sector per page) with ~200-byte rows, so its transaction spans seven 64 KiB sectors and exercises the multi-sector write path. Its previous one-sector numbers are kept above for the record. There is no before number for the multi-sector workload: the pre-optimisation code panicked the moment a database outgrew its first sector, so it cannot run this workload at all.