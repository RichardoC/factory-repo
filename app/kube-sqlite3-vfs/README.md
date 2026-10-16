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

| Benchmark                     | ns/op before (run 1 / run 2) | ns/op after (run 1 / run 2) | API calls/op before | API calls/op after |
| ----------------------------- | ---------------------------- | --------------------------- | ------------------- | ------------------ |
| BenchmarkSequentialRead       | 320759 / 298050              | 291785 / 416090             | 3.00                | 2.00               |
| BenchmarkRandomPageRead       | 765906 / 705638              | 769798 / 682883             | 3.00                | 2.00               |
| BenchmarkSequentialWrite      | 334660 / 410730              | 334553 / 356783             | 3.00                | 2.00               |
| BenchmarkSQLiteInsert1000Rows | 3026310 / 2981848            | 2509512 / 2829036           | 39.00               | 14.00              |

What changed per operation: an unlocked read used to pay a LIST + a GET of the last sector (in `FileSize`) plus a GET for the sector read, and now pays one LIST + one GET (nothing at all while a lock is held — sectors come from the cache); a sector overwrite used to try CREATE first and fall back to UPDATE (two calls) and now writes the sector once, choosing UPDATE or CREATE from what is known; `Lock`, `Unlock` and `CheckReservedLock` used to GET the lock ConfigMap before every transition and now use the locally tracked state.