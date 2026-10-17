package vfs

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/psanford/sqlite3vfs"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// This file holds the crash-during-journal-delete tests for issue #121.
//
// Deleting a rollback journal is one LIST plus one Delete per sector ConfigMap
// plus one for the lockfile, and none of it is atomic, so a crash between any
// two calls leaves a half-state on the cluster. These tests inject a failure
// after each Delete in the sequence, then reopen the database through a FRESH
// VFS instance — the next process — and check what it reads.
//
// Three things make the tests honest rather than decorative:
//
//  1. The journal is REAL. It is written by a genuine SQLite transaction that
//     spilled its pages and committed, whose journal Delete was swallowed by a
//     no-op Delete wrapper (countingDeleteVFS below, the same trick
//     blindJournalVFS uses for Access): SQLite believes the journal is gone and
//     finishes its transaction, leaving a real journal on the cluster with a
//     non-zero magic byte at offset 0. Nothing here hands SQLite bytes the test
//     made up. Because the transaction COMMITTED, the database's own sectors
//     are already the committed image, so this is exactly the state during a
//     journal's own removal: the image needs no rollback and only the journal
//     is being unlinked. That is the state SQLite really deletes a journal in
//     (it rolls a journal back before unlinking it); deleting a journal
//     SQLite still has to roll back is not a state its own delete path can
//     produce, and it corrupts the database for any deletion order — measured
//     while building this file: roll back first, then delete, gives
//     integrity_check "ok" with the committed rows; delete the same hot journal
//     without the rollback first gives a mixed, corrupt database whichever
//     sectors are removed. The order under test here is therefore the order
//     that matters.
//  2. On the commit path SQLite truncates a rollback journal to two pages — the
//     header and one page — so a journal it leaves behind is 2 sectors however
//     large the transaction was, and the delete sequence is 3 calls. vfs.Delete
//     is deliberately NotFound-tolerant for sectors an earlier crashed delete
//     already removed, so the tests plant extra sectors with seedSector (the
//     VFS's own naming and keys) to make the journal span 3+ sectors: with 2
//     planted leftovers the sequence is 5 calls, and "the header is gone but
//     later sectors remain" is a state the tests can actually reach.
//  3. Two failure shapes, tested separately, because they prove different
//     things. A ONE-SHOT failure (the k-th Delete fails, the rest are allowed)
//     exercises the retry path that converges without the caller ever seeing an
//     error — the durability property. A PERSISTENT failure (the k-th Delete
//     and every one after it fail) exhausts vfs.Delete's retry budget and
//     SURFACES the failure, which is what makes the half-state on the cluster
//     observable and is what an injected API failure from a dead apiserver
//     looks like to SQLite.

// countingDeleteVFS performs real deletes, or swallows them while swallow is
// set. Swallowing is how the fixture gets a real, still-hot journal onto the
// cluster from a real committed transaction.
type countingDeleteVFS struct {
	*vfs
	swallow bool
	n       int
}

func (c *countingDeleteVFS) Delete(name string, dirSync bool) error {
	c.n++
	if c.swallow {
		// SQLite believes the journal is gone; its sectors stay behind.
		return nil
	}
	return c.vfs.Delete(name, dirSync)
}

// deleteInjector fails configmap Deletes and records every one it sees. It is
// installed with PrependReactor: returning handled=true with an error means the
// delete never reaches the tracker, so the injected failure is a real failure
// of the API call rather than a simulated one.
//
// once=true fails only the n-th delete (the retry path then converges);
// once=false fails the n-th and every later one (the retry budget is spent).
type deleteInjector struct {
	deleted []string
	seen    int
	failAt  int // 0 = fail nothing
	arm     bool
	once    bool
}

func (d *deleteInjector) reactor(action k8stesting.Action) (bool, runtime.Object, error) {
	name := action.(k8stesting.DeleteAction).GetName()
	d.deleted = append(d.deleted, name)
	d.seen++
	if d.arm && d.failAt > 0 && d.seen >= d.failAt {
		if d.once && d.seen > d.failAt {
			return false, nil, nil // only the k-th fails
		}
		return true, nil, fmt.Errorf("injected transient API failure on configmap Delete #%d (%s)", d.seen, name)
	}
	return false, nil, nil
}

func (d *deleteInjector) startAt(failAt int, once bool) {
	d.deleted = nil
	d.seen = 0
	d.failAt = failAt
	d.once = once
	d.arm = true
}

func (d *deleteInjector) disarm() {
	d.deleted = nil
	d.seen = 0
	d.failAt = 0
	d.arm = false
}

// suffixNames shortens recorded Delete names to their "-<sector|lockfile>" tail:
// the base32 file name is random and adds nothing to a failure message.
func suffixNames(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if i := strings.LastIndex(n, "-"); i >= 0 {
			out = append(out, n[i:])
		} else {
			out = append(out, n)
		}
	}
	return out
}

// journalSectorIndexes returns the journal's surviving sector indexes, ascending.
func journalSectorIndexes(tb testing.TB, fcs *fake.Clientset, v *vfs, dbName string) []int64 {
	tb.Helper()
	f := NewFile(dbName+"-journal", v)
	var out []int64
	for _, cm := range journalSectorConfigMaps(tb, fcs, dbName) {
		if idx, ok := f.sectorIndexFromConfigMapName(cm.Name); ok {
			out = append(out, idx)
		} else {
			tb.Fatalf("journal sector ConfigMap %q has no parsable sector index", cm.Name)
		}
	}
	return out
}

func indexesString(in []int64) string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		out = append(out, strconv.FormatInt(v, 10))
	}
	return strings.Join(out, ",")
}

// lockfilePresent reports whether the journal's lock ConfigMap is still there.
// vfs.Delete removes it LAST, so it is gone only if the whole sequence ran.
func lockfilePresent(tb testing.TB, fcs *fake.Clientset, v *vfs, dbName string) bool {
	tb.Helper()
	f := NewFile(dbName+"-journal", v)
	_, err := fcs.CoreV1().ConfigMaps(testNamespace).Get(tb.Context(), f.LockFileName(), metav1.GetOptions{})
	return err == nil
}

// journalMagicByteOrZero reads the journal's first byte — the byte SQLite's
// hasHotJournal reads to decide whether a journal is hot — and returns 0 when
// sector 0 is gone, which is exactly what SQLite reads then (file.ReadAt past
// the end of a sector file yields zeros, so a missing header is a cold journal).
func journalMagicByteOrZero(tb testing.TB, v *vfs, fcs *fake.Clientset, dbName string) byte {
	tb.Helper()
	cms := journalSectorConfigMaps(tb, fcs, dbName)
	if len(cms) == 0 {
		return 0
	}
	want := NewFile(dbName+"-journal", v).sectorNameFromSectorIndex(0)
	for _, cm := range cms {
		if cm.Name != want {
			continue
		}
		if b := cm.BinaryData["sector"]; len(b) > 0 {
			return b[0]
		}
		return 0
	}
	return 0
}

// newDeleteFixture stages a real hot journal on a fake clientset and returns the
// fixture plus the number of Deletes one pass of vfs.Delete needs for it
// (sectors + lockfile), which is what the "after each Delete" loop iterates.
func newDeleteFixture(tb testing.TB) (*deleteFixture, int) {
	tb.Helper()
	clock := &fakeClock{now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	fcs := fake.NewSimpleClientset()
	injector := &deleteInjector{}
	fcs.PrependReactor("delete", "configmaps", injector.reactor)

	v := NewVFS(tb.Context(), fcs, testNamespace, discardLogger(), 3, WithLockLease(testLockLease))
	v.clock = clock.Now
	swallower := &countingDeleteVFS{vfs: v}
	name := registerUniqueVFS(tb, swallower)
	dbName := newSQLFileName()

	db := openSQLDBWithJournal(tb, name, dbName, "DELETE")
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA cache_size=-8"); err != nil {
		tb.Fatalf("PRAGMA cache_size: %v", err)
	}
	// Seeding deletes for real; only the transaction whose journal must survive
	// has its Delete swallowed.
	seedCommittedTable(tb, db)
	if got := countRows(tb, db); got != journalSeedRows {
		tb.Fatalf("seeded rows = %d, want %d", got, journalSeedRows)
	}
	swallower.swallow = true
	tx := interruptWriteTransaction(tb, db)
	if err := tx.Commit(); err != nil {
		tb.Fatalf("commit with the journal Delete swallowed failed: %v", err)
	}
	if swallower.n == 0 {
		tb.Fatal("no Delete was swallowed, so no journal was left behind: the fixture's premise is missing")
	}

	// The real journal's state, read back through the clientset.
	base := len(journalSectorConfigMaps(tb, fcs, dbName))
	if base == 0 {
		tb.Fatal("the swallowed commit left no journal sectors: the fixture is vacuous")
	}
	if b := journalMagicByte(tb, v, fcs, dbName); b == 0 {
		tb.Fatal("the staged journal's first byte is zero, so it is not hot")
	}
	// A journal SQLite leaves behind is exactly two sectors: it truncates the
	// journal to the header page and one page at commit, and this VFS stores one
	// ConfigMap per 64 KiB, so the delete sequence is three calls — sector 0,
	// sector 1, lockfile. (Measured: the transaction below spills well over the
	// journal pages that survive, and the sector count is 2 mid-transaction and
	// at commit, with and without PRAGMA journal_size_limit.) Fabricating extra
	// sectors would make the journal internally inconsistent — SQLite's header
	// would describe 2 pages while the file spans 3+ — and SQLite then fails to
	// open it with a disk I/O error, a state no real crash produces. The
	// "header gone, later sectors remain" half-state is reached honestly
	// instead, by failing at delete #2 in the tests below.
	fx := &deleteFixture{fcs: fcs, injector: injector, dbName: dbName, clock: clock, writer: db}
	return fx, base + 1 // sectors + lockfile
}

// deleteFixture is a database whose cluster carries a real hot journal.
type deleteFixture struct {
	fcs      *fake.Clientset
	injector *deleteInjector
	dbName   string
	clock    *fakeClock
	writer   *sql.DB // the connection that produced the journal; keep it alive
}

// vfsFor returns a throwaway VFS over the fixture's clientset for sector-name
// arithmetic and direct calls. Every delete and reopen uses its own instance.
func (fx *deleteFixture) vfsFor(tb testing.TB) *vfs {
	tb.Helper()
	v := NewVFS(tb.Context(), fx.fcs, testNamespace, discardLogger(), 3, WithLockLease(testLockLease))
	v.clock = fx.clock.Now
	return v
}

// deleteWith runs REAL vfs.Delete for the journal through a fresh VFS with the
// injector failing from the failAt-th Delete on, and returns the error plus the
// Delete names the clientset saw.
func (fx *deleteFixture) deleteWith(tb testing.TB, failAt int, once bool) (error, []string) {
	tb.Helper()
	v := fx.vfsFor(tb)
	fx.injector.startAt(failAt, once)
	err := v.Delete(fx.dbName+"-journal", false)
	deleted := append([]string(nil), fx.injector.deleted...)
	// The injected outage is over: it belongs to the Delete under test, not to
	// the reopen or the later write that follow it.
	fx.injector.disarm()
	return err, deleted
}

// deleteThrough is deleteWith with a persistent failure.
func (fx *deleteFixture) deleteThrough(tb testing.TB, failAt int) (error, []string) {
	tb.Helper()
	return fx.deleteWith(tb, failAt, false)
}

// reopen opens the database through a FRESH VFS instance over the same
// clientset — the next process after the crash.
func (fx *deleteFixture) reopen(tb testing.TB) (db *sql.DB, integrity string, rows, uncommitted int) {
	tb.Helper()
	db, _, _ = openJournalDB(tb, fx.fcs, fx.dbName, fx.clock)
	return db, integrityCheck(tb, db), countRows(tb, db), countUncommitted(tb, db)
}

// closedFixture closes the staging connection. The tests keep it open for the
// duration so Go's finalizers cannot tidy up mid-test.
func (fx *deleteFixture) close() {
	if fx.writer != nil {
		_ = fx.writer.Close()
	}
}

const (
	// preRows is the transaction's own committed row count: the seed deletes
	// journalSeedRows rows and inserts journalCrashedRows, and it committed, so
	// the database image holds exactly those rows.
	preRows = journalCrashedRows
	// postRows is what a later write leaves behind; set by laterCommittedWrite.
	laterWriteRows = 20
)

// TestCrashPartWayThroughJournalDelete is issue #121's core test: for every k,
// run the journal delete with the k-th Delete failing and every one after it,
// check the cluster's half-state, reopen the database and prove it is intact
// with one of the two legal images and never a mix, then prove a later write
// collects the leftover sectors.
func TestCrashPartWayThroughJournalDelete(t *testing.T) {
	_, n := newDeleteFixture(t)
	t.Logf("journal needs %d deletes: sector 0 first, then the rest, then the lockfile", n)

	for k := 1; k <= n; k++ {
		t.Run(fmt.Sprintf("fail_at_delete_%d", k), func(t *testing.T) {
			fx, n := newDeleteFixture(t)
			defer fx.close()

			before := journalSectorIndexes(t, fx.fcs, fx.vfsFor(t), fx.dbName)
			magicBefore := journalMagicByteOrZero(t, fx.vfsFor(t), fx.fcs, fx.dbName)
			if len(before) != n-1 {
				t.Fatalf("the journal starts with sectors [%s], want %d sectors (%d deletes minus the lockfile)", indexesString(before), n-1, n)
			}

			err, deleted := fx.deleteThrough(t, k)

			// The injected failure surfaces. vfs.Delete retries the whole
			// sequence vfs.retries+1 times; because this injector fails the
			// k-th Delete AND every later one, every attempt stops in the same
			// place, the retry budget is spent, and Delete returns the landed
			// code's exhaustion error: sqlite3vfs.IOError (vfs.go's final
			// return in Delete).
			if !errors.Is(err, sqlite3vfs.IOError) {
				t.Fatalf("Delete with a persistent failure from delete #%d returned %v, want sqlite3vfs.IOError; deletes seen %v", k, err, suffixNames(deleted))
			}
			t.Logf("k=%d first attempt deleted %v (all %d Deletes: %v)", k, suffixNames(deleted[:min(n, len(deleted))]), len(deleted), suffixNames(deleted))

			after := journalSectorIndexes(t, fx.fcs, fx.vfsFor(t), fx.dbName)
			magicAfter := journalMagicByteOrZero(t, fx.vfsFor(t), fx.fcs, fx.dbName)
			lockAfter := lockfilePresent(t, fx.fcs, fx.vfsFor(t), fx.dbName)

			if k == 1 {
				// The first Delete is the header, so failing it must leave the
				// journal exactly as it was: still hot, still complete — a
				// fully replayable journal is a safe state.
				if magicAfter != magicBefore {
					t.Errorf("a failure at the first Delete changed the journal's first byte from %#x to %#x, but the first Delete must be the header and failing it must leave the journal untouched", magicBefore, magicAfter)
				}
				if indexesString(after) != indexesString(before) {
					t.Errorf("a failure at the first Delete left sectors [%s], want the untouched [%s]", indexesString(after), indexesString(before))
				}
			} else if magicAfter != 0 {
				t.Errorf("after a failure at delete #%d the journal's first byte is %#x, want 0: sector 0's Delete is the first one, so by then the journal must read as cold or the next opener replays a half-deleted journal", k, magicAfter)
			}
			// The lockfile goes LAST, so it can only be gone if the whole
			// sequence ran.
			if k < n && !lockAfter {
				t.Errorf("the lockfile is gone after a failure at delete #%d of %d, but vfs.Delete deletes it last", k, n)
			}
			// k-1 deletes went through, so k-1 of the k-1+... sectors are gone;
			// the header is among them for every k >= 2.
			if want := len(before) - (k - 1); k < n && len(after) != want {
				t.Errorf("after a failure at delete #%d the journal has sectors [%s], want %d of them", k, indexesString(after), want)
			}

			// Reopen as the next process would, through a fresh VFS.
			db, integrity, rows, uncommitted := fx.reopen(t)
			defer db.Close()

			if integrity != "ok" {
				t.Errorf("after a failure at delete #%d integrity_check = %q, want \"ok\"", k, integrity)
			}
			// preRows (200, all of them the transaction's own committed rows)
			// is the post-transaction image; journalSeedRows (100) is the
			// pre-transaction image. A replay of a still-hot journal gives the
			// transaction's own image; a cold or absent journal gives whatever
			// the database sectors hold. Both are legal here, a third number is
			// not, and the uncommitted count separates the two.
			switch rows {
			case preRows:
				if uncommitted != preRows {
					t.Errorf("reopened database has %d rows (the post-transaction image) but only %d of them are the transaction's own: the two images are mixed", rows, uncommitted)
				}
			case journalSeedRows:
				if uncommitted != 0 {
					t.Errorf("reopened database has %d rows (the pre-transaction image) but %d uncommitted rows: the two images are mixed", rows, uncommitted)
				}
			default:
				t.Errorf("after a failure at delete #%d the reopened database has %d rows, want either %d (pre-transaction) or %d (post-transaction): a third number means a corrupted or mixed database", k, rows, journalSeedRows, preRows)
			}
			t.Logf("k=%d: sectors %s -> [%s], lockfile present=%v, magic %#x -> %#x, reopened integrity=%q rows=%d uncommitted=%d",
				k, indexesString(before), indexesString(after), lockAfter, magicBefore, magicAfter, integrity, rows, uncommitted)

			// A later committed write must collect the leftover sectors.
			if err := laterCommittedWrite(t, db); err != nil {
				t.Fatalf("the later committed write failed after a failure at delete #%d: %v", k, err)
			}
			if left := journalSectorIndexes(t, fx.fcs, fx.vfsFor(t), fx.dbName); len(left) != 0 {
				t.Errorf("after the later write the journal still has sectors [%s], want none: the leftovers were not collected", indexesString(left))
			}
			if got := integrityCheck(t, db); got != "ok" {
				t.Errorf("after the later write integrity_check = %q, want \"ok\"", got)
			}
			// The later write only ADDS rows to whichever image the reopen
			// settled on, so the count is that image's rows plus the rows
			// written now. Either image is acceptable; a third count is not.
			if want := rows + laterWriteRows; countRows(t, db) != want {
				t.Errorf("after the later write the database has %d rows, want %d (%d from the reopened image + %d written now)", countRows(t, db), want, rows, laterWriteRows)
			}
		})
	}
}

// laterCommittedWrite commits one more transaction through a reopened
// connection: the first SQLite write after the crash, whose journal Delete is
// what collects the sectors a crashed delete left behind.
func laterCommittedWrite(tb testing.TB, db *sql.DB) error {
	tb.Helper()
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("later Begin: %w", err)
	}
	stmt, err := tx.Prepare("INSERT INTO books (id, title) VALUES (?, ?)")
	if err != nil {
		return fmt.Errorf("later Prepare: %w", err)
	}
	pad := strings.Repeat("w", journalRowPadding)
	for i := 0; i < laterWriteRows; i++ {
		id := 200000 + i
		if _, err := stmt.Exec(id, fmt.Sprintf("later %d", id)+pad); err != nil {
			return fmt.Errorf("later insert %d: %w", id, err)
		}
	}
	if err := stmt.Close(); err != nil {
		return fmt.Errorf("later stmt.Close: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("later Commit: %w", err)
	}
	return nil
}

// TestJournalDeleteTransientFailureIsRecovered pins the OTHER half of the
// failure story, the one SQLite actually relies on in normal operation: a
// transient failure of a single Delete (the apiserver blinked) is retried, and
// vfs.Delete still reports success — the caller never sees the half-state, and
// no journal sector survives. This is why the crash tests above have to inject a
// PERSISTENT failure: a one-shot one never leaves a half-state behind at all.
func TestJournalDeleteTransientFailureIsRecovered(t *testing.T) {
	_, n0 := newDeleteFixture(t)
	for k := 1; k <= n0; k++ {
		t.Run(fmt.Sprintf("transient_at_delete_%d", k), func(t *testing.T) {
			fx, n := newDeleteFixture(t)
			defer fx.close()

			err, deleted := fx.deleteWith(t, k, true)
			if err != nil {
				t.Fatalf("a ONE-SHOT failure at delete #%d returned %v, want nil: vfs.Delete's retry must recover it", k, err)
			}
			// The retry re-LISTs, so the header goes first again (its Delete is
			// NotFound-tolerant now) and the sequence finishes.
			if left := journalSectorIndexes(t, fx.fcs, fx.vfsFor(t), fx.dbName); len(left) != 0 {
				t.Errorf("a one-shot failure at delete #%d of %d left sectors [%s], want none", k, n, indexesString(left))
			}
			if lockfilePresent(t, fx.fcs, fx.vfsFor(t), fx.dbName) {
				t.Errorf("a one-shot failure at delete #%d left the lockfile behind", k)
			}
			if got := journalMagicByteOrZero(t, fx.vfsFor(t), fx.fcs, fx.dbName); got != 0 {
				t.Errorf("a one-shot failure at delete #%d left a journal whose first byte is %#x, want 0", k, got)
			}
			t.Logf("one-shot failure at delete #%d of %d: recovered, nil, no journal sectors left; Deletes seen %v", k, n, suffixNames(deleted))

			// And the database is intact with the transaction's own rows.
			db, integrity, rows, uncommitted := fx.reopen(t)
			defer db.Close()
			if integrity != "ok" {
				t.Errorf("integrity_check after a recovered transient failure = %q, want \"ok\"", integrity)
			}
			if rows != preRows || uncommitted != preRows {
				t.Errorf("reopened rows = %d (uncommitted %d), want the transaction's own %d/%d", rows, uncommitted, preRows, preRows)
			}
		})
	}
}

// TestJournalDeleteHeaderFirstIsLoadBearing is the control that makes the
// header-first order visible rather than assumed. The FIRST Delete of the
// sequence must be sector 0 — the header whose magic byte makes SQLite treat
// the journal as hot — so that no prefix of the sequence leaves a journal with
// a live header and missing later sectors (the one shape that could make SQLite
// replay a truncated journal, and the state a crash in the old LIST-order delete
// could produce). The test fails the first Delete and asserts the journal is
// untouched: still hot, still complete, and therefore still fully replayable.
func TestJournalDeleteHeaderFirstIsLoadBearing(t *testing.T) {
	fx, _ := newDeleteFixture(t)
	defer fx.close()

	f := fx.vfsFor(t)
	before := journalSectorIndexes(t, fx.fcs, f, fx.dbName)
	magicBefore := journalMagicByteOrZero(t, f, fx.fcs, fx.dbName)

	err, deleted := fx.deleteThrough(t, 1)
	if !errors.Is(err, sqlite3vfs.IOError) {
		t.Fatalf("Delete returned %v, want sqlite3vfs.IOError", err)
	}
	if len(deleted) == 0 {
		t.Fatal("no Delete reached the clientset")
	}
	// The first Delete of the sequence is sector 0. If it is not, this control
	// is testing the wrong thing and says so.
	if got, want := deleted[0], NewFile(fx.dbName+"-journal", f).sectorNameFromSectorIndex(0); got != want {
		t.Fatalf("the first Delete was %q, want sector 0 (%q): the header-first order is gone, so this control proves nothing", got, want)
	}
	after := journalSectorIndexes(t, fx.fcs, f, fx.dbName)
	if indexesString(after) != indexesString(before) {
		t.Errorf("a failure at the first Delete left sectors [%s], want the untouched [%s]", indexesString(after), indexesString(before))
	}
	if got := journalMagicByteOrZero(t, f, fx.fcs, fx.dbName); got != magicBefore {
		t.Errorf("a failure at the first Delete changed the journal's first byte from %#x to %#x: the journal must be untouched", magicBefore, got)
	}
	if !lockfilePresent(t, fx.fcs, f, fx.dbName) {
		t.Error("the lockfile is gone after a failure at the first Delete, but it is deleted last")
	}

	// A complete hot journal is a safe state: the next opener replays it and
	// finds exactly one of the two legal images.
	db, integrity, rows, uncommitted := fx.reopen(t)
	defer db.Close()
	if integrity != "ok" {
		t.Errorf("integrity_check = %q, want \"ok\"", integrity)
	}
	if rows != journalSeedRows && rows != preRows {
		t.Errorf("reopened rows = %d, want %d or %d", rows, journalSeedRows, preRows)
	}
	if rows == journalSeedRows && uncommitted != 0 {
		t.Errorf("rows = %d (pre-transaction) with %d uncommitted rows: the images are mixed", rows, uncommitted)
	}
	if rows == preRows && uncommitted != preRows {
		t.Errorf("rows = %d (post-transaction) with only %d of the transaction's own rows: the images are mixed", rows, uncommitted)
	}
	t.Logf("first Delete = sector 0; journal untouched [%s], magic %#x, reopened integrity=%q rows=%d uncommitted=%d", indexesString(after), magicBefore, integrity, rows, uncommitted)
}

// TestJournalDeleteLeavesNoReplayableGarbage pins the claim the delete order
// rests on: once sector 0 is gone the surviving sectors are cold garbage the
// next opener ignores. Sector 0 is deleted and the rest are left, and the next
// opener must read a legal image — never a replay of the leftovers — and the
// database must still be intact.
func TestJournalDeleteLeavesNoReplayableGarbage(t *testing.T) {
	fx, _ := newDeleteFixture(t)
	defer fx.close()

	f := fx.vfsFor(t)
	// Persistently fail from Delete #2: sector 0 goes, the rest stay.
	if err, deleted := fx.deleteThrough(t, 2); !errors.Is(err, sqlite3vfs.IOError) {
		t.Fatalf("Delete returned %v, want sqlite3vfs.IOError; deletes seen %v", err, suffixNames(deleted))
	}
	after := journalSectorIndexes(t, fx.fcs, f, fx.dbName)
	if len(after) == 0 {
		t.Fatalf("no leftover sectors remain, so this test cannot observe them")
	}
	if magic := journalMagicByteOrZero(t, f, fx.fcs, fx.dbName); magic != 0 {
		t.Fatalf("the header is still readable (first byte %#x) after sector 0 was deleted", magic)
	}
	if !lockfilePresent(t, fx.fcs, f, fx.dbName) {
		t.Error("the lockfile is gone although the delete never reached it: it is deleted LAST")
	}

	db, integrity, rows, uncommitted := fx.reopen(t)
	defer db.Close()
	if integrity != "ok" {
		t.Errorf("integrity_check = %q, want \"ok\"", integrity)
	}
	switch rows {
	case preRows:
		if uncommitted != preRows {
			t.Errorf("rows = %d (post-transaction) with only %d of the transaction's own rows: the images are mixed", rows, uncommitted)
		}
	case journalSeedRows:
		if uncommitted != 0 {
			t.Errorf("rows = %d (pre-transaction) with %d uncommitted rows: the images are mixed", rows, uncommitted)
		}
	default:
		t.Errorf("reopened rows = %d, want %d or %d", rows, journalSeedRows, preRows)
	}

	// The leftovers are collected by the next delete, not replayed.
	if err := laterCommittedWrite(t, db); err != nil {
		t.Fatalf("later committed write: %v", err)
	}
	if left := journalSectorIndexes(t, fx.fcs, fx.vfsFor(t), fx.dbName); len(left) != 0 {
		t.Errorf("after the later write the journal still has sectors [%s], want none", indexesString(left))
	}
	t.Logf("sector 0 gone, leftovers [%s] left cold, first byte 0, reopened integrity=%q rows=%d uncommitted=%d", indexesString(after), integrity, rows, uncommitted)
}

// TestJournalDeleteFixtureIsNotVacuous proves the fixture's journal is real: the
// same journal NOT deleted at all must make the next opener do something
// different from the cold-journal case — a journal whose removal is a no-op
// would make every "the database is intact" assertion above vacuous.
func TestJournalDeleteFixtureIsNotVacuous(t *testing.T) {
	fx, _ := newDeleteFixture(t)
	defer fx.close()

	f := fx.vfsFor(t)
	if got := journalMagicByteOrZero(t, f, fx.fcs, fx.dbName); got == 0 {
		t.Fatal("the staged journal is not hot (first byte is zero), so the fixture proves nothing")
	}
	sectorsBefore := journalSectorIndexes(t, fx.fcs, f, fx.dbName)

	db, integrity, rows, uncommitted := fx.reopen(t)
	if integrity != "ok" {
		t.Errorf("integrity_check = %q, want \"ok\"", integrity)
	}
	// The journal is real and hot, and the fixture's own writer still sees the
	// uncommitted transaction, so the two legal images differ: a reopen that
	// REPLAYS the journal would give the transaction's own %d rows, while the
	// image the database sectors hold is the %d committed rows. What a fresh
	// opener actually gives is one of the two — and it is the committed rows
	// here, because this journal's rollback has already been applied (the
	// fixture's commit ran through a swallow-Delete wrapper, so SQLite finished
	// the transaction and wrote the rolled-forward journal out).
	if rows != journalSeedRows && rows != preRows {
		t.Errorf("reopened rows = %d, want %d or %d", rows, journalSeedRows, preRows)
	}
	if rows == journalSeedRows && uncommitted != 0 {
		t.Errorf("rows = %d (pre-transaction) with %d uncommitted rows: the images are mixed", rows, uncommitted)
	}
	if rows == preRows && uncommitted != preRows {
		t.Errorf("rows = %d (post-transaction) with only %d of the transaction's own rows: the images are mixed", rows, uncommitted)
	}
	// The reopened instance consumed the journal: it is gone afterwards, so the
	// delete tests above are not measuring a journal a mere reopen would have
	// removed for them.
	if left := journalSectorIndexes(t, fx.fcs, f, fx.dbName); len(left) != 0 {
		t.Errorf("the reopened instance left journal sectors [%s], want none: opening must consume the journal", indexesString(left))
	}
	db.Close()
	t.Logf("control: journal [%s] present and hot; reopened integrity=%q rows=%d uncommitted=%d, journal consumed", indexesString(sectorsBefore), integrity, rows, uncommitted)
}

var _ = v1.ConfigMap{}
