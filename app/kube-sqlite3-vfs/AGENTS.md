# kube-sqlite3-vfs

SQLite VFS storing database files in Kubernetes ConfigMaps.

## Build and checks

- Building needs **cgo and a C compiler** (`mattn/go-sqlite3`); the Go version
  is exactly what `go.mod` says (`go 1.27.2`) — CI uses the version from
  `go.mod`, do not add a toolchain line or lower the floor.
- Run all four from the module root and leave them clean:
  `gofmt -l .`, `go vet ./...`, `go build ./...`, `go test -race ./...`.
- Tests must not need a live Kubernetes cluster.

## Integration tests

- `pkg/vfs/vfs_integration_test.go` sits behind the `//go:build integration`
  tag and runs against a real kube-apiserver via envtest. **CI does NOT run
  it**; plain `go test ./...` excludes it. To run locally you need the
  envtest binaries (`setup-envtest use 1.37.0 -p path`, see README) and
  `go test -race -count=1 -tags integration ./pkg/vfs`.

## Benchmarks

- Run `go test ./pkg/vfs -run ZZZ -bench . -count=5` (`-run ZZZ` matches no
  test) and compare **medians** of the runs, never single numbers: `ns/op`
  varies run to run; `api-calls/op` (reported via `b.ReportMetric` from the
  fake clientset's recorded actions, see `pkg/vfs/bench_test.go`) is the
  stable metric and is the number that matters. Back any performance claim
  with before and after numbers.

## Invariants a change must keep (one line each, from the README design sections)

- Lock transitions are compare-and-swap on the lock ConfigMap via
  `resourceVersion`; on 409 Conflict re-read and re-decide, never force.
- At most one writer (RESERVED/PENDING/EXCLUSIVE), EXCLUSIVE only once no
  SHARED holder remains; an impossible transition is `SQLITE_BUSY`, not a block.
- Caches are dropped before any downgrade is written, and a file never adopts
  the ConfigMap's stored state as its own lock.
- Journal delete order is fixed: LIST, sector 0 (the header) FIRST, remaining
  sectors ascending, the lockfile LAST — deleting the lockfile (or later
  sectors) while the header survives leaves a journal SQLite replays as hot
  and corrupts the database.
- The file advertises `SQLITE_IOCAP_SAFE_APPEND`; without it SQLite zeroes the
  journal magic and short-circuits the hot-journal protocol.
- `file.CheckReservedLock` deliberately returns the **negation** of the true
  answer to work around an inverted upstream cgo shim — do not "fix" it
  without flipping the pinned test expectations too.
- Sector writes are gated only by lease loss (`SQLITE_BUSY` after a prune),
  never by lock level; lease renewal rides `ReadAt`/`WriteAt` at zero extra
  API calls.

Tests use client-go's fake clientset; never a live cluster.
