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
// Every benchmark seeds its sectors up front, and the SQLite insert
// benchmark keeps the default 4 KiB page size with its workload inside one
// 64 KiB sector, so its numbers stay directly comparable with the
// pre-optimisation baseline.

import (
	"fmt"
	"k8s.io/client-go/kubernetes/fake"
	"math/rand"
	"testing"
)

// seedSectors fills a file of the given sector count with fillPattern data.
func seedSectors(tb testing.TB, fcs *fake.Clientset, f *file, sectors int) []byte {
	tb.Helper()
	data := make([]byte, int64(sectors)*SectorSize)
	fillPattern(data)
	for i := 0; i < sectors; i++ {
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
	for i := 0; i < b.N; i++ {
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
	rng := rand.New(rand.NewSource(1))
	buf := make([]byte, pageSize)
	var calls int64
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		off := int64(rng.Intn(pages)) * pageSize
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
	for i := 0; i < b.N; i++ {
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
// The default 4 KiB page size keeps the whole database inside one 64 KiB
// sector (asserted below), so this benchmark's api-calls/op stays directly
// comparable with the pre-optimisation baseline; a 64 KiB-page workload like
// main.go's is covered by TestSQLiteMultiSector64KiBPages.
func BenchmarkSQLiteInsert1000Rows(b *testing.B) {
	const rows = 1000
	var calls int64
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		v, fcs := newTestVFS(b)
		vfsName := registerUniqueVFS(b, v)
		fileName := newSQLFileName()
		db := openSQLDB(b, vfsName, fileName)
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
			if _, err := stmt.Exec(fmt.Sprintf("r-%06d", r), "t"); err != nil {
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
		if size > SectorSize {
			b.Fatalf("database grew to %d bytes, past the one-sector cap (%d) this benchmark pins to stay comparable with the pre-optimisation baseline", size, SectorSize)
		}
		if err := db.Close(); err != nil {
			b.Fatalf("db.Close: %v", err)
		}
	}
	b.ReportMetric(float64(calls)/float64(b.N), "api-calls/op")
}
