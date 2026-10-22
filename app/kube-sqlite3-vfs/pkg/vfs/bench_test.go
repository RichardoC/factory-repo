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
	"strings"
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

// BenchmarkSQLiteInsert1000Rows inserts 1000 rows in a single committed
// transaction through database/sql, the module's own insert-heavy workload,
// with the in-memory journal that predates issue #68. One benchmark
// iteration is one full transaction; api-calls/op is the number of
// Kubernetes API calls that transaction costs. It is kept as the MEMORY
// baseline that the journal-mode matrix below is measured against.
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
	runInsertBenchmark(b, "MEMORY", 1)
}

// BenchmarkSQLiteInsert1000RowsJournalModes runs the same committed
// 1000-row insert workload under each SQLite journal mode — DELETE,
// TRUNCATE, PERSIST and MEMORY — for one committed transaction and for 10
// consecutive transactions (issue #120). One benchmark iteration is
// `transactions` consecutive committed 1000-row transactions; api-calls/op
// is the number of Kubernetes API calls that iteration costs, journal
// writes and the journal's post-commit deletion (DELETE) or truncation
// (TRUNCATE) included, and api-calls/tx restates it per single transaction
// for direct comparison with the 1-transaction sub-benchmarks.
//
// The 10-transaction variants are where TRUNCATE and PERSIST's
// per-transaction costs show up: both keep the journal file on the cluster,
// so every transaction after the first pays to reopen it (xAccess, xOpen)
// and to rewrite its header (xWrite, one UPDATE per touched sector), where
// DELETE pays one journal deletion at each commit and nothing between
// transactions, and MEMORY never touches the cluster for the journal at
// all. The measured numbers and the exact command are in the module README.
//
// Everything is deliberately identical across sub-benchmarks — the same 64
// KiB page_size before the first CREATE TABLE, the same CREATE TABLE before
// ClearActions, the same 1000-row prepared-insert transactions under
// ClearActions/len(fcs.Actions())/ReportMetric, and the same assertion that
// the database grew past one 64 KiB sector — so the sub-benchmarks'
// api-calls/op are directly comparable and the differences are exactly the
// journal costs. MEMORY/transactions=01 duplicates
// BenchmarkSQLiteInsert1000Rows and should reproduce its number.
func BenchmarkSQLiteInsert1000RowsJournalModes(b *testing.B) {
	for _, mode := range []string{"DELETE", "TRUNCATE", "PERSIST", "MEMORY"} {
		for _, transactions := range []int{1, 10} {
			b.Run(fmt.Sprintf("journal=%s/transactions=%02d", mode, transactions), func(b *testing.B) {
				runInsertBenchmark(b, mode, transactions)
			})
		}
	}
}

// runInsertBenchmark is the shared body of the insert benchmarks: it opens
// a sqlite database over the fake-clientset VFS with _journal=journalMode,
// and per iteration commits `transactions` consecutive 1000-row insert
// transactions, counting the Kubernetes API calls they cost. One iteration
// is one unit of work for api-calls/op: with transactions=1 that is one
// committed transaction, with transactions=10 it is the whole
// 10-transaction sequence.
func runInsertBenchmark(b *testing.B, journalMode string, transactions int) {
	const rows = 1000
	var calls int64
	for range b.N {
		b.StopTimer()
		v, fcs := newTestVFS(b)
		vfsName := registerUniqueVFS(b, v)
		fileName := newSQLFileName()
		db := openSQLDBWithJournal(b, vfsName, fileName, journalMode)
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
		// The journal mode must be what the caller asked for before the
		// timed work starts: a mode SQLite silently ignored would make the
		// whole matrix report the same number under different names.
		var reportedMode string
		if err := db.QueryRow("PRAGMA journal_mode").Scan(&reportedMode); err != nil {
			b.Fatalf("PRAGMA journal_mode: %v", err)
		}
		// SQLite reports the pragma value lower-case ("delete"), so the
		// comparison is case-insensitive.
		if !strings.EqualFold(reportedMode, journalMode) {
			b.Fatalf("PRAGMA journal_mode = %s, want %s", reportedMode, journalMode)
		}
		fcs.ClearActions()
		b.StartTimer()

		for t := 0; t < transactions; t++ {
			tx, err := db.Begin()
			if err != nil {
				b.Fatalf("Begin: %v", err)
			}
			stmt, err := tx.Prepare("INSERT INTO books (id, title) VALUES (?, ?)")
			if err != nil {
				b.Fatalf("Prepare: %v", err)
			}
			// The primary key includes the transaction index: the rows of
			// transaction 2 must not collide with transaction 1's.
			for r := 0; r < rows; r++ {
				if _, err := stmt.Exec(fmt.Sprintf("r-%02d-%06d", t, r), fmt.Sprintf("title %02d %06d %0178d", t, r, r)); err != nil {
					b.Fatalf("insert %d: %v", r, err)
				}
			}
			if err := stmt.Close(); err != nil {
				b.Fatalf("stmt.Close: %v", err)
			}
			if err := tx.Commit(); err != nil {
				b.Fatalf("Commit: %v", err)
			}
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
	if transactions > 1 {
		b.ReportMetric(float64(calls)/(float64(b.N)*float64(transactions)), "api-calls/tx")
	}
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
