# kube-sqlite3-vfs
sqlite3 using kubernetes as the backing storage

## Requirements and building

The module targets **Go 1.27** (`go 1.27` in `go.mod`; CI uses the version from
`go.mod`, so no explicit toolchain line is needed). Building `main` and the
`cmd/` tools needs a **cgo toolchain** and a **C compiler**: `mattn/go-sqlite3`
is imported for the `sqlite3_*` symbols the `sqlite3vfs` C shim links against.

```sh
cd app/kube-sqlite3-vfs
go build ./...                 # library and all four commands
go build -o kube-sqlite3-vfs . # just the demo binary
```

## Running the tests and benchmarks

```sh
cd app/kube-sqlite3-vfs
go test -race ./...                                              # unit tests
go test ./pkg/vfs -run ZZZ -bench . -benchmem                    # benchmarks
go test ./pkg/vfs -run ZZZ -bench . -benchtime 10x               # benchmarks, fixed iteration count
```

`-run ZZZ` matches no test, so only the benchmarks run. The tests use
client-go's fake clientset and need no cluster. `api-calls/op` is stable
between runs; `ns/op` is not, so compare medians of several `-count=5` runs
rather than single numbers.

## How it works

Creates a set of configmaps per file requested, which is named a base32 encoding of the filename
the namespace contains
a configmap called "lockfile" which contains the lock information
a series of configmaps named which contain up to 64kB of data each

namespaces all labelled with "kube-sqlite3-vfs": "used" to ease cleanup

Configmap names can be 253 characters long after encoding

## Command-line flags

All four commands (`main`, `db-upload`, `db-download`, `file-create`) share:

* `--kubeconfig PATH` — absolute path to the kubeconfig file (defaults to
  `$HOME/.kube/config`);
* `-v`, `--verbosity` — log at **debug** level as text instead of the default
  info-level JSON;
* `--retries N` — number of attempts `Open` makes against races between
  concurrent clients (default `1`).

`db-download` additionally takes `--filename PATH`, the local SQLite file to
store in the cluster, defaulting to `file2.db`. (`db-upload` and
`db-download` are demo tools with fixed remote name `file2.db`.)

Every command prints an error to stderr and exits non-zero; none of them
panics on an ordinary error.

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
## Go 1.27 and dependencies (issue #9)

The module moved from `go 1.19` to `go 1.27` and its dependencies from their
2023 versions, among them several `golang.org/x/*` releases with published
vulnerabilities. `govulncheck ./...` now reports no vulnerabilities in this
module.

| Module | Old | New |
| ------ | --- | --- |
| Go (language/module version) | 1.19 | 1.27 |
| `k8s.io/api` | v0.26.0 | v0.37.1 |
| `k8s.io/apimachinery` | v0.26.0 | v0.37.1 |
| `k8s.io/client-go` | v0.26.0 | v0.37.1 |
| `github.com/mattn/go-sqlite3` | v1.14.16 | v1.14.52 |
| `github.com/google/uuid` | v1.3.0 | v1.6.0 |
| `github.com/thought-machine/go-flags` | v1.5.0 | v1.7.0 |
| `github.com/psanford/sqlite3vfs` | v0.0.0-20260428… | v0.0.0-20260519004904-f9180fa2acc9 |
| `go.uber.org/zap` | v1.24.0 | **removed** — replaced by `log/slog` |
| `golang.org/x/net`, `.../sys`, `.../text`, `.../time`, `.../term`, `.../oauth2` and the rest of the indirect set | 2023 releases | current releases (`golang.org/x/net` v0.61.0, `golang.org/x/sys` v0.49.0, …) |

`k8s.io/{api,apimachinery,client-go}` move together at the same version, as
they must. Everything else is at `latest`; `go mod tidy` is clean. Nothing
in the direct set had to be held back — the only dependency that went away is
zap.

### Modernisation (behaviour unchanged except where noted)

* **`context.Context` through the VFS.** `NewVFS(ctx, kc, namespace, logger, retries)`
  holds the context and every Kubernetes call the VFS makes uses it, so callers
  can cancel or time out API calls. There is no `context.TODO()` left in the
  library. SQLite calls into the VFS through cgo, so there is no per-call
  context to accept; the context is per VFS.
* **Errors, not panics.** The library returns errors: `Unlock` used to call
  `logger.Panicf`/`Panic` on an invalid request and now returns a wrapped error,
  pinned by `TestUnlockRejectsInvalidRequests`. The commands print the error to
  stderr and exit non-zero. *Deliberate behaviour change* (see below).
* **`errors.Is`/`errors.As` and `%w`.** Sentinels such as `errSectorNotFound`
  are compared with `errors.Is`, and errors that gain context are wrapped with
  `%w`.
* **Modern idioms** where they simplify: `maps.Copy`, builtin `min`/`max`,
  range-over-int, `math/rand/v2`.
* **Dead code removed.** `cmd/db-create` (whose body after flag/logger setup was
  entirely commented out) is deleted, along with the large commented-out block
  in `cmd/db-upload` and the write-never-read `Sector.Labels` field.
* **Docs.** `pkg/vfs` has a package comment and every exported identifier has a
  doc comment.
* **zap → `log/slog`.** zap is gone from `go.mod` and `go.sum`. The diff stays
  small because every zap call maps onto an equivalent `slog` call
  (`logger.Info`/`Error`/`Debug`/`With`), the standard library removes two
  direct dependencies (`go.uber.org/zap`, `go.uber.org/multierr`) and their
  indirect tail, and `-v` keeps exactly its old meaning: debug-level logging
  (as `zap.NewDevelopment()` was) instead of info-level JSON.

### Deliberate behaviour changes

1. **`Unlock` returns an error instead of panicking** on an invalid request. A
   panic unwound through the cgo sqlite boundary and killed the process without
   a usable message; the caller now gets a wrapped error.
2. **The commands exit non-zero with a stderr message instead of panicking.**
3. **`db-download --filename` defaults to `file2.db`** instead of the author's
   hardcoded `/home/richardf/gitclones/kube-sqlite3-vfs/file2.db`. The old
   default could not work on any other machine.
4. **`-v` selects slog debug text logging** instead of zap's development logger.
   The effective level (debug) is unchanged.

## Benchmark results after the Go 1.27 / dependency work (issue #9)

Medians of a `-count=5` run of `go test ./pkg/vfs -run ZZZ -bench . -benchmem`,
before (the tree as it stood after issue #8, recorded on the same machine) and
after the modernisation. `api-calls/op` is stable; `ns/op` is not.

| Benchmark | ns/op before | ns/op after | change | API calls/op |
| --------- | ------------ | ----------- | ------ | ------------ |
| BenchmarkSequentialRead | 162694 | ~154000–156000 | −4% to −5% | 2 (unchanged) |
| BenchmarkSequentialReadLocked | 1694 | ~1685–1706 | ±1% | 0 (unchanged) |
| BenchmarkRandomPageRead | 327324 | ~325000–336000 | −1% to +3% | 2 (unchanged) |
| BenchmarkSequentialWrite | 158834 | ~161000–167000 | +1% to +5% | 2 (unchanged) |
| BenchmarkSQLiteInsert1000Rows | 2933441 | ~2730000–2820000 | −4% to −7% | 17 (unchanged) |

No benchmark regressed by more than 10%; the worst movement is a ~5% `ns/op`
increase on `BenchmarkSequentialWrite`, which is within run-to-run noise on this
shared machine and less than the already-recorded variation of the issue #8
baseline. `api-calls/op` is identical for every benchmark, so the on-cluster
call pattern did not change.
