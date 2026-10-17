package vfs

import (
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/psanford/sqlite3vfs"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// This file holds the crash-recovery tests for issue #68: a rollback journal
// stored through this VFS must make a mid-transaction crash recoverable. They
// drive real SQLite across the cgo boundary over the fake clientset, with TWO
// VFS instances over ONE clientset standing in for two processes — the crashed
// holder and the reader that recovers from what it left behind.
//
// Two details make the mid-transaction state reachable and the tests honest:
//
//   - A small page cache (PRAGMA cache_size) plus a transaction that REUSES
//     the committed pages (a DELETE followed by inserts) makes SQLite spill
//     the uncommitted changes onto the cluster, so the database sectors at the
//     crash are genuinely modified by a transaction that never committed. A
//     transaction that only appends pages can leave the committed image
//     intact and the test vacuous.
//   - The holder is "killed" by abandoning its open transaction and the
//     *sql.DB without Commit, Rollback or Close, so no Unlock is ever sent;
//     the lease it took is then allowed to lapse by advancing the VFS clock,
//     never by sleeping.

// discardLogger is the silent logger every VFS here is built with.
func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// blindJournalVFS is a vfs whose Access answers the pre-#68 blanket "sqlite
// side files never exist" for "-journal" names. It is the fail-without-the-
// change control: the journal's ConfigMaps are on the cluster, but SQLite is
// told there is nothing there, so it never asks whether the journal is hot and
// accepts a half-written transaction as committed.
type blindJournalVFS struct{ *vfs }

func (b blindJournalVFS) Access(name string, flags sqlite3vfs.AccessFlag) (bool, error) {
	if strings.HasSuffix(name, "-journal") {
		return false, nil
	}
	return b.vfs.Access(name, flags)
}

func journalSectorConfigMaps(tb testing.TB, fcs *fake.Clientset, dbName string) []v1.ConfigMap {
	tb.Helper()
	return sectorConfigMapsNamed(tb, fcs, dbName+"-journal")
}

func databaseSectorConfigMaps(tb testing.TB, fcs *fake.Clientset, dbName string) []v1.ConfigMap {
	tb.Helper()
	return sectorConfigMapsNamed(tb, fcs, dbName)
}

// sectorConfigMapsNamed returns the sector ConfigMaps whose stored "filename"
// data key names the given file. Journal and database sectors carry identical
// names and labels; only that data key distinguishes them, so this is the only
// way to see the journal's sectors.
func sectorConfigMapsNamed(tb testing.TB, fcs *fake.Clientset, name string) []v1.ConfigMap {
	tb.Helper()
	all, err := fcs.CoreV1().ConfigMaps(testNamespace).List(tb.Context(), metav1.ListOptions{})
	if err != nil {
		tb.Fatalf("list configmaps: %v", err)
	}
	var out []v1.ConfigMap
	for _, cm := range all.Items {
		if cm.Data["filename"] == name {
			out = append(out, cm)
		}
	}
	return out
}

func sectorNames(cms []v1.ConfigMap) []string {
	out := make([]string, 0, len(cms))
	for _, cm := range cms {
		out = append(out, cm.Name)
	}
	return out
}

// journalMagicByte returns the byte at offset 0 of the rollback journal as
// stored on the cluster: the magic byte SQLite's hasHotJournal reads first. A
// non-zero value there is what makes SQLite treat the journal as hot and go on
// to play it back. It fatals if the journal has no stored sector 0.
func journalMagicByte(tb testing.TB, v *vfs, fcs *fake.Clientset, dbName string) byte {
	tb.Helper()
	cms := journalSectorConfigMaps(tb, fcs, dbName)
	if len(cms) == 0 {
		tb.Fatalf("journal %q has no sector ConfigMaps, so its magic byte cannot be read", dbName+"-journal")
	}
	// Sector 0 holds file offset 0. Its name is deterministic, so ask the VFS
	// for it rather than guessing from list order.
	want := NewFile(dbName+"-journal", v).sectorNameFromSectorIndex(0)
	for _, cm := range cms {
		if cm.Name != want {
			continue
		}
		b := cm.BinaryData["sector"]
		if len(b) == 0 {
			tb.Fatalf("journal sector 0 ConfigMap %q carries no bytes", cm.Name)
		}
		return b[0]
	}
	tb.Fatalf("journal sector 0 ConfigMap %q not found among journal sectors %v", want, sectorNames(cms))
	return 0
}

// crashDB models a killed process holding db open: the connection carrying the
// open write transaction, and with it SQLite's file handles, stays exactly as
// it is, and nothing calls Commit, Rollback, Close or Unlock. The dead
// holder's lease is left to lapse on the cluster rather than being released.
// The caller keeps db and tx referenced past this point so Go's finalizers do
// not perform the orderly teardown a crash does not have.
func crashDB(tb testing.TB, db *sql.DB, tx *sql.Tx) {
	tb.Helper()
	// Both handles are abandoned open: the transaction never committed or
	// rolled back, and neither db nor tx is closed. Assigning to the blank
	// identifier is exactly "the process died with these open".
	_ = db
	_ = tx
}

// openJournalDB opens dbName through a fresh VFS instance over fcs, in DELETE
// journal mode (the mode main.go and the cmd tools now default to), with a
// small page cache so a transaction that reuses committed pages spills them.
func openJournalDB(tb testing.TB, fcs *fake.Clientset, dbName string, clock *fakeClock) (*sql.DB, *vfs, string) {
	tb.Helper()
	v := NewVFS(tb.Context(), fcs, testNamespace, discardLogger(), 3, WithLockLease(testLockLease))
	v.clock = clock.Now
	name := registerUniqueVFS(tb, v)
	db := openSQLDBWithJournal(tb, name, dbName, "DELETE")
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA cache_size=-8"); err != nil {
		tb.Fatalf("PRAGMA cache_size: %v", err)
	}
	return db, v, name
}

func integrityCheck(tb testing.TB, db *sql.DB) string {
	tb.Helper()
	var got string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&got); err != nil {
		tb.Fatalf("PRAGMA integrity_check: %v", err)
	}
	return got
}

func countRows(tb testing.TB, db *sql.DB) int {
	tb.Helper()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM books").Scan(&n); err != nil {
		tb.Fatalf("SELECT COUNT(*) FROM books: %v", err)
	}
	return n
}

func countUncommitted(tb testing.TB, db *sql.DB) int {
	tb.Helper()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM books WHERE title LIKE 'uncommitted%'").Scan(&n); err != nil {
		tb.Fatalf("count uncommitted rows: %v", err)
	}
	return n
}

const (
	journalSeedRows     = 100 // rows committed before the crash
	journalCrashedRows  = 200 // rows the interrupted transaction inserts
	journalRowPadding   = 200 // bytes per title: makes the pages spill
	journalTitlePattern = "committed %d"
)

// seedCommittedTable creates the books table and commits journalSeedRows rows
// through db. This is the pre-transaction state that must survive every crash.
func seedCommittedTable(tb testing.TB, db *sql.DB) {
	tb.Helper()
	if _, err := db.Exec(`CREATE TABLE books (id INTEGER PRIMARY KEY, title TEXT)`); err != nil {
		tb.Fatalf("CREATE TABLE: %v", err)
	}
	tx, err := db.Begin()
	if err != nil {
		tb.Fatalf("seed Begin: %v", err)
	}
	stmt, err := tx.Prepare("INSERT INTO books (id, title) VALUES (?, ?)")
	if err != nil {
		tb.Fatalf("seed Prepare: %v", err)
	}
	pad := strings.Repeat("c", journalRowPadding)
	for i := 1; i <= journalSeedRows; i++ {
		if _, err := stmt.Exec(i, fmt.Sprintf(journalTitlePattern, i)+pad); err != nil {
			tb.Fatalf("seed insert %d: %v", i, err)
		}
	}
	if err := stmt.Close(); err != nil {
		tb.Fatalf("seed stmt.Close: %v", err)
	}
	if err := tx.Commit(); err != nil {
		tb.Fatalf("seed Commit: %v", err)
	}
}

// interruptWriteTransaction opens a write transaction, deletes every committed
// row, and inserts journalCrashedRows uncommitted rows. Deleting the committed
// rows is what forces SQLite to modify the sectors the committed image uses, so
// the crash leaves a database that is genuinely half-written. The transaction
// is returned uncommitted for the caller to abandon.
func interruptWriteTransaction(tb testing.TB, db *sql.DB) *sql.Tx {
	tb.Helper()
	tx, err := db.Begin()
	if err != nil {
		tb.Fatalf("interrupted Begin: %v", err)
	}
	if _, err := tx.Exec("DELETE FROM books"); err != nil {
		tb.Fatalf("interrupted DELETE: %v", err)
	}
	stmt, err := tx.Prepare("INSERT INTO books (id, title) VALUES (?, ?)")
	if err != nil {
		tb.Fatalf("interrupted Prepare: %v", err)
	}
	pad := strings.Repeat("u", journalRowPadding)
	for i := 0; i < journalCrashedRows; i++ {
		id := 100000 + i
		if _, err := stmt.Exec(id, fmt.Sprintf("uncommitted %d", id)+pad); err != nil {
			tb.Fatalf("interrupted insert %d: %v", id, err)
		}
	}
	if err := stmt.Close(); err != nil {
		tb.Fatalf("interrupted stmt.Close: %v", err)
	}
	return tx
}

// TestCrashAfterCompleteJournalRollsBackOnReopen is issue #68's case 1: the
// process dies AFTER SQLite has written the complete rollback journal and
// modified at least one database sector, but BEFORE the transaction commits. A
// fresh VFS instance over the same fake clientset must find the journal hot
// (its sectors exist and its magic byte is non-zero), play it back, and present
// the pre-transaction database intact — the committed rows there, the
// uncommitted rows gone, and the journal's sectors cleaned up afterwards.
func TestCrashAfterCompleteJournalRollsBackOnReopen(t *testing.T) {
	fcs := fake.NewSimpleClientset()
	dbName := newSQLFileName()
	clock := &fakeClock{now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}

	db, crasher, _ := openJournalDB(t, fcs, dbName, clock)
	seedCommittedTable(t, db)
	tx := interruptWriteTransaction(t, db)

	// THE MID-TRANSACTION STATE, read from the fake clientset: the journal's
	// sectors and the database's modified sectors are both on the "disk", and
	// the journal's magic byte at offset 0 is non-zero, which is what makes
	// SQLite's hasHotJournal treat it as hot.
	journal := journalSectorConfigMaps(t, fcs, dbName)
	if len(journal) == 0 {
		t.Fatalf("no journal sector ConfigMaps mid-transaction: the rollback journal is not stored through the VFS")
	}
	if got := len(databaseSectorConfigMaps(t, fcs, dbName)); got == 0 {
		t.Fatalf("no database sector ConfigMaps mid-transaction")
	}
	if b := journalMagicByte(t, crasher, fcs, dbName); b == 0 {
		t.Fatalf("journal magic byte at offset 0 is zero: hasHotJournal would skip it, so the journal can never be hot")
	}

	// KILL THE PROCESS: abandon the transaction and the connection with no
	// Commit, Rollback, Close or Unlock.
	crashDB(t, db, tx)

	// Let the dead holder's lease lapse — with the clock, not a sleep. Once it
	// has passed, the recovering instance's first lock CAS prunes the dead
	// writer and can take the lock.
	clock.Advance(testLockLease + time.Second)

	// RECOVER through a fresh VFS #2 over the same clientset. Its first read
	// walks SQLite's PAGER_OPEN -> hasHotJournal path, sees the journal's
	// existing sectors and non-zero magic, plays the journal back (restoring
	// the pre-transaction image), and only then reads.
	recovered, _, _ := openJournalDB(t, fcs, dbName, clock)
	defer recovered.Close()

	if got := integrityCheck(t, recovered); got != "ok" {
		t.Errorf("PRAGMA integrity_check after crash-and-recover = %q, want %q", got, "ok")
	}
	if got := countRows(t, recovered); got != journalSeedRows {
		t.Errorf("rows after recovery = %d, want %d (the uncommitted transaction must be rolled back)", got, journalSeedRows)
	}
	if got := countUncommitted(t, recovered); got != 0 {
		t.Errorf("uncommitted rows surviving recovery = %d, want 0", got)
	}
	// A committed row is intact, with its title as written.
	var title string
	if err := recovered.QueryRow("SELECT title FROM books WHERE id = 1").Scan(&title); err != nil {
		t.Fatalf("read committed row after recovery: %v", err)
	}
	if want := fmt.Sprintf(journalTitlePattern, 1) + strings.Repeat("c", journalRowPadding); title != want {
		t.Errorf("committed row 1 title = %q, want %q", title, want)
	}

	// The journal's sector ConfigMaps are gone once it has been played back:
	// SQLite unlinks the journal after a successful rollback.
	if left := journalSectorConfigMaps(t, fcs, dbName); len(left) != 0 {
		t.Errorf("journal sector ConfigMaps still on the cluster after recovery = %v, want none", sectorNames(left))
	}
}

// TestCrashBeforeCompleteJournalKeepsDataIntact is issue #68's case 2: the
// process dies with NO journal sectors stored, so there is nothing for SQLite
// to roll back — the database is simply the committed image, and reopening must
// give integrity_check ok with the pre-transaction data intact, nothing rolled
// back or lost. The unfinished journal is modelled by abandoning a transaction
// that has written nothing: SQLite creates no journal sectors for it.
func TestCrashBeforeCompleteJournalKeepsDataIntact(t *testing.T) {
	fcs := fake.NewSimpleClientset()
	dbName := newSQLFileName()
	clock := &fakeClock{now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}

	db, _, _ := openJournalDB(t, fcs, dbName, clock)
	seedCommittedTable(t, db)

	// A write transaction that has written nothing meaningful yet: the process
	// dies here, before any journal sector is stored.
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("unfinished Begin: %v", err)
	}
	if got := len(journalSectorConfigMaps(t, fcs, dbName)); got != 0 {
		t.Fatalf("journal already has %d sector ConfigMaps before the crash: the test does not model an unfinished journal", got)
	}

	crashDB(t, db, tx)
	clock.Advance(testLockLease + time.Second)

	recovered, _, _ := openJournalDB(t, fcs, dbName, clock)
	defer recovered.Close()

	if got := integrityCheck(t, recovered); got != "ok" {
		t.Errorf("PRAGMA integrity_check after a crash before the journal was written = %q, want %q", got, "ok")
	}
	if got := countRows(t, recovered); got != journalSeedRows {
		t.Errorf("rows after recovery = %d, want %d (nothing may be lost)", got, journalSeedRows)
	}
	if got := countUncommitted(t, recovered); got != 0 {
		t.Errorf("uncommitted rows after recovery = %d, want 0", got)
	}
}

// TestCrashMidTransactionWithoutStoredJournalLosesData is issue #68's case 3:
// the fail-without-the-change control that keeps the two tests above from being
// vacuous. It runs the SAME crash as case 1 and reopens with the same reader,
// but with the journal it cannot see or cannot use:
//
//   - blind: DELETE journal mode, but Access answers the pre-#68 blanket "a
//     -journal name never exists", so SQLite is told there is no hot journal
//     while the journal's ConfigMaps sit in the cluster;
//   - MEMORY: _journal=MEMORY, the mode main.go and the cmd tools used before
//     this change, whose journal never reaches the VFS at all.
//
// Both must lose or corrupt data — a non-ok integrity_check or the wrong row
// count — or this test fails and the premise of the whole change is wrong.
func TestCrashMidTransactionWithoutStoredJournalLosesData(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mode     string
		blind    bool
		wantCorr bool
	}{
		{name: "blind-blanket-Access-DELETE", mode: "DELETE", blind: true},
		{name: "in-memory-journal", mode: "MEMORY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fcs := fake.NewSimpleClientset()
			dbName := newSQLFileName()
			clock := &fakeClock{now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}

			crasher := NewVFS(t.Context(), fcs, testNamespace, discardLogger(), 3, WithLockLease(testLockLease))
			crasher.clock = clock.Now
			crasherName := registerUniqueVFS(t, crasher)
			db := openSQLDBWithJournal(t, crasherName, dbName, tc.mode)
			db.SetMaxOpenConns(1)
			if _, err := db.Exec("PRAGMA cache_size=-8"); err != nil {
				t.Fatalf("PRAGMA cache_size: %v", err)
			}
			seedCommittedTable(t, db)
			tx := interruptWriteTransaction(t, db)
			crashDB(t, db, tx)
			clock.Advance(testLockLease + time.Second)

			// Reopen. For "blind" the reader is the pre-change VFS but still in
			// DELETE mode, so the only difference from the passing test is the
			// Access answer.
			var recovered *sql.DB
			if tc.blind {
				blind := blindJournalVFS{crasher}
				blindName := registerUniqueVFS(t, blind)
				var err error
				recovered, err = sql.Open("sqlite3", fmt.Sprintf("file:%s?_journal=DELETE&vfs=%s", dbName, blindName))
				if err != nil {
					t.Fatalf("open blind reader: %v", err)
				}
				recovered.SetMaxOpenConns(1)
			} else {
				recovered, _, _ = openJournalDB(t, fcs, dbName, clock)
			}
			defer recovered.Close()

			check := integrityCheck(t, recovered)
			rows := countRows(t, recovered)
			uncommitted := countUncommitted(t, recovered)
			t.Logf("mode=%s blind=%v after a mid-transaction crash: integrity_check = %q, rows = %d, uncommitted = %d (want rows = %d, uncommitted = 0)",
				tc.mode, tc.blind, check, rows, uncommitted, journalSeedRows)

			if check == "ok" && rows == journalSeedRows && uncommitted == 0 {
				t.Errorf("with mode=%s blind=%v the database survived a mid-transaction crash intact (integrity_check %q, rows %d): this control no longer shows the missing journal losing data", tc.mode, tc.blind, check, rows)
			}
		})
	}
}
