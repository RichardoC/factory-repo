package vfs

// Reader-side tests for issue #121: the states a rollback journal can be left
// in when a process dies part-way through deleting it, and what this VFS then
// reports to a reader that opens the name again.
//
// The delete of a journal is one LIST plus one Delete per sector ConfigMap plus
// one for the lockfile, and none of that is atomic, so a crash can leave any
// prefix of it done. SQLite decides what to do with such a name from two facts
// only — the answer to xAccess (here: the journal has at least one sector
// ConfigMap) and the first byte read at file offset 0 (here: the first byte of
// sector 0, a missing sector reading as zeros). These tests pin the reader side
// of every half-state at the VFS layer, with no SQLite involved: the exact
// vfs.Access / vfs.Open / FileSize / ReadAt answers, the ConfigMaps that
// survive, and the fact that a read never creates the sector it cannot find.
//
// The sibling file vfs_test.go owns TestDeleteJournalRemovesAllSectorsAndLockfile
// (the all-at-once happy path); this file owns the half-states around it.
//
// Nothing here changes production code. Each assertion is labelled in its
// comment as [as-is] (describes the landed code) or [would-need-a-change]
// (documents a hazard this VFS currently has), so the reader can tell a pinned
// behaviour from a latent one.

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"testing"

	"github.com/psanford/sqlite3vfs"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// journalHeaderFirstByte is the byte SQLite's hasHotJournal reads at offset 0
// to decide whether a journal must be played back. Any non-zero value marks a
// hot journal; the fillPattern helper writes a zero there, so the fixtures
// below seed sector 0 with this instead.
const journalHeaderFirstByte = 0x21

// halfStateDeleteSpy is a delete reactor for the fake clientset that records
// every ConfigMap Delete the VFS issues and can fail them on demand: from the
// n-th call onward (failFrom), or every call (failEvery). It returns
// "not handled" for a call it means to succeed, so the tracker performs the
// delete exactly as a real API server would.
//
// failFrom is mutable so one test can inject a crash mid-sequence and then let
// the delete through again — the "resume after a crash" the work order asks
// for — without rebuilding the clientset (clearing fake.Clientset.ReactionChain
// would also drop the tracker's own default reactors and silently disable the
// fake clientset).
type halfStateDeleteSpy struct {
	mu        sync.Mutex
	calls     []string // every Delete name, in order
	failFrom  int      // 1-based: fail this call and every later one
	failEvery bool
}

func (s *halfStateDeleteSpy) reactor(action k8stesting.Action) (bool, runtime.Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := action.(k8stesting.DeleteAction).GetName()
	s.calls = append(s.calls, name)
	n := len(s.calls)
	if s.failEvery || (s.failFrom > 0 && n >= s.failFrom) {
		return true, nil, fmt.Errorf("injected API failure on configmap delete #%d (%q)", n, name)
	}
	return false, nil, nil
}

// setFailFrom makes the spy fail from the n-th delete (1-based) onward; n <= 0
// lets every delete through again.
func (s *halfStateDeleteSpy) setFailFrom(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failFrom = n
	s.failEvery = false
}

func (s *halfStateDeleteSpy) stopFailing() { s.setFailFrom(0) }

// deleteCalls returns a copy of the names the VFS asked the API to delete.
func (s *halfStateDeleteSpy) deleteCalls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

// newHalfStateFixture builds the journal state every subtest starts from: a
// journal name with sectors 0..2 and a lockfile, plus a same-stem database with
// two sectors and its own lockfile, so each subtest can assert the database was
// left alone.
//
// The lockfile is the real one Open creates; the sector ConfigMaps are seeded
// directly (seedMultiSectorFile above sector 1 plus one extra), as state another
// process would have written.
type halfStateFixture struct {
	v         *vfs
	fcs       *fake.Clientset
	spy       *halfStateDeleteSpy
	journal   string // e.g. "half-state-1.db-journal"
	dbName    string // the same-stem database, e.g. "half-state-1.db"
	jf        *file  // a *file for the journal name: names and labels
	df        *file  // a *file for the database name
	headerAt0 []byte // the bytes seeded into journal sector 0
}

func newHalfStateFixture(t *testing.T, name string) *halfStateFixture {
	t.Helper()
	v, fcs := newTestVFS(t)
	spy := &halfStateDeleteSpy{}
	fcs.PrependReactor("delete", "configmaps", spy.reactor)

	journal := name + "-journal"
	dbName := name
	fx := &halfStateFixture{v: v, fcs: fcs, spy: spy, journal: journal, dbName: dbName}

	// The journal: sector 0 carries a non-zero magic byte (it is hot), sectors
	// 1 and 2 carry data a rollback would replay.
	fx.jf = openTestFile(t, v, journal)
	fx.headerAt0 = make([]byte, SectorSize)
	fillPattern(fx.headerAt0)
	fx.headerAt0[0] = journalHeaderFirstByte
	seedSector(t, fcs, fx.jf, 0, fx.headerAt0)
	seedSector(t, fcs, fx.jf, 1, make([]byte, SectorSize))
	seedSector(t, fcs, fx.jf, 2, make([]byte, 1234))

	// The same-stem database: two sectors of its own, which no journal delete
	// may touch.
	fx.df = openTestFile(t, v, dbName)
	seedMultiSectorFile(t, fcs, fx.df, multiSectorLen)

	return fx
}

// assertIntactState checks what every subtest requires after any journal
// half-state: the same-stem database keeps its two sectors and its lockfile,
// and the journal's own sector ConfigMaps are exactly the ones named.
func (fx *halfStateFixture) assertDatabaseUntouched(t *testing.T) {
	t.Helper()
	if n := len(listSectorConfigMaps(t, fx.fcs, fx.df)); n != 2 {
		t.Errorf("same-stem database %q: %d sector configmaps, want 2 (a journal delete must not touch the database)", fx.dbName, n)
	}
	if _, err := fx.fcs.CoreV1().ConfigMaps(testNamespace).Get(t.Context(), fx.df.LockFileName(), metav1.GetOptions{}); err != nil {
		t.Errorf("same-stem database lockfile after journal delete: %v, want it present", err)
	}
}

// journalSectorNames returns the names of the journal's surviving sector
// ConfigMaps, sorted, for comparison against an expected set.
func (fx *halfStateFixture) journalSectorNames(t *testing.T) []string {
	t.Helper()
	names := sectorNames(listSectorConfigMaps(t, fx.fcs, fx.jf))
	sort.Strings(names)
	return names
}

// sectorName is the ConfigMap name of one journal sector, as the VFS builds it.
func (fx *halfStateFixture) sectorName(i int64) string {
	return fx.jf.sectorNameFromSectorIndex(i)
}

// assertSectorNames compares the journal's surviving sector ConfigMaps to the
// given indices; a nil/empty want means none may survive.
func (fx *halfStateFixture) assertSectorNames(t *testing.T, wantIndices ...int64) {
	t.Helper()
	want := make([]string, 0, len(wantIndices))
	for _, i := range wantIndices {
		want = append(want, fx.sectorName(i))
	}
	sort.Strings(want)
	got := fx.journalSectorNames(t)
	if len(got) != len(want) {
		t.Fatalf("journal sectors after the half-delete = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("journal sector %d = %q, want %q (the delete order must be header-first)", i, got[i], want[i])
		}
	}
}

// assertNoSectorConfigMap fails if the named sector ConfigMap still exists; the
// GET also proves nothing re-created it.
func (fx *halfStateFixture) assertNoSectorConfigMap(t *testing.T, name string) {
	t.Helper()
	if _, err := fx.fcs.CoreV1().ConfigMaps(testNamespace).Get(t.Context(), name, metav1.GetOptions{}); !kerrors.IsNotFound(err) {
		t.Errorf("sector configmap %q get = %v, want NotFound", name, err)
	}
}

// configMapCount is the number of ConfigMaps in the test namespace, whatever
// their labels: the before/after witness that a read created nothing.
func (fx *halfStateFixture) configMapCount(t *testing.T) int {
	t.Helper()
	all, err := fx.fcs.CoreV1().ConfigMaps(testNamespace).List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list configmaps: %v", err)
	}
	return len(all.Items)
}

// TestHalfDeletedJournalReaderSide pins the reader side of each half-state a
// crashed journal delete can leave. Subtests share nothing but the fixture
// shape: each builds its own clientset.
func TestHalfDeletedJournalReaderSide(t *testing.T) {
	// 1. Fresh-name state: the crash lands right after the header sector's
	// delete, so the journal's name still exists (Access is per sector count)
	// while it can no longer be replayed.
	t.Run("header-gone-later-sectors-survive", func(t *testing.T) {
		fx := newHalfStateFixture(t, "half-state-1.db")

		// The journal is a fresh name with sectors 0..2 and a lockfile.
		fx.assertSectorNames(t, 0, 1, 2)
		if !sectorConfigMapExists(t, fx.fcs, fx.jf.LockFileName()) {
			t.Fatal("setup: the journal's lockfile is missing")
		}
		if got, err := fx.v.Access(fx.journal, sqlite3vfs.AccessExists); err != nil || !got {
			t.Fatalf("setup: Access(%q) = (%v, %v), want (true, nil): the name exists while it has sectors", fx.journal, got, err)
		}

		// Crash after the FIRST delete: everything from the 2nd call on fails,
		// so the retry loop re-LISTs and keeps failing on the leftovers. Delete
		// exhausts its retries and reports IOError.
		fx.spy.setFailFrom(2)
		err := fx.v.Delete(fx.journal, false)
		if err != sqlite3vfs.IOError { // [as-is] vfs.go:1386 returns the bare IOError; a wrapped error would degrade at the cgo boundary.
			t.Fatalf("Delete with every delete after the header failing = %v, want sqlite3vfs.IOError", err)
		}
		calls := fx.spy.deleteCalls()
		if len(calls) == 0 || calls[0] != fx.sectorName(0) { // [as-is] vfs.go:1389 deleteSectorsInSafeOrder sorts header-first for "-journal" names.
			t.Fatalf("first configmap delete = %v, want the header sector %q first", calls, fx.sectorName(0))
		}
		// [as-is] Exactly the header went: the later sectors survive, and so
		// does the lockfile (it is deleted after all sectors, vfs.go:1360).
		fx.assertSectorNames(t, 1, 2)
		fx.assertNoSectorConfigMap(t, fx.sectorName(0))
		if !sectorConfigMapExists(t, fx.fcs, fx.jf.LockFileName()) {
			t.Error("the journal's lockfile is gone after only the header sector was deleted, want it present (the lockfile goes last)")
		}
		// [as-is] Access answers from the sector count, so the name still
		// exists: SQLite is told there is a journal name here.
		if got, err := fx.v.Access(fx.journal, sqlite3vfs.AccessExists); err != nil || !got {
			t.Errorf("Access(%q) with sectors 1..2 left = (%v, %v), want (true, nil)", fx.journal, got, err)
		}
		fx.assertDatabaseUntouched(t)
	})

	// 2. The invariant that makes half-state 1 safe: with the header gone, the
	// first byte at offset 0 reads as zero, which is what hasHotJournal tests.
	t.Run("header-gone-offset-zero-reads-zero", func(t *testing.T) {
		fx := newHalfStateFixture(t, "half-state-2.db")

		// Control: before the delete the header byte is the non-zero magic
		// byte, i.e. the journal is hot.
		if got := journalMagicByte(t, fx.v, fx.fcs, fx.dbName); got != journalHeaderFirstByte {
			t.Fatalf("setup: journal magic byte = 0x%02x, want 0x%02x (the fixture must build a hot journal)", got, journalHeaderFirstByte)
		}

		// The same crash as subtest 1: the header sector goes, the rest stays.
		fx.spy.setFailFrom(2)
		if err := fx.v.Delete(fx.journal, false); err != sqlite3vfs.IOError {
			t.Fatalf("Delete leaving the half-state = %v, want sqlite3vfs.IOError", err)
		}
		fx.assertSectorNames(t, 1, 2)

		// Open the journal as a reader would and read offset 0. The journal
		// still has sectors, so Open must not treat it as fresh.
		f := openTestFile(t, fx.v, fx.journal)
		size, err := f.FileSize()
		if err != nil {
			t.Fatalf("FileSize on the half-deleted journal: %v", err)
		}
		if size <= 0 {
			t.Fatalf("FileSize on the half-deleted journal = %d, want > 0 (sectors 1 and 2 remain)", size)
		}
		buf := make([]byte, 16)
		for i := range buf {
			buf[i] = 0xff
		}
		n, err := f.ReadAt(buf, 0)
		// [as-is] The header sector is a hole: ReadAt returns the hole's zero
		// bytes (none of them) as a short read with io.EOF (vfs.go:353-420).
		if n != 0 || !errors.Is(err, io.EOF) {
			t.Errorf("ReadAt(16 bytes @0) on the half-deleted journal = (%d, %v), want (0, io.EOF)", n, err)
		}
		// [as-is] A hole yields no bytes, so the caller's buffer keeps whatever
		// it held. SQLite's own hasHotJournal path asks for one byte at offset
		// 0 and takes a short read as "not hot"; the byte this VFS can supply
		// there is zero, so the journal is cold.
		for i := range buf {
			if buf[i] != 0xff {
				t.Errorf("ReadAt with a hole at offset 0 wrote at least %d byte(s) into the buffer; the hole must contribute none", n)
				break
			}
		}
		// Read the one byte SQLite's hasHotJournal reads, at offset 0, through
		// the same file object: a short read of zero bytes is the answer "there
		// is no non-zero magic byte here", so the journal reads as cold even
		// though it is still accessible (subtest 1). The single byte of the
		// buffer stays untouched, so no non-zero header byte can leak out.
		one := make([]byte, 1)
		nOne, oneErr := f.ReadAt(one, 0)
		if nOne != 0 || !errors.Is(oneErr, io.EOF) {
			t.Errorf("ReadAt(1 byte @0) on the half-deleted journal = (%d, %v), want (0, io.EOF): the magic byte must not be readable", nOne, oneErr)
		}
		if one[0] == journalHeaderFirstByte {
			t.Error("the VFS still supplies the journal's original magic byte at offset 0, so hasHotJournal could call the half-deleted journal hot")
		}
		// The explanation for that answer, read straight from the cluster: the
		// sector holding offset 0 is gone, and no surviving sector is sector 0.
		if fx.hasSectorZeroConfigMap(t) {
			t.Error("journal sector 0 still has a ConfigMap, so the zero first byte cannot be a hole")
		}
		fx.assertDatabaseUntouched(t)
	})

	// 3. The pitfall state: the header is present, the later sectors are gone
	// (what an out-of-order delete, or deleting the lockfile first and letting
	// another client re-create the journal, would produce).
	t.Run("header-present-later-sectors-gone", func(t *testing.T) {
		v, fcs := newTestVFS(t)
		spy := &halfStateDeleteSpy{}
		fcs.PrependReactor("delete", "configmaps", spy.reactor)
		journal := "half-state-3.db-journal"
		dbName := "half-state-3.db"

		jf := NewFile(journal, v)
		header := make([]byte, 512)
		fillPattern(header)
		header[0] = journalHeaderFirstByte
		seedSector(t, fcs, jf, 0, header)
		// Sectors 1 and 2 were never written, and neither was a lockfile.
		df := openTestFile(t, v, dbName)
		seedMultiSectorFile(t, fcs, df, multiSectorLen)

		before := configMapsInNamespace(t, fcs)
		if got := len(listSectorConfigMaps(t, fcs, jf)); got != 1 {
			t.Fatalf("setup: journal has %d sector configmaps, want only sector 0", got)
		}

		// A reader opens the journal. Open finds a lockfile-less name: it
		// creates the lockfile through the CAS (vfs.go:1234-1245) — [as-is] a
		// lockfile is re-created by the very open that finds the journal — and
		// sees one sector, so it does not create an empty sector 0.
		f := openTestFile(t, v, journal)

		size, err := f.FileSize()
		if err != nil {
			t.Fatalf("FileSize: %v", err)
		}
		// [as-is] sizeFromSectorList (sector.go:222) takes the largest
		// index*SectorSize+len(data) over the sectors that exist, so the
		// journal looks 512 bytes long although sectors 1 and 2 are missing.
		if size != 512 {
			t.Errorf("FileSize with only sector 0 present = %d, want 512 (the size of the sectors that remain)", size)
		}

		// A read of the range spanning the missing sector: the hole contributes
		// no bytes, so this is a short read ending in io.EOF — NOT an error, and
		// not a panic.
		buf := make([]byte, 4096)
		n, err := f.ReadAt(buf, 0)
		if !errors.Is(err, io.EOF) {
			t.Errorf("ReadAt spanning the missing sectors err = %v, want io.EOF (errSectorNotFound is treated as empty bytes)", err)
		}
		// [as-is] The read stops where the file appears to end: 512 bytes, the
		// only sector that exists.
		if n != 512 {
			t.Errorf("ReadAt spanning the missing sectors = %d bytes, want 512 (a short read of what exists)", n)
		}
		if n > 0 && buf[0] != journalHeaderFirstByte {
			t.Errorf("first byte read from the present header sector = 0x%02x, want 0x%02x", buf[0], journalHeaderFirstByte)
		}
		// Reading past the end is (0, io.EOF), as a hole-free file would be.
		if n, err := f.ReadAt(make([]byte, 8), 4096); n != 0 || !errors.Is(err, io.EOF) {
			t.Errorf("ReadAt past the apparent end = (%d, %v), want (0, io.EOF)", n, err)
		}

		// [as-is] The read created nothing: the missing sectors are still
		// missing, and the only ConfigMap that appeared is the lockfile Open
		// created for the previously lock-less name (sector.go:194 getSector
		// never writes; the imported code created the sector on read).
		if _, err := fcs.CoreV1().ConfigMaps(testNamespace).Get(t.Context(), jf.sectorNameFromSectorIndex(1), metav1.GetOptions{}); !kerrors.IsNotFound(err) {
			t.Errorf("sector 1 after a read into the hole = %v, want NotFound (a read must not create a sector)", err)
		}
		if _, err := fcs.CoreV1().ConfigMaps(testNamespace).Get(t.Context(), jf.sectorNameFromSectorIndex(2), metav1.GetOptions{}); !kerrors.IsNotFound(err) {
			t.Errorf("sector 2 after a read into the hole = %v, want NotFound (a read must not create a sector)", err)
		}
		if got := len(listSectorConfigMaps(t, fcs, jf)); got != 1 {
			t.Errorf("journal sectors after the reads = %d, want 1 (sector 0 only)", got)
		}
		after := configMapsInNamespace(t, fcs)
		// Exactly one ConfigMap may have appeared — the lockfile Open created.
		// It is recorded explicitly rather than assumed, because "the read
		// created a sector" is the hazard this assertion exists to catch.
		if after != before+1 {
			t.Errorf("ConfigMaps in the namespace went from %d to %d, want exactly one more (the lockfile Open created, not a sector)", before, after)
		}
		if !sectorConfigMapExists(t, fcs, jf.LockFileName()) {
			t.Error("Open on the lock-less journal did not create a lockfile, so the one extra ConfigMap is unexpected")
		}
		// The same-stem database is untouched here too.
		if got := len(listSectorConfigMaps(t, fcs, df)); got != 2 {
			t.Errorf("same-stem database sectors = %d, want 2", got)
		}
		if !sectorConfigMapExists(t, fcs, df.LockFileName()) {
			t.Error("same-stem database lockfile is missing")
		}
	})

	// 4. Leftovers are collected: the next delete of the name converges, and a
	// repeat delete of an already-deleted journal is a no-op that succeeds.
	t.Run("later-delete-collects-leftovers", func(t *testing.T) {
		fx := newHalfStateFixture(t, "half-state-4.db")

		// Reach the half-state the same way a crash does.
		fx.spy.setFailFrom(2)
		if err := fx.v.Delete(fx.journal, false); err != sqlite3vfs.IOError {
			t.Fatalf("Delete leaving the half-state = %v, want sqlite3vfs.IOError", err)
		}
		fx.assertSectorNames(t, 1, 2)

		// The resumed delete: no reactor failure, so it collects every leftover
		// and then the lockfile.
		fx.spy.stopFailing()
		if err := fx.v.Delete(fx.journal, false); err != nil {
			t.Fatalf("resumed Delete = %v, want nil", err)
		}
		fx.assertSectorNames(t) // none
		if _, err := fx.fcs.CoreV1().ConfigMaps(testNamespace).Get(t.Context(), fx.jf.LockFileName(), metav1.GetOptions{}); !kerrors.IsNotFound(err) {
			t.Errorf("journal lockfile after the resumed delete = %v, want NotFound", err)
		}
		// [as-is] Access answers false once the last sector is gone: the name
		// stops existing exactly then, and the header went first.
		if got, err := fx.v.Access(fx.journal, sqlite3vfs.AccessExists); err != nil || got {
			t.Errorf("Access(%q) after the resumed delete = (%v, %v), want (false, nil)", fx.journal, got, err)
		}
		// The whole journal is gone, not just its sectors.
		if n := len(sectorConfigMapsNamed(t, fx.fcs, fx.journal)); n != 0 {
			t.Errorf("%d ConfigMaps still claim filename %q, want 0", n, fx.journal)
		}

		// A second Delete of the already-deleted journal succeeds: SQLite
		// deletes the journal after every transaction, sometimes one it never
		// created (vfs_test.go TestDeleteNonexistentJournalSucceeds).
		beforeCalls := len(fx.spy.deleteCalls())
		if err := fx.v.Delete(fx.journal, false); err != nil {
			t.Errorf("second Delete of the deleted journal = %v, want nil", err)
		}
		// [as-is] The second pass deletes nothing but the lockfile name, whose
		// NotFound the code treats as success (vfs.go:1363).
		calls := fx.spy.deleteCalls()[beforeCalls:]
		if len(calls) != 1 || calls[0] != fx.jf.LockFileName() {
			t.Errorf("delete calls of the second Delete = %v, want exactly the lockfile name %q", calls, fx.jf.LockFileName())
		}
		// And the database of the same stem is still whole.
		fx.assertDatabaseUntouched(t)
	})

	// 6. The latent hazard this ordering leaves. After the header sector is gone
	// the name still exists and still has a size, so an ordinary write to offset
	// 0 re-CREATES sector 0 (a write creates every sector it touches,
	// sector.go:96) and can install a non-zero first byte: a cold, half-deleted
	// journal becomes a hot one again, and SQLite would try to replay the
	// leftovers as a rollback. Nothing in the delete path owns this — the write
	// path cannot know the name is a journal whose delete was interrupted — so it
	// is pinned here as executable evidence rather than a comment.
	t.Run("write-into-the-hole-can-resurrect-a-hot-header", func(t *testing.T) {
		fx := newHalfStateFixture(t, "half-state-6.db")

		fx.spy.setFailFrom(2)
		if err := fx.v.Delete(fx.journal, false); err != sqlite3vfs.IOError {
			t.Fatalf("Delete leaving the half-state = %v, want sqlite3vfs.IOError", err)
		}
		fx.assertSectorNames(t, 1, 2)
		if fx.hasSectorZeroConfigMap(t) {
			t.Fatal("setup: sector 0 survived the half-delete, so this subtest is not testing a hole")
		}

		f := openTestFile(t, fx.v, fx.journal)
		size, err := f.FileSize()
		if err != nil {
			t.Fatalf("FileSize: %v", err)
		}
		// [as-is] FileSize still counts the leftover sectors, so the cold
		// journal looks as long as the hot one did.
		if want := int64(2*SectorSize + 1234); size != want {
			t.Errorf("FileSize of the half-deleted journal = %d, want %d (the sectors that remain)", size, want)
		}

		// The write that resurrects the header: four bytes at offset 0, exactly
		// what a page write into the file's first page does.
		written := []byte{0xde, 0xad, 0xbe, 0xef}
		n, err := f.WriteAt(written, 0)
		if err != nil || n != len(written) {
			t.Fatalf("WriteAt(4 bytes @0) = (%d, %v), want (%d, nil)", n, err, len(written))
		}
		// [would-need-a-change] Sector 0 is back, first byte non-zero: the
		// journal SQLite reads as cold is hot again, over leftover payload.
		// deleteSectorsInSafeOrder's comment (vfs.go:1389) says a crashed delete
		// leaves "unreachable sectors" that only "the next delete or a truncate"
		// collects; this shows a WRITE re-reaches them first. A production fix
		// would have to keep the name from being written while its delete is
		// incomplete (a tombstone, or deleting the lockfile before the sectors so
		// a writer must re-create the file), because the write path cannot tell a
		// half-deleted journal from a page recycle.
		if !fx.hasSectorZeroConfigMap(t) {
			t.Fatal("WriteAt did not create sector 0: the resurrection this subtest documents did not happen")
		}
		if got := journalMagicByte(t, fx.v, fx.fcs, fx.dbName); got != 0xde {
			t.Errorf("journal magic byte after the write = 0x%02x, want 0xde (the byte just written)", got)
		}
		// A reader sees that non-zero first byte too, so hasHotJournal answers
		// "hot" for a journal whose payload is already-garbage leftovers.
		head := make([]byte, len(written))
		if n, err := f.ReadAt(head, 0); n != len(written) || err != nil {
			t.Errorf("ReadAt of the resurrected header = (%d, %v), want (%d, nil)", n, err, len(written))
		} else if head[0] != 0xde {
			t.Errorf("first byte at offset 0 after the write = 0x%02x, want 0xde", head[0])
		}

		// The interrupted delete still collects the leftovers once it can run:
		// the hazard is a window, not a permanent leak.
		fx.spy.stopFailing()
		if err := fx.v.Delete(fx.journal, false); err != nil {
			t.Fatalf("resumed Delete = %v, want nil", err)
		}
		fx.assertSectorNames(t)
		if got, err := fx.v.Access(fx.journal, sqlite3vfs.AccessExists); err != nil || got {
			t.Errorf("Access(%q) after the resumed delete = (%v, %v), want (false, nil)", fx.journal, got, err)
		}
		fx.assertDatabaseUntouched(t)
	})

	// 5. Idempotence under repeated failure: a delete whose every call fails
	// damages nothing and reports IOError; once the API recovers, one more
	// Delete converges to fully deleted with nil.
	t.Run("repeated-failure-is-idempotent", func(t *testing.T) {
		fx := newHalfStateFixture(t, "half-state-5.db")

		// Every Delete fails: the sector loop removes nothing at all, and
		// Delete burns its retries and reports IOError.
		fx.spy.setFailFrom(1)
		err := fx.v.Delete(fx.journal, false)
		if err != sqlite3vfs.IOError { // [as-is] vfs.go:1386.
			t.Fatalf("Delete with every configmap delete failing = %v, want sqlite3vfs.IOError", err)
		}
		// [as-is] Not one sector went: each failed call left its ConfigMap
		// untouched, so "no more than the header sector was removed" holds with
		// room to spare (nothing was).
		fx.assertSectorNames(t, 0, 1, 2)
		if !sectorConfigMapExists(t, fx.fcs, fx.jf.LockFileName()) {
			t.Error("the lockfile is gone after a delete whose sector deletes all failed, want it present")
		}
		// The VFS retried: retries=3 in newTestVFS means 4 passes over the
		// sector list, each one attempting all three sectors.
		if got := len(fx.spy.deleteCalls()); got != 12 {
			t.Errorf("configmap delete attempts under permanent failure = %d, want 12 (4 passes x 3 sectors)", got)
		}
		// Access is still true throughout: the journal's name never stopped
		// existing, and its header is still there.
		if got, err := fx.v.Access(fx.journal, sqlite3vfs.AccessExists); err != nil || !got {
			t.Errorf("Access(%q) after the failed delete = (%v, %v), want (true, nil)", fx.journal, got, err)
		}

		// The API recovers: one more Delete converges.
		fx.spy.stopFailing()
		if err := fx.v.Delete(fx.journal, false); err != nil {
			t.Fatalf("Delete after the API recovered = %v, want nil", err)
		}
		fx.assertSectorNames(t)
		if _, err := fx.fcs.CoreV1().ConfigMaps(testNamespace).Get(t.Context(), fx.jf.LockFileName(), metav1.GetOptions{}); !kerrors.IsNotFound(err) {
			t.Errorf("journal lockfile after the recovered delete = %v, want NotFound", err)
		}
		if got, err := fx.v.Access(fx.journal, sqlite3vfs.AccessExists); err != nil || got {
			t.Errorf("Access(%q) after the recovered delete = (%v, %v), want (false, nil)", fx.journal, got, err)
		}
		// Nothing of the journal is left, and the database is untouched.
		if n := len(sectorConfigMapsNamed(t, fx.fcs, fx.journal)); n != 0 {
			t.Errorf("%d ConfigMaps still claim filename %q, want 0", n, fx.journal)
		}
		fx.assertDatabaseUntouched(t)

		// A delete of a journal that has only leftovers (no header) and no
		// lockfile also converges: build that state directly and delete it.
		leftover := NewFile("half-state-5-leftovers.db-journal", fx.v)
		seedSector(t, fx.fcs, leftover, 1, make([]byte, 64))
		seedSector(t, fx.fcs, leftover, 2, make([]byte, 64))
		if !sectorConfigMapExists(t, fx.fcs, leftover.LockFileName()) {
			t.Log("note: the leftover-only journal has no lockfile, as a header-first crash leaves it")
		}
		if err := fx.v.Delete(leftover.RawName, false); err != nil {
			t.Errorf("Delete of a header-less leftover journal = %v, want nil", err)
		}
		if names := sectorNames(listSectorConfigMaps(t, fx.fcs, leftover)); len(names) != 0 {
			t.Errorf("leftover journal sectors after Delete = %v, want none", names)
		}
		if got, err := fx.v.Access(leftover.RawName, sqlite3vfs.AccessExists); err != nil || got {
			t.Errorf("Access(%q) after Delete = (%v, %v), want (false, nil)", leftover.RawName, got, err)
		}
	})
}

// sectorConfigMapExists reports whether a ConfigMap of the given name is
// present in the test namespace.
func sectorConfigMapExists(t *testing.T, fcs *fake.Clientset, name string) bool {
	t.Helper()
	_, err := fcs.CoreV1().ConfigMaps(testNamespace).Get(t.Context(), name, metav1.GetOptions{})
	return err == nil
}

// configMapsInNamespace counts every ConfigMap in the test namespace, whatever
// its labels: the before/after witness that a read created nothing.
func configMapsInNamespace(t *testing.T, fcs *fake.Clientset) int {
	t.Helper()
	all, err := fcs.CoreV1().ConfigMaps(testNamespace).List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list configmaps: %v", err)
	}
	return len(all.Items)
}

// hasSectorZeroConfigMap reports whether the journal still has a sector 0
// ConfigMap — the sector holding file offset 0, i.e. SQLite's journal header.
func (fx *halfStateFixture) hasSectorZeroConfigMap(t *testing.T) bool {
	t.Helper()
	for _, cm := range listSectorConfigMaps(t, fx.fcs, fx.jf) {
		if cm.Name == fx.sectorName(0) {
			return true
		}
	}
	return false
}
