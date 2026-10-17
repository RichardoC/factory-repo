package vfs

// Concurrency tests for issue #47's mutual exclusion, run through
// database/sql exactly as two independent processes would contend: two VFS
// instances over ONE fake clientset (same namespace, so the same lockfile
// and sector ConfigMaps), each registered under its own name and backing
// exactly one SQLite connection, writing into the same database at the same
// time.
//
// The VFS reports a conflicting lock as sqlite3vfs.BusyError, which the
// sqlite3vfs cgo shim maps to SQLITE_BUSY (5); the DSN's _busy_timeout then
// makes SQLite's busy handler sleep and retry the failed lock transition.
// SQLITE_BUSY can still escape the handler — e.g. on BEGIN or COMMIT, where
// SQLite reports rather than retries — so each writer's batch is wrapped in
// the bounded busy retry below; the retry re-runs the whole batch, which is
// safe because a failed transaction rolled back and every id belongs to
// exactly one batch.
//
// Everything here runs against the fake clientset: no cluster, like every
// test in this package.

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
	"github.com/psanford/sqlite3vfs"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// testLockLease is deliberately shorter than the 30s default so a lease bug
// (a holder whose entries never renew, or one pruned while alive) cannot
// hide behind the default, yet several times wider than the 5s busy timeout
// so no live holder's entries expire mid-contention: casLock re-stamps a
// holder's leases only on a successful CAS, so a connection stuck in the
// busy handler does not renew its own entries and must not run out of lease
// while it waits.
const testLockLease = 15 * time.Second

// configMapGVR is the GroupVersionResource of the ConfigMaps every VFS
// object is stored in.
var configMapGVR = corev1.SchemeGroupVersion.WithResource("configmaps")

// newArbitratingFakeClientset returns a fake clientset whose ConfigMap
// writes carry the optimistic-concurrency semantics of a real API server.
//
// The bare fake cannot arbitrate concurrent compare-and-swap loops: its
// tracker never surfaces a resourceVersion on Get and accepts every Update
// unconditionally, so two racing casLock loops can interleave as
// Get -> Get -> Update -> Update and the second write silently clobbers the
// first — a lost update no real API server would allow, and one that lets
// two processes both believe they hold the lock (observed as a reader
// acquiring SHARED under another instance's EXCLUSIVE). Issue #47's
// locking is BUILT on the API server's 409-on-stale-resourceVersion, so
// the reactors below restore exactly that behaviour:
//
//   - get stamps the returned ConfigMap with the current resourceVersion of
//     its name;
//   - update accepts a write only when it carries no resourceVersion (the
//     sector-write path, an unconditional put on a real server too) or the
//     current one; a stale one is rejected with the same 409 Conflict the
//     API server returns, which casLock re-reads and re-decides on;
//   - create refuses an existing name with AlreadyExists, as the API server
//     does (casLock treats that as "re-read").
//
// The resourceVersions live in a per-name counter guarded by a mutex, which
// is what makes two concurrent writes to one ConfigMap strictly ordered.
func newArbitratingFakeClientset(tb testing.TB) *fake.Clientset {
	tb.Helper()
	fcs := fake.NewSimpleClientset()
	var mu sync.Mutex
	currentRV := make(map[string]int)

	nextRV := func(name string) string {
		currentRV[name]++
		return strconv.Itoa(currentRV[name])
	}

	fcs.PrependReactor("get", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		ga := action.(k8stesting.GetAction)
		obj, err := fcs.Tracker().Get(configMapGVR, action.GetNamespace(), ga.GetName())
		if err != nil {
			return true, nil, err
		}
		cm := obj.(*corev1.ConfigMap).DeepCopy()
		mu.Lock()
		cm.ResourceVersion = strconv.Itoa(currentRV[ga.GetName()])
		mu.Unlock()
		return true, cm, nil
	})

	fcs.PrependReactor("create", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		ca := action.(k8stesting.CreateAction)
		cm := ca.GetObject().(*corev1.ConfigMap).DeepCopy()
		// A real API server takes the namespace from the request path, not
		// the body; some VFS-built ConfigMaps carry none.
		cm.Namespace = action.GetNamespace()
		mu.Lock()
		defer mu.Unlock()
		if _, err := fcs.Tracker().Get(configMapGVR, action.GetNamespace(), cm.Name); err == nil {
			return true, nil, kerrors.NewAlreadyExists(configMapGVR.GroupResource(), cm.Name)
		}
		cm.ResourceVersion = nextRV(cm.Name)
		if err := fcs.Tracker().Add(cm); err != nil {
			return true, nil, err
		}
		return true, cm, nil
	})

	fcs.PrependReactor("update", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		ua := action.(k8stesting.UpdateAction)
		cm := ua.GetObject().(*corev1.ConfigMap).DeepCopy()
		mu.Lock()
		defer mu.Unlock()
		if cm.ResourceVersion != "" && cm.ResourceVersion != strconv.Itoa(currentRV[cm.Name]) {
			return true, nil, kerrors.NewConflict(configMapGVR.GroupResource(), cm.Name,
				fmt.Errorf("the object has been modified: resourceVersion %q is stale, current is %d", cm.ResourceVersion, currentRV[cm.Name]))
		}
		cm.ResourceVersion = nextRV(cm.Name)
		cm.Namespace = action.GetNamespace()
		if err := fcs.Tracker().Update(configMapGVR, cm, action.GetNamespace()); err != nil {
			return true, nil, err
		}
		return true, cm, nil
	})

	return fcs
}

// newContendingVFSInstances returns two VFS instances over one arbitrating
// fake clientset (see newArbitratingFakeClientset for why the reactors are
// needed). Shared storage, distinct VFS registrations: what two separate
// processes see.
func newContendingVFSInstances(tb testing.TB) (*vfs, *vfs) {
	tb.Helper()
	fcs := newArbitratingFakeClientset(tb)
	logger := slog.New(slog.DiscardHandler)
	v1 := NewVFS(tb.Context(), fcs, testNamespace, logger, 3, WithLockLease(testLockLease))
	v2 := NewVFS(tb.Context(), fcs, testNamespace, logger, 3, WithLockLease(testLockLease))
	return v1, v2
}

// openContendingSQLDB opens one sqlite connection through the named VFS,
// with the busy timeout that lets SQLite wait out the other instance's
// locks. One sql.DB with SetMaxOpenConns(1) is exactly one sqlite
// connection, so two of these are two contending processes.
func openContendingSQLDB(tb testing.TB, vfsName, fileName string) *sql.DB {
	tb.Helper()
	dsn := fmt.Sprintf("file:%s?_journal=MEMORY&vfs=%s&_busy_timeout=5000", fileName, vfsName)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		tb.Fatalf("sql.Open(%q) failed: %v", dsn, err)
	}
	db.SetMaxOpenConns(1)
	return db
}

// isBusyError reports whether err is SQLite's SQLITE_BUSY (5): the driver
// surfaces the VFS's sqlite3vfs.BusyError as a sqlite3.Error carrying that
// code — as the base code or inside an extended code — and the VFS's own
// BusyError value is matched directly should it ever escape unwrapped.
func isBusyError(err error) bool {
	if errors.Is(err, sqlite3vfs.BusyError) {
		return true
	}
	var sqliteErr sqlite3.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	return sqliteErr.Code == sqlite3.ErrBusy ||
		sqlite3.ErrNo(sqliteErr.ExtendedCode)&sqlite3.ErrNo(sqlite3.ErrNoMask) == sqlite3.ErrBusy
}

// insertBatchWithBusyRetry runs one writer's whole batch — BEGIN, rows
// inserts, COMMIT — retrying the batch a bounded number of times while
// SQLite reports SQLITE_BUSY. The DSN's busy timeout already makes SQLite
// wait out conflicting locks inside the busy handler, but BUSY can still be
// reported past it (notably on BEGIN and COMMIT, which race the other
// connection's lock transitions), so the batch retries from scratch: the
// failed transaction rolled back, and the id space is partitioned between
// the writers, so a re-run cannot double-insert. The bound keeps a hopeless
// contention bug a fast, explicit failure rather than a hang.
func insertBatchWithBusyRetry(t *testing.T, db *sql.DB, prefix string, rows int) {
	t.Helper()
	const attempts = 20
	const retryDelay = 50 * time.Millisecond
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		err = insertBatchOnce(db, prefix, rows)
		if err == nil {
			return
		}
		if !isBusyError(err) {
			t.Errorf("insert batch %q: %v", prefix, err)
			return
		}
		if attempt < attempts {
			time.Sleep(retryDelay)
		}
	}
	t.Errorf("insert batch %q: still SQLITE_BUSY after %d bounded attempts: %v", prefix, attempts, err)
}

// insertBatchOnce performs one attempt of a writer's batch as one
// transaction. Any failure — busy or otherwise — rolls the transaction back
// before returning, releasing every lock the attempt held.
func insertBatchOnce(db *sql.DB, prefix string, rows int) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare("INSERT INTO books (id, title) VALUES (?, ?)")
	if err != nil {
		tx.Rollback()
		return err
	}
	for i := 0; i < rows; i++ {
		if _, err := stmt.Exec(fmt.Sprintf("%s-%03d", prefix, i), fmt.Sprintf("title %s %d", prefix, i)); err != nil {
			stmt.Close()
			tx.Rollback()
			return err
		}
	}
	if err := stmt.Close(); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		tx.Rollback()
		return err
	}
	return nil
}

// expectedBooks maps the ids writer prefix must have committed to the
// titles they must carry: rows prefix-000 .. prefix-(rows-1).
func expectedBooks(prefix string, rows int) map[string]string {
	want := make(map[string]string, rows)
	for i := 0; i < rows; i++ {
		want[fmt.Sprintf("%s-%03d", prefix, i)] = fmt.Sprintf("title %s %d", prefix, i)
	}
	return want
}

// assertDatabaseConsistent checks, over ONE connection, that the database
// both writers committed into is intact and complete: integrity_check says
// exactly "ok", the row count matches, and the id/title set is exactly the
// union of the two writers' rows — every committed row present, none lost
// or duplicated by the concurrent writes.
func assertDatabaseConsistent(t *testing.T, db *sql.DB, wantIDs map[string]string) {
	t.Helper()

	// PRAGMA integrity_check returns one "ok" row, or one row per problem.
	check, err := db.Query("PRAGMA integrity_check")
	if err != nil {
		t.Fatalf("PRAGMA integrity_check: %v", err)
	}
	var verdicts []string
	for check.Next() {
		var verdict string
		if err := check.Scan(&verdict); err != nil {
			t.Fatalf("scan integrity_check row: %v", err)
		}
		verdicts = append(verdicts, verdict)
	}
	if err := check.Err(); err != nil {
		t.Fatalf("PRAGMA integrity_check rows: %v", err)
	}
	check.Close()
	if len(verdicts) != 1 || verdicts[0] != "ok" {
		t.Fatalf("PRAGMA integrity_check = %v, want exactly [ok]", verdicts)
	}

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM books").Scan(&count); err != nil {
		t.Fatalf("COUNT(*): %v", err)
	}
	if count != len(wantIDs) {
		t.Errorf("COUNT(*) = %d, want %d", count, len(wantIDs))
	}

	got := make(map[string]string, count)
	rows, err := db.Query("SELECT id, title FROM books")
	if err != nil {
		t.Fatalf("SELECT id, title: %v", err)
	}
	for rows.Next() {
		var id, title string
		if err := rows.Scan(&id, &title); err != nil {
			t.Fatalf("scan row: %v", err)
		}
		if _, dup := got[id]; dup {
			t.Errorf("row %q appears more than once", id)
		}
		got[id] = title
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	rows.Close()

	for id, wantTitle := range wantIDs {
		gotTitle, ok := got[id]
		if !ok {
			t.Errorf("committed row %q is missing from the database", id)
			continue
		}
		if gotTitle != wantTitle {
			t.Errorf("row %q = title %q, want %q", id, gotTitle, wantTitle)
		}
	}
	for id := range got {
		if _, ok := wantIDs[id]; !ok {
			t.Errorf("unexpected row %q in the database", id)
		}
	}
}

// TestConcurrentWritersTwoInstancesKeepDatabaseConsistent is the issue #47
// proof: two processes' connections write concurrently through the new
// holder/CAS/lease locking, and the database comes out consistent — the
// writers' transactions excluded each other instead of interleaving their
// sector writes.
func TestConcurrentWritersTwoInstancesKeepDatabaseConsistent(t *testing.T) {
	v1, v2 := newContendingVFSInstances(t)
	vfs1Name := registerUniqueVFS(t, v1)
	vfs2Name := registerUniqueVFS(t, v2)
	fileName := newSQLFileName()

	dbA := openContendingSQLDB(t, vfs1Name, fileName)
	defer dbA.Close()
	dbB := openContendingSQLDB(t, vfs2Name, fileName)
	defer dbB.Close()

	// The schema exists before either writer goroutine starts, so the
	// concurrency under test is purely the two insert transactions.
	if _, err := dbA.Exec(`CREATE TABLE books (
		id text NOT NULL PRIMARY KEY,
		title text
	)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}

	const rowsPerWriter = 50
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		insertBatchWithBusyRetry(t, dbA, "a", rowsPerWriter)
	}()
	go func() {
		defer wg.Done()
		<-start
		insertBatchWithBusyRetry(t, dbB, "b", rowsPerWriter)
	}()
	// Both goroutines are released before either can finish: each one's
	// whole batch runs only after this close, so the two transactions
	// contend on the lockfile rather than queueing behind each other.
	close(start)
	wg.Wait()
	if t.Failed() {
		return
	}

	want := expectedBooks("a", rowsPerWriter)
	for id, title := range expectedBooks("b", rowsPerWriter) {
		want[id] = title
	}
	// One connection performs every check, so the asserted state is one
	// reader's view of the settled database.
	assertDatabaseConsistent(t, dbA, want)
}

// TestConcurrentReaderSeesConsistentDataWhileWriterHoldsTx pins the reader
// half of SQLite's chart through the new locking: while one instance's
// connection holds a writer transaction (RESERVED, uncommitted), the other
// instance still takes SHARED and reads a consistent snapshot — integrity
// ok, uncommitted rows invisible — and sees them the moment the commit
// lands. The two instances never block here: readers are allowed under a
// RESERVED writer, which is what keeps this test cheap and deterministic.
func TestConcurrentReaderSeesConsistentDataWhileWriterHoldsTx(t *testing.T) {
	v1, v2 := newContendingVFSInstances(t)
	vfs1Name := registerUniqueVFS(t, v1)
	vfs2Name := registerUniqueVFS(t, v2)
	fileName := newSQLFileName()

	dbA := openContendingSQLDB(t, vfs1Name, fileName)
	defer dbA.Close()
	dbB := openContendingSQLDB(t, vfs2Name, fileName)
	defer dbB.Close()

	if _, err := dbA.Exec(`CREATE TABLE books (
		id text NOT NULL PRIMARY KEY,
		title text
	)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	const seededRows = 10
	insertBatchWithBusyRetry(t, dbA, "a", seededRows)
	if t.Failed() {
		return
	}

	// A writer transaction that stays open: its rows are committed only
	// after the reader below has looked twice.
	const heldRows = 5
	tx, err := dbA.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	stmt, err := tx.Prepare("INSERT INTO books (id, title) VALUES (?, ?)")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	for i := seededRows; i < seededRows+heldRows; i++ {
		if _, err := stmt.Exec(fmt.Sprintf("a-%03d", i), fmt.Sprintf("title a %d", i)); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	if err := stmt.Close(); err != nil {
		t.Fatalf("stmt.Close: %v", err)
	}

	// The reader on the other instance must read cleanly under the open
	// writer transaction, and must not see the uncommitted rows.
	readerCount := func() int {
		t.Helper()
		var n int
		if err := dbB.QueryRow("SELECT COUNT(*) FROM books").Scan(&n); err != nil {
			t.Fatalf("reader COUNT(*) while writer holds its transaction: %v", err)
		}
		return n
	}
	if n := readerCount(); n != seededRows {
		t.Errorf("reader COUNT(*) during writer tx = %d, want %d (uncommitted rows must be invisible)", n, seededRows)
	}
	checkConsistent := func() {
		t.Helper()
		check, err := dbB.Query("PRAGMA integrity_check")
		if err != nil {
			t.Fatalf("reader PRAGMA integrity_check during writer tx: %v", err)
		}
		var verdicts []string
		for check.Next() {
			var verdict string
			if err := check.Scan(&verdict); err != nil {
				t.Fatalf("scan integrity_check row: %v", err)
			}
			verdicts = append(verdicts, verdict)
		}
		if err := check.Err(); err != nil {
			t.Fatalf("PRAGMA integrity_check rows: %v", err)
		}
		check.Close()
		if len(verdicts) != 1 || verdicts[0] != "ok" {
			t.Errorf("reader PRAGMA integrity_check during writer tx = %v, want exactly [ok]", verdicts)
		}
	}
	checkConsistent()

	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if n := readerCount(); n != seededRows+heldRows {
		t.Errorf("reader COUNT(*) after commit = %d, want %d", n, seededRows+heldRows)
	}
}

// openRollbackJournalSQLDB opens one sqlite connection through the named
// VFS in sqlite's default DELETE journal mode — with a rollback journal on
// the "disk", which is what makes hasHotJournal (and with it
// xCheckReservedLock) part of every reader's first lock. No busy timeout:
// a reader of an uncommitted transaction must not fail at all here, and a
// failure should surface immediately rather than after handler retries.
func openRollbackJournalSQLDB(tb testing.TB, vfsName, fileName string) *sql.DB {
	tb.Helper()
	dsn := fmt.Sprintf("file:%s?vfs=%s", fileName, vfsName)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		tb.Fatalf("sql.Open(%q) failed: %v", dsn, err)
	}
	db.SetMaxOpenConns(1)
	return db
}

// TestCheckReservedLockCrossesCGOBoundary pins the CheckReservedLock answer
// END TO END, through sqlite itself: two connections through two registered
// VFS instances over one fake clientset, writer A in DELETE journal mode
// holding an open write transaction (RESERVED, journal on the "disk"),
// reader B's first read reaching hasHotJournal — the only SQLite consumer
// of xCheckReservedLock — which must learn, across the cgo boundary, that a
// live writer holds the database and leave A's journal alone.
//
// Upstream github.com/psanford/sqlite3vfs inverts the answer at the shim
// (goVFSCheckReservedLock writes *pResOut = 0 for a Go true), which is why
// file.CheckReservedLock returns the negation. Without that workaround the
// answer SQLite consumes here is "no writer" while A's journal exists: B
// judges the journal hot and tries to play it back, which from B's
// unlocked state means an EXCLUSIVE lock the VFS refuses — B's plain SELECT
// fails while A merely holds an uncommitted transaction. This test fails
// the moment the negation is removed (until upstream maps the answer
// straight through), which is the point: in-process asserts cannot see
// what SQLite finally consumes.
//
// TWO conditions make that failure observable. The journal must be visible
// to B through xAccess — since issue #68 the base VFS reports a
// "-journal" name's existence from the cluster (its sector ConfigMaps), as
// a real filesystem reports from its directory, so SQLite reaches
// hasHotJournal. (Before that change the base answered "sqlite side files
// never exist" and kept SQLite from ever asking.) And the journal's magic
// byte at offset 0 must be non-zero, because hasHotJournal returns before
// calling xCheckReservedLock when it reads zero there; SQLite writes that
// magic only for a file that advertises SQLITE_IOCAP_SAFE_APPEND, so
// TestFileAdvertisesSafeAppend pins the flag and keeps this test from
// quietly going vacuous again. Verified by reverting the negation with the
// flag in place: this test fails with "database is locked", and passes with
// it.
func TestCheckReservedLockCrossesCGOBoundary(t *testing.T) {
	v1, v2 := newContendingVFSInstances(t)
	vfs1Name := registerUniqueVFS(t, v1)
	vfs2Name := registerUniqueVFS(t, v2)
	fileName := newSQLFileName()

	dbA := openRollbackJournalSQLDB(t, vfs1Name, fileName)
	defer dbA.Close()
	dbB := openRollbackJournalSQLDB(t, vfs2Name, fileName)
	defer dbB.Close()

	// Setup runs on A alone: table plus committed rows. B stays untouched —
	// its first read below must be its first contact with the database, so
	// it walks the PAGER_OPEN -> hasHotJournal path the boundary crossing
	// lives on (a reader with a lock already held skips that check).
	if _, err := dbA.Exec(`CREATE TABLE books (
		id text NOT NULL PRIMARY KEY,
		title text
	)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	const seededRows = 10
	insertBatchWithBusyRetry(t, dbA, "a", seededRows)
	if t.Failed() {
		return
	}

	// A writer transaction left open: A holds RESERVED and its rollback
	// journal is on the disk (visible to B through journalVisibleVFS).
	const heldRows = 5
	tx, err := dbA.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	stmt, err := tx.Prepare("INSERT INTO books (id, title) VALUES (?, ?)")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	for i := seededRows; i < seededRows+heldRows; i++ {
		if _, err := stmt.Exec(fmt.Sprintf("a-%03d", i), fmt.Sprintf("title a %d", i)); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	if err := stmt.Close(); err != nil {
		t.Fatalf("stmt.Close: %v", err)
	}

	// THE BOUNDARY: B's first read while A's journal exists and A holds
	// RESERVED. xCheckReservedLock on B's file must tell SQLite (via the
	// shim) that a writer holds the database, so A's journal is treated as
	// live and left alone: the read succeeds on the pre-transaction
	// snapshot, uncommitted rows invisible. With the inversion unworked,
	// this read fails — SQLite would roll back a live writer's journal.
	var n int
	if err := dbB.QueryRow("SELECT COUNT(*) FROM books").Scan(&n); err != nil {
		t.Fatalf("reader COUNT(*) while writer holds an open transaction and its journal: %v (with a correct xCheckReservedLock answer the live journal is left alone)", err)
	}
	if n != seededRows {
		t.Errorf("reader COUNT(*) during writer tx = %d, want %d (uncommitted rows must be invisible)", n, seededRows)
	}

	// The journal leaves with the commit, and B reads the committed rows
	// through the same path: Access(journal) now answers false, so no
	// boundary crossing is even needed — proving the setup was the journal,
	// not the writer lock, that shaped the first read.
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := dbB.QueryRow("SELECT COUNT(*) FROM books").Scan(&n); err != nil {
		t.Fatalf("reader COUNT(*) after commit: %v", err)
	}
	if n != seededRows+heldRows {
		t.Errorf("reader COUNT(*) after commit = %d, want %d", n, seededRows+heldRows)
	}
}

// TestFileAdvertisesSafeAppend pins the device characteristic that makes
// SQLite's hot-journal protocol work at all against this VFS (and with it
// the cgo CheckReservedLock answer below).
//
// SQLite's writeJournalHdr (pager.c) writes the rollback journal's magic
// bytes at offset 0 only when the database file advertises
// SQLITE_IOCAP_SAFE_APPEND, or the pager runs no-sync / an in-memory
// journal; otherwise it deliberately writes zeros, assuming a crash could
// leave garbage appended to a partially grown file. hasHotJournal then
// reads that first byte and treats zero as "no hot journal" — skipping its
// xCheckReservedLock call entirely. With the flag missing, SQLite never
// even asks the question TestCheckReservedLockCrossesCGOBoundary exists to
// pin, so that test passes in both worlds and proves nothing. This cheap
// assertion is what fails first when the characteristic is dropped, naming
// the reason instead of leaving a silently vacuous cgo test behind.
func TestFileAdvertisesSafeAppend(t *testing.T) {
	v, _ := newTestVFS(t)
	f := openTestFile(t, v, "safeappend.db")

	got := f.DeviceCharacteristics()
	if got&sqlite3vfs.IocapSafeAppend == 0 {
		t.Errorf("DeviceCharacteristics() = %v, missing IocapSafeAppend: SQLite would zero the rollback journal's magic at offset 0, "+
			"hasHotJournal would treat every journal as cold, and TestCheckReservedLockCrossesCGOBoundary would stop exercising the answer", got)
	}
	if got&sqlite3vfs.IocapAtomic64K == 0 {
		t.Errorf("DeviceCharacteristics() = %v, missing IocapAtomic64K: one sector is one whole ConfigMap write", got)
	}
}
