package vfs

// Benchmarks for the kube VFS against the fake kubernetes clientset.
//
// CPU time is not what hurts here: every sector operation turns into
// Kubernetes API round trips, and the fake clientset records exactly those
// as "actions". Each benchmark therefore wraps the operation under measure
// in fake.ClearActions() / len(fake.Actions()) and reports the average
// number of recorded API calls per operation alongside ns/op via
// b.ReportMetric("api-calls/op"). The before and after numbers for issue #8
// (both recorded with -benchtime=10x, before and after the optimisation) are
// tabulated in the module README.
//
// Every benchmark seeds its sectors up front. The SQLite insert benchmarks
// run at 64 KiB pages — the configuration main.go and the cmd tools use —
// and its transaction spans several 64 KiB sectors.

import (
	"fmt"
	"math/rand/v2"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/psanford/sqlite3vfs"
)

// seedSectors fills a file of the given sector count with fillPattern data.
func seedSectors(tb testing.TB, fcs *fake.Clientset, f *file, sectors int) []byte {
	tb.Helper()
	data := make([]byte, int64(sectors)*SectorSize)
	fillPattern(data)
	for i := range sectors {
		seedSector(tb, fcs, f, int64(i), data[int64(i)*SectorSize:(int64(i)+1)*SectorSize])
	}
	return data
}

// BenchmarkSequentialRead reads one 64 KiB sector at a time, wrapping around
// a four-sector file.
func BenchmarkSequentialRead(b *testing.B) {
	v, fcs := newTestVFS(b)
	f := openTestFile(b, v, "bench-seq-read.db")
	const sectors = 4
	seedSectors(b, fcs, f, sectors)

	buf := make([]byte, SectorSize)
	var calls int64
	b.ReportAllocs()
	for i := range b.N {
		off := int64(i%sectors) * SectorSize
		fcs.ClearActions()
		if _, err := f.ReadAt(buf, off); err != nil {
			b.Fatal(err)
		}
		calls += int64(len(fcs.Actions()))
	}
	b.ReportMetric(float64(calls)/float64(b.N), "api-calls/op")
}

// BenchmarkSequentialReadLocked reads one 64 KiB sector at a time while a
// SHARED lock is held: the sector cache is warm and FileSize is served from
// memory, so a read costs no Kubernetes API call at all. This is the steady
// state of a sqlite reader between write transactions, and it is exactly
// what the sector cache and the O(1) FileSize buy; the unlocked benchmark
// above pays the revalidating LIST plus the sector GET on every read.
func BenchmarkSequentialReadLocked(b *testing.B) {
	v, fcs := newTestVFS(b)
	f := openTestFile(b, v, "bench-seq-read-locked.db")
	const sectors = 4
	seedSectors(b, fcs, f, sectors)

	if err := f.Lock(sqlite3vfs.LockShared); err != nil {
		b.Fatal(err)
	}
	// One warm-up read outside the timed loop: it pays the revalidating
	// LIST that fills the cache once SHARED is held; every read after it
	// is served entirely from memory.
	buf := make([]byte, SectorSize)
	if _, err := f.ReadAt(buf, 0); err != nil {
		b.Fatal(err)
	}

	var calls int64
	b.ReportAllocs()
	for i := range b.N {
		off := int64(i%sectors) * SectorSize
		fcs.ClearActions()
		if _, err := f.ReadAt(buf, off); err != nil {
			b.Fatal(err)
		}
		calls += int64(len(fcs.Actions()))
	}
	b.ReportMetric(float64(calls)/float64(b.N), "api-calls/op")
}

// BenchmarkRandomPageRead reads random 4 KiB pages (sqlite's default page
// size) out of a 1 MiB file, with a fixed seed for reproducibility.
func BenchmarkRandomPageRead(b *testing.B) {
	v, fcs := newTestVFS(b)
	f := openTestFile(b, v, "bench-rand-read.db")
	const (
		sectors  = 16
		pageSize = 4096
	)
	seedSectors(b, fcs, f, sectors)

	pages := sectors * int(SectorSize) / pageSize
	rng := rand.New(rand.NewPCG(1, 1))
	buf := make([]byte, pageSize)
	var calls int64
	b.ReportAllocs()
	for range b.N {
		off := int64(rng.IntN(pages)) * pageSize
		fcs.ClearActions()
		if _, err := f.ReadAt(buf, off); err != nil {
			b.Fatal(err)
		}
		calls += int64(len(fcs.Actions()))
	}
	b.ReportMetric(float64(calls)/float64(b.N), "api-calls/op")
}

// BenchmarkSequentialWrite overwrites one full 64 KiB sector at a time,
// wrapping around a four-sector file.
func BenchmarkSequentialWrite(b *testing.B) {
	v, fcs := newTestVFS(b)
	f := openTestFile(b, v, "bench-seq-write.db")
	const sectors = 4
	seedSectors(b, fcs, f, sectors)

	buf := make([]byte, SectorSize)
	fillPatternAlt(buf)
	var calls int64
	b.ReportAllocs()
	for i := range b.N {
		off := int64(i%sectors) * SectorSize
		fcs.ClearActions()
		if _, err := f.WriteAt(buf, off); err != nil {
			b.Fatal(err)
		}
		calls += int64(len(fcs.Actions()))
	}
	b.ReportMetric(float64(calls)/float64(b.N), "api-calls/op")
}

// BenchmarkSQLiteInsert1000Rows inserts 1000 rows in a single transaction
// through database/sql, the module's own insert-heavy workload. One
// benchmark iteration is one full transaction; api-calls/op is the number of
// Kubernetes API calls that transaction costs.
//
// It runs at 64 KiB pages — SQLite's largest page size, exactly one VFS
// sector per page, and the configuration main.go and the cmd tools use — and
// its ~200-byte rows push the transaction across several 64 KiB sectors
// (asserted below), so the multi-sector write path is what gets measured.
// The pre-optimisation code panicked the moment a database outgrew its
// first sector, so this workload has no pre-optimisation baseline; the
// one-sector numbers the previous 4 KiB-page version of this benchmark
// produced are preserved in the module README.
func BenchmarkSQLiteInsert1000Rows(b *testing.B) {
	const rows = 1000
	var calls int64
	for range b.N {
		b.StopTimer()
		v, fcs := newTestVFS(b)
		vfsName := registerUniqueVFS(b, v)
		fileName := newSQLFileName()
		db := openSQLDB(b, vfsName, fileName)
		// 64 KiB pages, as main.go and the cmd tools use. page_size is only
		// honoured while the database is still empty, so it must come
		// before the first CREATE TABLE.
		if _, err := db.Exec("PRAGMA page_size=65536"); err != nil {
			b.Fatalf("PRAGMA page_size=65536: %v", err)
		}
		if _, err := db.Exec(`CREATE TABLE books (
			id text NOT NULL PRIMARY KEY,
			title text
		)`); err != nil {
			b.Fatalf("CREATE TABLE: %v", err)
		}
		fcs.ClearActions()
		b.StartTimer()

		tx, err := db.Begin()
		if err != nil {
			b.Fatalf("Begin: %v", err)
		}
		stmt, err := tx.Prepare("INSERT INTO books (id, title) VALUES (?, ?)")
		if err != nil {
			b.Fatalf("Prepare: %v", err)
		}
		for r := 0; r < rows; r++ {
			if _, err := stmt.Exec(fmt.Sprintf("r-%06d", r), fmt.Sprintf("title %06d %0180d", r, r)); err != nil {
				b.Fatalf("insert %d: %v", r, err)
			}
		}
		if err := stmt.Close(); err != nil {
			b.Fatalf("stmt.Close: %v", err)
		}
		if err := tx.Commit(); err != nil {
			b.Fatalf("Commit: %v", err)
		}
		calls += int64(len(fcs.Actions()))
		b.StopTimer()

		size, err := NewFile(fileName, v).FileSize()
		if err != nil {
			b.Fatalf("FileSize: %v", err)
		}
		if size <= SectorSize {
			b.Fatalf("database stayed at %d bytes: the transaction never left the first 64 KiB sector, so this benchmark would not span several sectors", size)
		}
		if err := db.Close(); err != nil {
			b.Fatalf("db.Close: %v", err)
		}
	}
	b.ReportMetric(float64(calls)/float64(b.N), "api-calls/op")
}

// BenchmarkSQLiteInsert1000RowsJournalDelete is BenchmarkSQLiteInsert1000Rows
// verbatim except for the SQLite journal mode: it opens the database with
// _journal=DELETE instead of _journal=MEMORY, so the transaction's rollback
// journal is stored through this VFS as its own "<db>-journal" file (a
// sector ConfigMap per 64 KiB) rather than in process memory. One benchmark
// iteration is one committed 1000-row transaction; api-calls/op is the
// number of Kubernetes API calls that transaction costs, journal writes and
// the journal's post-commit deletion included.
//
// Everything else is deliberately identical — the same 64 KiB page_size
// before the first CREATE TABLE, the same CREATE TABLE before
// ClearActions, the same 1000-row prepared-insert transaction under
// ClearActions/len(fcs.Actions())/ReportMetric("api-calls/op"), and the same
// assertion that the database grew past one 64 KiB sector — so the two
// benchmarks' api-calls/op are directly comparable and the difference is
// exactly what the on-cluster journal costs. The existing benchmark and its
// numbers are untouched: the DSN journal mode comes from a separate helper
// (openSQLDBWithJournal), not from a change to openSQLDB.
func BenchmarkSQLiteInsert1000RowsJournalDelete(b *testing.B) {
	const rows = 1000
	var calls int64
	for range b.N {
		b.StopTimer()
		v, fcs := newTestVFS(b)
		vfsName := registerUniqueVFS(b, v)
		fileName := newSQLFileName()
		db := openSQLDBWithJournal(b, vfsName, fileName, "DELETE")
		// 64 KiB pages, as main.go and the cmd tools use. page_size is only
		// honoured while the database is still empty, so it must come
		// before the first CREATE TABLE.
		if _, err := db.Exec("PRAGMA page_size=65536"); err != nil {
			b.Fatalf("PRAGMA page_size=65536: %v", err)
		}
		if _, err := db.Exec(`CREATE TABLE books (
			id text NOT NULL PRIMARY KEY,
			title text
		)`); err != nil {
			b.Fatalf("CREATE TABLE: %v", err)
		}
		fcs.ClearActions()
		b.StartTimer()

		tx, err := db.Begin()
		if err != nil {
			b.Fatalf("Begin: %v", err)
		}
		stmt, err := tx.Prepare("INSERT INTO books (id, title) VALUES (?, ?)")
		if err != nil {
			b.Fatalf("Prepare: %v", err)
		}
		for r := 0; r < rows; r++ {
			if _, err := stmt.Exec(fmt.Sprintf("r-%06d", r), fmt.Sprintf("title %06d %0180d", r, r)); err != nil {
				b.Fatalf("insert %d: %v", r, err)
			}
		}
		if err := stmt.Close(); err != nil {
			b.Fatalf("stmt.Close: %v", err)
		}
		if err := tx.Commit(); err != nil {
			b.Fatalf("Commit: %v", err)
		}
		calls += int64(len(fcs.Actions()))
		b.StopTimer()

		size, err := NewFile(fileName, v).FileSize()
		if err != nil {
			b.Fatalf("FileSize: %v", err)
		}
		if size <= SectorSize {
			b.Fatalf("database stayed at %d bytes: the transaction never left the first 64 KiB sector, so this benchmark would not span several sectors", size)
		}
		if err := db.Close(); err != nil {
			b.Fatalf("db.Close: %v", err)
		}
	}
	b.ReportMetric(float64(calls)/float64(b.N), "api-calls/op")
}

// BenchmarkLockUnlockShared measures the lock path itself: one
// Lock(Shared) transition and one Unlock(None) per iteration, with the
// api-calls/op metric counting the Kubernetes calls each full acquire +
// release cycle costs. Issue #47 replaces the lock ConfigMap's single
// unconditional write with a read-modify-write CAS (resourceVersion) that
// also carries holders and leases, so the lock path costs more calls per
// transition than it did; this benchmark is the before/after measure. Its
// issue #47 baseline (before the CAS change, run with -benchtime 10x) is
// tabulated in the module README.
func BenchmarkLockUnlockShared(b *testing.B) {
	v, fcs := newTestVFS(b)
	f := openTestFile(b, v, "bench-lock-unlock.db")

	var calls int64
	b.ReportAllocs()
	for range b.N {
		fcs.ClearActions()
		if err := f.Lock(sqlite3vfs.LockShared); err != nil {
			b.Fatal(err)
		}
		if err := f.Unlock(sqlite3vfs.LockNone); err != nil {
			b.Fatal(err)
		}
		calls += int64(len(fcs.Actions()))
	}
	b.ReportMetric(float64(calls)/float64(b.N), "api-calls/op")
}

// BenchmarkLockUnlockExclusive measures the write side of the lock path:
// one climb from a held SHARED lock to EXCLUSIVE — sqlite's SHARED ->
// (PENDING) -> EXCLUSIVE route, which issue #47 implements as two CAS
// steps against the lock ConfigMap — and one downgrade back to SHARED
// (Unlock below the held level), per iteration. Holding SHARED between
// iterations mirrors a sqlite writer between transactions. Each iteration
// is therefore three CAS transitions: two for the climb, one for the
// downgrade. Its issue #47 baseline is tabulated in the module README.
func BenchmarkLockUnlockExclusive(b *testing.B) {
	v, fcs := newTestVFS(b)
	f := openTestFile(b, v, "bench-lock-unlock-exclusive.db")
	if err := f.Lock(sqlite3vfs.LockShared); err != nil {
		b.Fatal(err)
	}

	var calls int64
	b.ReportAllocs()
	for range b.N {
		fcs.ClearActions()
		if err := f.Lock(sqlite3vfs.LockExclusive); err != nil {
			b.Fatal(err)
		}
		if err := f.Unlock(sqlite3vfs.LockShared); err != nil {
			b.Fatal(err)
		}
		calls += int64(len(fcs.Actions()))
	}
	b.ReportMetric(float64(calls)/float64(b.N), "api-calls/op")
}
