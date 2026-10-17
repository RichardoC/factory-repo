# kube-sqlite3-vfs
sqlite3 using kubernetes as the backing storage

## Requirements and building

The module requires **Go 1.27.2 or later** (`go 1.27.2` in `go.mod`; CI uses
the version from `go.mod`, so no explicit toolchain line is needed. Toolchains
older than 1.27.2 are refused: their standard library carries
vulnerabilities govulncheck reports as called, all fixed in 1.27.2).
Building `main` and the
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
a configmap called `<base32 filename>-lockfile` which holds the locking state
(issue #47): the `lock` data key still carries the highest lock level any live
holder has — the aggregate old readers saw; a `shared-holders` data key maps every
SHARED holder's id (a UUID generated per `Open`) to its lease expiry; a `writer`
data key carries the one writer's id, level and lease expiry, or is empty when no
writer holds
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

**Concurrent access between processes is now guarded by real mutual exclusion (issue #47) — under the limits listed below.** The lock ConfigMap no longer stores one bare level written blindly. Every transition is a compare-and-swap: the holder state is read, the new state is decided from what was read, and the Update carries the `resourceVersion` just read, so two processes can never overwrite each other's holdings; on a 409 Conflict the state is re-read and the decision re-made, never forced. A missing lockfile is created with Create, and `AlreadyExists` from that Create is treated as "re-read". Each file — generated once per `Open`, and per `NewFile` — carries a holder id (a UUID) and every lock operation acts as that holder, so a process can tell its own locks from another process's; `Close` releases them. The stored state expresses sqlite's model: any number of SHARED holders, at most one writer at a time (RESERVED, PENDING or EXCLUSIVE), and EXCLUSIVE only once no other holder remains. A transition another process has made impossible — a second RESERVED, EXCLUSIVE while other SHARED holders remain — returns SQLITE_BUSY (`sqlite3vfs.BusyError`) instead of blocking; sqlite retries on its own schedule. The aggregate `lock` key still holds the highest level any live holder has, so a reader of the old single-level format still sees a meaningful value. The #19 cache rules are unchanged: caches are dropped before any downgrade is written, and a file never adopts the ConfigMap's stored state as its own lock.

The guarantees hold only under these limits:

* **Roughly synchronised clocks.** Leases compare client-generated UTC timestamps (default 30 s, configurable with `WithLockLease`): every holder entry carries an expiry, entries past their expiry are pruned before any lock decision, and a successful CAS re-stamps the holder's own entries.
* **A paused process can lose its lock.** A process paused or suspended for longer than its lease — a laptop asleep, a VM frozen, a SIGSTOP — has its entries pruned by the next holder's CAS and its lock can be taken by another process. On resuming, its next lock-path operation reports the loss and resets its local state instead of writing under a false belief.
* **Every process must run this version.** A process still running the pre-#47 code updates the lock ConfigMap unconditionally, carrying no resourceVersion, and overwrites whatever holders are stored there. The guarantee holds only while every process touching the database runs the new code.
* **Lease renewal needs the file to be driven.** `Lock`, `Unlock`, `Close` and `CheckReservedLock` re-stamp the lease when less than half of it remains, and so do `ReadAt` and `WriteAt` — the read/write path used to gain no renewal at all, so one I/O stretch longer than the lease could lapse the lease mid-transaction while `WriteAt` kept writing. Now a due renewal rides the next sector read or write at zero extra API calls while the lease is fresh, and once the lease is lost the file refuses to serve or write sectors (`SQLITE_BUSY`) until sqlite re-locks and revalidates. A transaction still lapses if its I/O stops entirely — a paused process loses its lock as described above — but a merely long transaction keeps its lease as long as its I/O keeps flowing.
* **The file advertises `SQLITE_IOCAP_SAFE_APPEND` (and atomic 64 KiB writes).** SQLite only writes a rollback journal's magic bytes at offset 0 when the database file claims safe appends; otherwise it writes zeros there on purpose, and `hasHotJournal` — which reads that byte to decide whether a journal is hot — then never asks whether a live writer holds a reserved lock. One sector is one whole ConfigMap, written exactly once, so an append can never leave garbage after bytes a reader has seen: the guarantee the unix VFS reports for the same flag. Without it SQLite's hot-journal protocol is short-circuited and a reader can play back a live writer's journal.
* **Sector writes are gated only by lease loss, not by lock level.** The exclusion is enforced on the lock transitions sqlite performs; `WriteAt` is refused (`SQLITE_BUSY`) only once this file's own lease has been pruned by another process — until a successful `Lock` re-establishes the holder — so a file never keeps writing sectors under a lock it no longer holds. A file that never took a lock at all (the cmd tools, which drive `NewFile` directly) stays ungated, as before: it still must not be run against a database other processes are using.

### Known upstream bug: `CheckReservedLock` is inverted by the dependency

At the pinned version of [`github.com/psanford/sqlite3vfs`](https://github.com/psanford/sqlite3vfs) (`v0.0.0-20260519004904-f9180fa2acc9`), the cgo shim's `goVFSCheckReservedLock` writes `*pResOut = 0` when the Go `File.CheckReservedLock` answers `true` and `1` when it answers `false` (`sqlite3vfscgo.go`, around lines 399–403) — the opposite of SQLite's `xCheckReservedLock` contract, which is "set `*pResOut` to a non-zero value if a reserved lock exists". SQLite consumes that value in `hasHotJournal` to decide whether an existing rollback journal belongs to a live writer (reserved lock held → leave it alone) or is a stale crash remnant (nobody holds reserved → play it back), so with the uncorrected inversion SQLite can read or delete another writer's journal mid-write.

Worked around in this VFS: `file.CheckReservedLock` deliberately returns the negation of the true answer — the last Go code the answer passes through — so the value that reaches SQLite is correct. The cost is that the Go-level answer is inverted for direct callers too (the tests pin the inverted expectations). Remove the negation — and flip those expectations — once upstream maps the File's answer straight through.

`TestCheckReservedLockCrossesCGOBoundary` pins the crossing itself, through real SQLite across two VFS instances: writer A holds RESERVED with its rollback journal on the disk, reader B's first read reaches `hasHotJournal`, and B must leave A's journal alone. That test only exercises the answer if the file advertises `SQLITE_IOCAP_SAFE_APPEND`; without it SQLite zeroes the journal's magic at offset 0 and `hasHotJournal` discards the value before `xCheckReservedLock` can matter — see the device-characteristics bullet above. The test therefore fails if the negation is removed AND the characteristic is advertised, and passing in-process assertions alone cannot see this: the value SQLite finally consumes is what counts.

The VFS is no longer "really very slow" at the Kubernetes API, though: reads and writes no longer LIST every sector ConfigMap of the file, `FileSize` is O(1) from memory while a SHARED-or-better lock is held (and otherwise costs one LIST, which also revalidates and warms the sector cache), each changed sector is written exactly once per write (UPDATE or CREATE chosen from what is known), reads never create missing sectors, and a lock transition for a level already held still makes no lock API call at all. See the benchmarks below.

### API changes and migration notes (issue #47)

* **`NewVFS` takes variadic options.** The signature is now `NewVFS(ctx, kc, namespace, logger, retries, opts ...Option)`. Existing calls compile unchanged (the variadic parameter may be omitted); to pass options, append them: `NewVFS(ctx, kc, ns, logger, retries, vfs.WithLockLease(10*time.Second))`. The only option today is `WithLockLease`.
* **`WithLockLease(0)` (or any non-positive value) keeps the default 30 s lease.** `Option` cannot return an error, so an out-of-range value is documented behaviour — the default stands — rather than a silently broken lease. Pass a positive duration to shorten or lengthen the lease.

## TODO

* ~~have filesize be O(1) rather than O(n)~~ — done: `FileSize` is O(1) while a SHARED-or-better lock is held, and otherwise costs one LIST that revalidates and warms the sector cache.

## On-cluster data layout (issue #47 added lockfile keys; nothing else moved)

The on-cluster data layout is backward and forward compatible — databases written by the old code remain readable by the new code and vice versa — but issue #47 did add two data keys to the lockfile ConfigMap (see the bullet below):

* sector ConfigMaps are still named `<base32 filename>-<index>`, labelled `data: sector` and `relevant-file: <base32 filename>`, and hold the sector bytes in the `sector` binary key plus the `filename` data key;
* the lockfile ConfigMap is still named `<base32 filename>-lockfile`, labelled `data: lockfile` and `relevant-file`, and holds the locking state: the lock level — the highest level any live holder has — in the `lock` data key as before, plus the issue #47 keys `shared-holders` (holder id → lease expiry) and `writer` (the one writer's id, level and lease); a lockfile written by the old code carries only `lock`, which the new code reads as "nobody holds anything" until its next CAS rewrites it in the new format;
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

## Locking (issue #47): mutual exclusion and what it costs

The WARNINGS section above describes the new locking guarantees: compare-and-swap on the lock ConfigMap via `resourceVersion`, holder ids, leases. This table records what that costs at the Kubernetes API, measured as in the issue #8 table above — fake clientset, `go test ./pkg/vfs -run ZZZ -bench . -benchtime 10x -count 2`; BEFORE was recorded on the pre-#47 tree with the same flags, AFTER on this tree. `BenchmarkLockUnlockShared` was additionally run with `-count 5` for a stable median. ns/op varies between runs; api-calls/op is exact and is the number that matters.

| Benchmark                                                                | api-calls/op before | api-calls/op after | ns/op after (run 1 / run 2) |
| ------------------------------------------------------------------------ | ------------------- | ------------------ | --------------------------- |
| BenchmarkSequentialRead                                                  | 2.000               | 2.000              | 197822 / 212903             |
| BenchmarkSequentialReadLocked (held SHARED lock)                         | 0                   | 0                  | 80374 / 87765               |
| BenchmarkRandomPageRead                                                  | 2.000               | 2.000              | 468399 / 535553             |
| BenchmarkSequentialWrite                                                 | 2.000               | 2.000              | 200935 / 216833             |
| BenchmarkSQLiteInsert1000Rows (64 KiB pages, transaction spans 7 sectors) | 17.00               | 24.00              | 2456432 / 2197694           |
| BenchmarkLockUnlockShared                                                | 2.000               | 4.000              | 24686 / 19554               |
| BenchmarkLockUnlockExclusive (new)                                       | —                   | 6.000              | 30912 / 31336               |

The read and write paths are untouched — their api-calls/op is identical, because the sector path still makes no lock calls at all (`BenchmarkSequentialReadLocked` stays at 0). Everything that moved is the CAS itself: a transition that writes now GETs the lock ConfigMap first, where the old code wrote blindly. `BenchmarkLockUnlockShared`'s acquire + release cycle goes 2 → 4 (one GET + one UPDATE per transition). The insert transaction performs five lock transitions — SHARED, RESERVED, EXCLUSIVE, back to SHARED, NONE — costing +5 GETs, and its EXCLUSIVE climb is now two CAS steps (claim PENDING, then acquire EXCLUSIVE once no other holder remains) where the old code wrote EXCLUSIVE in a single blind UPDATE, another +1 GET +1 UPDATE: 17 → 24. `BenchmarkLockUnlockExclusive` is new and has no before number; its three transitions (PENDING claim, EXCLUSIVE acquire, downgrade to SHARED) cost 6. The extra GET and the JSON round-trip of the holder state also show in CPU terms: `BenchmarkLockUnlockShared` went from ~12.9k–14.7k ns/op and 127 allocs/op before to ~20k–41k ns/op across seven runs (median of the five `-count 5` runs ~30k) and 170 allocs/op. What did not change: `CheckReservedLock` costs the same as before (zero calls while this file holds RESERVED or better locally, one GET otherwise), and a lock transition for a level already held still makes no lock API call.

## Go 1.27 and dependencies (issue #9)

The module moved from `go 1.19` to `go 1.27` and its dependencies from their
2023 versions, among them several `golang.org/x/*` releases with published
vulnerabilities. On go1.27.0 govulncheck still reported eight called
standard-library vulnerabilities (net/http, http2, crypto/tls,
mime/multipart), all fixed in go1.27.2, so the issue #28 follow-up raised
the module's floor to `go 1.27.2`. `govulncheck ./...` now reports no
vulnerabilities in this module.

| Module | Old | New |
| ------ | --- | --- |
| Go (language/module version) | 1.19 | 1.27.2 |
| `k8s.io/api` | v0.26.3 | v0.37.1 |
| `k8s.io/apimachinery` | v0.26.3 | v0.37.1 |
| `k8s.io/client-go` | v0.26.3 | v0.37.1 |
| `github.com/mattn/go-sqlite3` | v1.14.16 | v1.14.52 |
| `github.com/google/uuid` | v1.3.0 | v1.6.0 |
| `github.com/thought-machine/go-flags` | v1.6.2 | v1.7.0 |
| `github.com/psanford/sqlite3vfs` | v0.0.0-20230408… | v0.0.0-20260519004904-f9180fa2acc9 |
| `go.uber.org/zap` | v1.24.0 | **removed** — replaced by `log/slog` |
| `golang.org/x/net`, `.../sys`, `.../text`, `.../time`, `.../term`, `.../oauth2` and the rest of the indirect set | 2023 releases | current releases (`golang.org/x/net` v0.61.0, `golang.org/x/sys` v0.49.0, …) |

`k8s.io/{api,apimachinery,client-go}` move together at the same version, as
they must. Everything else is at `latest`; `go mod tidy` is clean. Nothing
in the direct set had to be held back — the only dependency that went away is
zap.

### Follow-up (issue #28)

* **Go 1.27.2 floor.** `go.mod` requires `go 1.27.2` (see above:
  eight called standard-library vulnerabilities on 1.27.0, all fixed in
  1.27.2). `govulncheck ./...` reports nothing reachable.
* **The tools exit non-zero on failure.** `db-download` used to log failed
  reads, writes, size checks and byte comparisons and still exit 0 on a
  half-failed transfer (and ignored the `Seek` error between them), and
  `main` logged a failed insert and committed the rest of the transaction
  anyway. Every failure now returns up to `main`, which prints it to stderr
  and exits 1 — and because nothing calls `os.Exit` mid-run any more, the
  deferred `db.Close`/`f.Close` calls actually run on the error paths.
* **`file-create` opened the wrong file.** It opened the literal path `fn`
  instead of the variable `fn` holding the file's name, so the local half of
  its differential comparison always went to a stray file called `fn`.
* **Tests.** `main`, `db-upload` and `db-download` are covered by tests
  driven with the fake clientset (`go test -race ./...`): db-upload's stored
  sector bytes, db-download's transfer and its size/byte verification, and
  the failure paths — missing file, failed sector write, failed insert,
  failed commit, size mismatch, byte mismatch.

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
