package vfs

// Behaviour tests for the kube VFS, run entirely against the fake kubernetes
// clientset so no cluster is needed.
//
// These tests pin the optimised behaviour of issue #8: writes grow a file
// across any number of 64 KiB sectors (WriteAt fetches each sector at most
// once and writes each changed sector exactly once), reads never create
// missing sectors and treat holes as short reads/EOF, FileSize is O(1) from
// memory while a SHARED-or-better lock is held (and otherwise costs one
// revalidating LIST that also warms the sector cache), and lock state is
// tracked locally rather than re-GET per transition.
//
// Where a test wants to start from a specific multi-sector layout it seeds
// the sector ConfigMaps directly into the fake clientset; growth is pinned
// by TestWriteAtGrowIntoNewSectorsRoundTrip, and the multi-sector path is
// also exercised through real SQLite at 64 KiB pages (the cmd tools'
// configuration) by TestSQLiteMultiSector64KiBPages.

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/psanford/sqlite3vfs"
	"go.uber.org/zap"
	v1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const testNamespace = "vfs-tests"

var (
	vfsNameCounter uint64
	dbFileCounter  uint64
	sectorSize     = int64(SectorSize)
	multiSectorLen = 70000 // spans two 64 KiB sectors
)

// newTestVFS returns a vfs backed by an in-memory fake clientset.
func newTestVFS(tb testing.TB) (*vfs, *fake.Clientset) {
	tb.Helper()
	fcs := fake.NewSimpleClientset()
	return NewVFS(fcs, testNamespace, zap.NewNop().Sugar(), 3), fcs
}

// openTestFile opens name through the VFS itself, as sqlite would.
func openTestFile(tb testing.TB, v *vfs, name string) *file {
	tb.Helper()
	sf, _, err := v.Open(name, 0)
	if err != nil {
		tb.Fatalf("Open(%q) failed: %v", name, err)
	}
	f, ok := sf.(*file)
	if !ok {
		tb.Fatalf("Open(%q) returned %T, want *file", name, sf)
	}
	return f
}

// registerUniqueVFS registers the vfs under a process-unique name:
// sqlite3vfs.RegisterVFS is process-global, so every test needs its own.
func registerUniqueVFS(tb testing.TB, v *vfs) string {
	tb.Helper()
	name := fmt.Sprintf("kube-vfs-test-%d", atomic.AddUint64(&vfsNameCounter, 1))
	if err := sqlite3vfs.RegisterVFS(name, v); err != nil {
		tb.Fatalf("RegisterVFS(%q) failed: %v", name, err)
	}
	return name
}

func newSQLFileName() string {
	return fmt.Sprintf("sql-test-%d.db", atomic.AddUint64(&dbFileCounter, 1))
}

// openSQLDB opens a sqlite database backed by the named registered VFS,
// mirroring the DSN the module's own main uses (in-memory journal).
func openSQLDB(tb testing.TB, vfsName, fileName string) *sql.DB {
	tb.Helper()
	dsn := fmt.Sprintf("file:%s?_journal=MEMORY&vfs=%s", fileName, vfsName)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		tb.Fatalf("sql.Open(%q) failed: %v", dsn, err)
	}
	// One connection at a time, as the module's main does: the VFS has no
	// real multi-connection locking story.
	db.SetMaxOpenConns(1)
	return db
}

func fillPattern(p []byte) {
	for i := range p {
		p[i] = byte(i % 251)
	}
}

// fillPatternAlt fills with a different repeating pattern than fillPattern.
func fillPatternAlt(p []byte) {
	for i := range p {
		p[i] = byte((i + 17) % 251)
	}
}

// seedSector writes a sector ConfigMap into the fake clientset directly,
// with exactly the name, labels and keys the VFS's own WriteSector uses.
// This stands in for state written by another process, and lets tests build
// multi-sector files despite the write-grow panic described at the top.
func seedSector(tb testing.TB, fcs *fake.Clientset, f *file, index int64, data []byte) {
	tb.Helper()
	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      f.sectorNameFromSectorIndex(index),
			Namespace: testNamespace,
			Labels:    f.SectorLabels,
		},
		BinaryData: map[string][]byte{"sector": data},
		Data:       map[string]string{"filename": f.RawName},
	}
	if _, err := fcs.CoreV1().ConfigMaps(testNamespace).Create(context.TODO(), cm, metav1.CreateOptions{}); err != nil {
		if !kerrors.IsAlreadyExists(err) {
			tb.Fatalf("seed sector %d: %v", index, err)
		}
		// Open() pre-creates an empty sector 0; overwrite it.
		if _, err := fcs.CoreV1().ConfigMaps(testNamespace).Update(context.TODO(), cm, metav1.UpdateOptions{}); err != nil {
			tb.Fatalf("seed (update) sector %d: %v", index, err)
		}
	}
}

// seedMultiSectorFile seeds sectors 0 and 1 holding the first n bytes of a
// fillPattern buffer.
func seedMultiSectorFile(tb testing.TB, fcs *fake.Clientset, f *file, n int) []byte {
	tb.Helper()
	data := make([]byte, n)
	fillPattern(data)
	seedSector(tb, fcs, f, 0, data[:SectorSize])
	if n > int(SectorSize) {
		seedSector(tb, fcs, f, 1, data[SectorSize:])
	}
	return data
}

func listSectorConfigMaps(tb testing.TB, fcs *fake.Clientset, f *file) []v1.ConfigMap {
	tb.Helper()
	cms, err := fcs.CoreV1().ConfigMaps(testNamespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(f.SectorLabels).String(),
	})
	if err != nil {
		tb.Fatalf("list sector configmaps: %v", err)
	}
	return cms.Items
}

func getLockfileConfigMap(tb testing.TB, fcs *fake.Clientset, f *file) *v1.ConfigMap {
	tb.Helper()
	cm, err := fcs.CoreV1().ConfigMaps(testNamespace).Get(context.TODO(), f.LockFileName(), metav1.GetOptions{})
	if err != nil {
		tb.Fatalf("get lockfile %q: %v", f.LockFileName(), err)
	}
	return cm
}

func lockState(tb testing.TB, fcs *fake.Clientset, f *file) string {
	tb.Helper()
	return getLockfileConfigMap(tb, fcs, f).Data["lock"]
}

func mustWriteAt(tb testing.TB, f *file, p []byte, off int64) int {
	tb.Helper()
	n, err := f.WriteAt(p, off)
	if err != nil {
		tb.Fatalf("WriteAt(len=%d, off=%d) failed: %v", len(p), off, err)
	}
	if n != len(p) {
		tb.Fatalf("WriteAt(len=%d, off=%d) wrote %d bytes", len(p), off, n)
	}
	return n
}

func mustReadAt(tb testing.TB, f *file, p []byte, off int64) int {
	tb.Helper()
	n, err := f.ReadAt(p, off)
	if err != nil && err != io.EOF {
		tb.Fatalf("ReadAt(len=%d, off=%d) failed: %v", len(p), off, err)
	}
	return n
}

// --- Open ---

func TestOpenFreshFileCreatesLockfileAndEmptySector(t *testing.T) {
	v, fcs := newTestVFS(t)
	f := openTestFile(t, v, "fresh.db")

	lf := getLockfileConfigMap(t, fcs, f)
	if got := lf.Data["lock"]; got != sqlite3vfs.LockNone.String() {
		t.Errorf("fresh lockfile lock = %q, want %q", got, sqlite3vfs.LockNone.String())
	}
	if got := lf.Data["relevant-file"]; got != string(f.b32ByteFromString("fresh.db")) {
		t.Errorf("lockfile relevant-file = %q, want the base32 file label", got)
	}

	sectors := listSectorConfigMaps(t, fcs, f)
	if len(sectors) != 1 {
		t.Fatalf("fresh file has %d sector configmaps, want exactly 1", len(sectors))
	}
	if got := sectors[0].BinaryData["sector"]; len(got) != 0 {
		t.Errorf("fresh sector 0 holds %d bytes, want empty", len(got))
	}

	size, err := f.FileSize()
	if err != nil {
		t.Fatalf("FileSize on fresh file: %v", err)
	}
	if size != 0 {
		t.Errorf("FileSize on fresh file = %d, want 0", size)
	}
}

func TestOpenExistingFileKeepsDataAndSectors(t *testing.T) {
	v, fcs := newTestVFS(t)
	f1 := openTestFile(t, v, "existing.db")

	data := make([]byte, 1000)
	fillPattern(data)
	mustWriteAt(t, f1, data, 0)
	if err := f1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	before := listSectorConfigMaps(t, fcs, f1)
	if len(before) != 1 {
		t.Fatalf("before reopen: %d sector configmaps, want 1", len(before))
	}

	f2 := openTestFile(t, v, "existing.db")

	after := listSectorConfigMaps(t, fcs, f2)
	if len(after) != len(before) {
		t.Errorf("reopen changed the sector count: %d -> %d, want unchanged", len(before), len(after))
	}
	size, err := f2.FileSize()
	if err != nil {
		t.Fatalf("FileSize after reopen: %v", err)
	}
	if size != int64(len(data)) {
		t.Errorf("FileSize after reopen = %d, want %d", size, len(data))
	}
	got := make([]byte, len(data))
	if n := mustReadAt(t, f2, got, 0); n != len(data) {
		t.Fatalf("read after reopen got %d bytes, want %d", n, len(data))
	}
	if !bytes.Equal(got, data) {
		t.Error("data changed across reopen")
	}
}

// --- WriteAt ---

func TestWriteAtSingleSectorRoundTrip(t *testing.T) {
	v, fcs := newTestVFS(t)
	f := openTestFile(t, v, "roundtrip.db")

	data := make([]byte, 1000)
	fillPattern(data)
	mustWriteAt(t, f, data, 0)

	if sectors := listSectorConfigMaps(t, fcs, f); len(sectors) != 1 {
		t.Fatalf("1000 byte write produced %d sectors, want 1", len(sectors))
	}
	got := make([]byte, 1000)
	if n := mustReadAt(t, f, got, 0); n != 1000 {
		t.Fatalf("read back %d bytes, want 1000", n)
	}
	if !bytes.Equal(got, data) {
		t.Error("read back data differs from what was written")
	}
}

func TestWriteAtFullSectorOverwrite(t *testing.T) {
	v, _ := newTestVFS(t)
	f := openTestFile(t, v, "fullsector.db")

	data := make([]byte, SectorSize)
	fillPattern(data)
	mustWriteAt(t, f, data, 0)

	got := make([]byte, SectorSize)
	if n := mustReadAt(t, f, got, 0); n != len(data) {
		t.Fatalf("read back %d bytes, want %d", n, len(data))
	}
	if !bytes.Equal(got, data) {
		t.Error("full-sector round trip differs")
	}

	size, err := f.FileSize()
	if err != nil {
		t.Fatalf("FileSize: %v", err)
	}
	if size != int64(len(data)) {
		t.Errorf("FileSize = %d, want %d", size, len(data))
	}
}

// TestWriteAtMultiSectorOverExisting rewrites a whole two-sector file in one
// WriteAt. Both sectors already exist, so no unwritten sector is touched.
func TestWriteAtMultiSectorOverExisting(t *testing.T) {
	v, fcs := newTestVFS(t)
	f := openTestFile(t, v, "multisector.db")
	seedMultiSectorFile(t, fcs, f, multiSectorLen)

	data := make([]byte, multiSectorLen)
	fillPatternAlt(data)
	mustWriteAt(t, f, data, 0)

	if sectors := listSectorConfigMaps(t, fcs, f); len(sectors) != 2 {
		t.Fatalf("%d byte write left %d sectors, want 2", multiSectorLen, len(sectors))
	}
	got := make([]byte, multiSectorLen)
	if n := mustReadAt(t, f, got, 0); n != multiSectorLen {
		t.Fatalf("read back %d bytes, want %d", n, multiSectorLen)
	}
	if !bytes.Equal(got, data) {
		t.Error("read back data differs from what was written")
	}

	size, err := f.FileSize()
	if err != nil {
		t.Fatalf("FileSize: %v", err)
	}
	if size != int64(multiSectorLen) {
		t.Errorf("FileSize = %d, want %d", size, multiSectorLen)
	}
}

func TestWriteAtPartialOverwriteKeepsSurroundingBytes(t *testing.T) {
	v, fcs := newTestVFS(t)
	f := openTestFile(t, v, "overwrite.db")
	seedMultiSectorFile(t, fcs, f, multiSectorLen)

	replacement := []byte("REPLACED-REPLACED-REPLACED")
	mustWriteAt(t, f, replacement, 100)

	want := make([]byte, multiSectorLen)
	fillPattern(want)
	copy(want[100:100+len(replacement)], replacement)

	got := make([]byte, multiSectorLen)
	if n := mustReadAt(t, f, got, 0); n != multiSectorLen {
		t.Fatalf("read back %d bytes, want %d", n, multiSectorLen)
	}
	if !bytes.Equal(got, want) {
		t.Error("partial overwrite damaged bytes outside the overwritten range")
	}

	if sectors := listSectorConfigMaps(t, fcs, f); len(sectors) != 2 {
		t.Errorf("partial overwrite left %d sectors, want 2", len(sectors))
	}
}

// TestWriteAtGrowIntoNewSectorsRoundTrip replaces
// TestWriteAtIntoUnwrittenSectorPanicsDocumentsCurrentBehaviour, which
// pinned the worst defect of the code as imported: getSector wrote an empty
// sector for anything missing and then dereferenced the nil response, so ANY
// write reaching an unwritten sector panicked — capping every file at one
// 64 KiB sector and crashing SQLite (unrecoverably, through cgo) the moment
// a database grew past its first page. getSector no longer creates sectors
// and WriteAt handles missing ones itself, so growth works.
func TestWriteAtGrowIntoNewSectorsRoundTrip(t *testing.T) {
	v, fcs := newTestVFS(t)
	f := openTestFile(t, v, "grow.db") // only the empty sector 0 exists

	data := make([]byte, multiSectorLen)
	fillPattern(data)
	mustWriteAt(t, f, data, 0)

	if sectors := listSectorConfigMaps(t, fcs, f); len(sectors) != 2 {
		t.Fatalf("grow write left %d sector configmaps, want 2", len(sectors))
	}
	got := make([]byte, multiSectorLen)
	if n := mustReadAt(t, f, got, 0); n != multiSectorLen {
		t.Fatalf("read back %d bytes, want %d", n, multiSectorLen)
	}
	if !bytes.Equal(got, data) {
		t.Error("read back data differs from what was written")
	}
	size, err := f.FileSize()
	if err != nil {
		t.Fatalf("FileSize: %v", err)
	}
	if size != int64(multiSectorLen) {
		t.Errorf("FileSize = %d, want %d", size, multiSectorLen)
	}
}

// --- ReadAt ---

func TestReadAtAcrossSectorBoundary(t *testing.T) {
	v, fcs := newTestVFS(t)
	f := openTestFile(t, v, "across.db")
	data := seedMultiSectorFile(t, fcs, f, multiSectorLen)

	off := int64(SectorSize) - 36 // 36 bytes before the boundary
	got := make([]byte, 100)
	if n := mustReadAt(t, f, got, off); n != 100 {
		t.Fatalf("read got %d bytes, want 100", n)
	}
	if !bytes.Equal(got, data[off:off+100]) {
		t.Error("read across the sector boundary returned wrong bytes")
	}
}

func TestReadAtShortReadReturnsEOF(t *testing.T) {
	v, _ := newTestVFS(t)
	f := openTestFile(t, v, "shortread.db")

	data := make([]byte, 1000)
	fillPattern(data)
	mustWriteAt(t, f, data, 0)

	got := make([]byte, 2000) // longer than the file
	n, err := f.ReadAt(got, 0)
	if n != len(data) {
		t.Errorf("short read got %d bytes, want %d", n, len(data))
	}
	if err != io.EOF {
		t.Errorf("short read err = %v, want io.EOF", err)
	}
	if !bytes.Equal(got[:n], data) {
		t.Error("short read returned wrong bytes")
	}
}

func TestReadAtPastEndReturnsEOF(t *testing.T) {
	v, _ := newTestVFS(t)
	f := openTestFile(t, v, "pastend.db")

	data := make([]byte, 1000)
	fillPattern(data)
	mustWriteAt(t, f, data, 0)

	got := make([]byte, 100)
	for _, off := range []int64{1000, 5000} {
		n, err := f.ReadAt(got, off)
		if n != 0 {
			t.Errorf("ReadAt(off=%d) got %d bytes, want 0", off, n)
		}
		if err != io.EOF {
			t.Errorf("ReadAt(off=%d) err = %v, want io.EOF", off, err)
		}
	}

	// Same for a never-written (empty) file.
	empty := openTestFile(t, v, "pastend-empty.db")
	if n, err := empty.ReadAt(got, 0); n != 0 || err != io.EOF {
		t.Errorf("ReadAt on empty file got (%d, %v), want (0, io.EOF)", n, err)
	}
}

// --- FileSize ---

func TestFileSizeExact(t *testing.T) {
	v, fcs := newTestVFS(t)
	f := openTestFile(t, v, "size.db")

	data := make([]byte, 1000)
	fillPattern(data)
	mustWriteAt(t, f, data, 0)
	if size, err := f.FileSize(); err != nil || size != 1000 {
		t.Errorf("FileSize after 1000 byte write = (%d, %v), want (1000, nil)", size, err)
	}

	data = make([]byte, SectorSize)
	fillPattern(data)
	mustWriteAt(t, f, data, 0)
	if size, err := f.FileSize(); err != nil || size != int64(SectorSize) {
		t.Errorf("FileSize after %d byte write = (%d, %v), want (%d, nil)", SectorSize, size, err, SectorSize)
	}

	// A two-sector file (seeded into the fake clientset): the size is
	// lastSectorIndex*SectorSize + len(lastSectorData).
	data = make([]byte, multiSectorLen)
	fillPattern(data)
	seedSector(t, fcs, f, 1, data[SectorSize:])
	if size, err := f.FileSize(); err != nil || size != int64(multiSectorLen) {
		t.Errorf("FileSize on two-sector file = (%d, %v), want (%d, nil)", size, err, multiSectorLen)
	}
}

// --- Truncate ---
//
// Both defects of the imported code are fixed and pinned below: a failing
// sector write fails the whole truncate instead of being swallowed while
// the delete loop carries on (TestTruncatePropagatesWriteSectorError), and
// the delete loop starts after the last surviving sector — the one holding
// the kept prefix, found via sectorForPos(size-1) — so a mid-sector shrink
// keeps its prefix (TestTruncateShrinkMidSectorKeepsPrefix) and a
// sector-aligned shrink drops exactly the sectors past the new size
// (TestTruncateShrinkAlignedToSectorEnd).

func TestTruncateNoOpGrow(t *testing.T) {
	v, _ := newTestVFS(t)
	f := openTestFile(t, v, "noopgrow.db")

	data := make([]byte, 1000)
	fillPattern(data)
	mustWriteAt(t, f, data, 0)

	if err := f.Truncate(2000); err != nil { // grow is a no-op
		t.Fatalf("Truncate(grow) = %v, want nil", err)
	}
	if err := f.Truncate(1000); err != nil { // exact size is a no-op
		t.Fatalf("Truncate(same size) = %v, want nil", err)
	}

	size, err := f.FileSize()
	if err != nil {
		t.Fatalf("FileSize: %v", err)
	}
	if size != 1000 {
		t.Errorf("FileSize after no-op truncate = %d, want 1000", size)
	}
	got := make([]byte, 1000)
	if n := mustReadAt(t, f, got, 0); n != 1000 || !bytes.Equal(got, data) {
		t.Error("no-op truncate damaged the data")
	}
}

func TestTruncateShrinkAlignedToSectorEnd(t *testing.T) {
	// 65537 bytes: one full sector plus a 1-byte second sector. Truncating
	// to 65536 drops the second sector and keeps the first, and succeeds.
	v, fcs := newTestVFS(t)
	f := openTestFile(t, v, "shrink-aligned.db")
	data := seedMultiSectorFile(t, fcs, f, int(sectorSize)+1)

	if err := f.Truncate(sectorSize); err != nil {
		t.Fatalf("Truncate(%d) = %v, want nil", sectorSize, err)
	}

	size, err := f.FileSize()
	if err != nil {
		t.Fatalf("FileSize after truncate: %v", err)
	}
	if size != sectorSize {
		t.Errorf("FileSize after truncate = %d, want %d", size, sectorSize)
	}
	if sectors := listSectorConfigMaps(t, fcs, f); len(sectors) != 1 {
		t.Errorf("%d sector configmaps remain after truncate, want 1", len(sectors))
	}

	got := make([]byte, 100)
	if n := mustReadAt(t, f, got, 0); n != 100 || !bytes.Equal(got, data[:100]) {
		t.Error("truncate damaged the kept prefix")
	}
	if n, err := f.ReadAt(got, sectorSize); n != 0 || err != io.EOF {
		t.Errorf("read past the new EOF got (%d, %v), want (0, io.EOF)", n, err)
	}
}

// TestTruncateShrinkMidSectorKeepsPrefix replaces
// TestTruncateShrinkMidSectorDocumentsCurrentBehaviour, which pinned the
// imported bug: a mid-sector truncate reported success while deleting EVERY
// sector, including the one holding the kept prefix, destroying the file.
// The delete loop now starts after the surviving sector, the surviving
// sector keeps the kept prefix, and sectors beyond the new size are gone.
func TestTruncateShrinkMidSectorKeepsPrefix(t *testing.T) {
	v, fcs := newTestVFS(t)
	f := openTestFile(t, v, "shrink-mid.db")
	data := seedMultiSectorFile(t, fcs, f, multiSectorLen)

	if err := f.Truncate(700); err != nil {
		t.Fatalf("Truncate(700) = %v, want nil", err)
	}

	size, err := f.FileSize()
	if err != nil {
		t.Fatalf("FileSize after truncate: %v", err)
	}
	if size != 700 {
		t.Errorf("FileSize after truncate = %d, want 700", size)
	}
	if sectors := listSectorConfigMaps(t, fcs, f); len(sectors) != 1 {
		t.Fatalf("%d sector configmaps remain after truncate, want 1", len(sectors))
	}

	got := make([]byte, 700)
	if n := mustReadAt(t, f, got, 0); n != 700 {
		t.Fatalf("read back %d bytes, want 700", n)
	}
	if !bytes.Equal(got, data[:700]) {
		t.Error("truncate damaged the kept prefix")
	}
	if n, err := f.ReadAt(got, 700); n != 0 || err != io.EOF {
		t.Errorf("read past the new EOF got (%d, %v), want (0, io.EOF)", n, err)
	}
}

// TestTruncatePropagatesWriteSectorError pins the fix for the imported bug
// of ignoring WriteSector's error in Truncate: a failing sector write must
// fail the truncate instead of silently continuing to delete sectors. This
// test fails without that fix (the error used to be swallowed).
func TestTruncatePropagatesWriteSectorError(t *testing.T) {
	v, fcs := newTestVFS(t)
	f := openTestFile(t, v, "truncerr.db")
	seedMultiSectorFile(t, fcs, f, multiSectorLen)

	boom := errors.New("boom: sector write failed")
	fcs.PrependReactor("update", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, boom
	})

	if err := f.Truncate(700); err == nil {
		t.Fatal("Truncate(700) with a failing sector write = nil, want the write error")
	}
	// Nothing may have been deleted either: the surviving sectors are still
	// both there.
	if sectors := listSectorConfigMaps(t, fcs, f); len(sectors) != 2 {
		t.Errorf("%d sector configmaps remain after the failed truncate, want 2 (no deletions)", len(sectors))
	}
}

// TestReadAtDoesNotCreateSectors pins the read-side of the no-create rule:
// reading — including a read that reaches a sector which does not exist —
// must never write a sector ConfigMap to the cluster. The imported code
// created an empty sector on read (a write to the cluster, from a read!) and
// then panicked dereferencing the nil response it had just replaced.
func TestReadAtDoesNotCreateSectors(t *testing.T) {
	v, fcs := newTestVFS(t)
	f := openTestFile(t, v, "holeread.db")

	data := make([]byte, SectorSize)
	fillPattern(data)
	mustWriteAt(t, f, data, 0)
	if len(listSectorConfigMaps(t, fcs, f)) != 1 {
		t.Fatal("setup: want exactly 1 sector")
	}

	// A read that reaches past the end of the file, into a sector that does
	// not exist, must neither create a sector nor panic.
	got := make([]byte, 64)
	n, err := f.ReadAt(got, SectorSize-10)
	if n != 10 {
		t.Errorf("read reaching the missing sector got %d bytes, want 10 (short read)", n)
	}
	if err != io.EOF {
		t.Errorf("read reaching the missing sector err = %v, want io.EOF", err)
	}
	if sectors := listSectorConfigMaps(t, fcs, f); len(sectors) != 1 {
		t.Errorf("read left %d sector configmaps, want 1 (reads must not create sectors)", len(sectors))
	}
}

// --- Delete ---

func TestDeleteRemovesSectorsAndLockfile(t *testing.T) {
	v, fcs := newTestVFS(t)
	f := openTestFile(t, v, "doomed.db")
	seedMultiSectorFile(t, fcs, f, multiSectorLen)
	if len(listSectorConfigMaps(t, fcs, f)) != 2 {
		t.Fatal("setup: want 2 sectors before delete")
	}

	if err := v.Delete("doomed.db", false); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	all, err := fcs.CoreV1().ConfigMaps(testNamespace).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list all configmaps: %v", err)
	}
	if len(all.Items) != 0 {
		t.Errorf("%d configmaps remain after Delete, want 0 (sectors and lockfile)", len(all.Items))
	}

	// Deleting a file that is already gone must succeed, not error.
	if err := v.Delete("doomed.db", false); err != nil {
		t.Errorf("Delete of already-deleted file = %v, want nil", err)
	}

	// The lockfile is really gone.
	if _, err := fcs.CoreV1().ConfigMaps(testNamespace).Get(context.TODO(), f.LockFileName(), metav1.GetOptions{}); !kerrors.IsNotFound(err) {
		t.Errorf("lockfile get after delete: err = %v, want NotFound", err)
	}
}

// --- Lock transitions ---
//
// The lock levels the sqlite3vfs shim passes through are None=0, Shared=1,
// Reserved=2, Pending=3, Exclusive=4. SQLite never requests Pending
// explicitly (it is implicit in the OS backend), and this VFS rejects it.

func TestLockTransitionsValid(t *testing.T) {
	v, fcs := newTestVFS(t)
	f := openTestFile(t, v, "locks.db")

	if err := f.Lock(sqlite3vfs.LockNone); err != nil {
		t.Fatalf("Lock(None) on an unlocked file = %v, want nil", err)
	}
	if err := f.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("Lock(Shared) = %v, want nil", err)
	}
	if got := lockState(t, fcs, f); got != sqlite3vfs.LockShared.String() {
		t.Errorf("lock after Lock(Shared) = %q, want %q", got, sqlite3vfs.LockShared.String())
	}

	if err := f.Lock(sqlite3vfs.LockReserved); err != nil {
		t.Fatalf("Lock(Reserved) from Shared = %v, want nil", err)
	}
	if err := f.Lock(sqlite3vfs.LockExclusive); err != nil {
		t.Fatalf("Lock(Exclusive) from Reserved = %v, want nil", err)
	}
	if got := lockState(t, fcs, f); got != sqlite3vfs.LockExclusive.String() {
		t.Errorf("lock after Lock(Exclusive) = %q, want %q", got, sqlite3vfs.LockExclusive.String())
	}

	// Requesting a level already held or lower is a silent no-op.
	if err := f.Lock(sqlite3vfs.LockShared); err != nil {
		t.Errorf("Lock(Shared) while Exclusive = %v, want nil", err)
	}
	if got := lockState(t, fcs, f); got != sqlite3vfs.LockExclusive.String() {
		t.Errorf("downward Lock request changed the stored lock to %q, want %q", got, sqlite3vfs.LockExclusive.String())
	}
}

func TestLockTransitionsInvalid(t *testing.T) {
	v, fcs := newTestVFS(t)
	f := openTestFile(t, v, "badlocks.db") // lockfile exists at LockNone

	if err := f.Lock(sqlite3vfs.LockReserved); err == nil {
		t.Error("Lock(Reserved) from None = nil, want an error")
	}
	if err := f.Lock(sqlite3vfs.LockExclusive); err == nil {
		t.Error("Lock(Exclusive) from None = nil, want an error")
	}
	if err := f.Lock(sqlite3vfs.LockPending); err == nil {
		t.Error("Lock(Pending) = nil, want an error (sqlite never requests pending)")
	}
	if got := lockState(t, fcs, f); got != sqlite3vfs.LockNone.String() {
		t.Errorf("failed lock requests changed the stored lock to %q, want %q", got, sqlite3vfs.LockNone.String())
	}
}

func TestUnlockTransitions(t *testing.T) {
	v, fcs := newTestVFS(t)
	f := openTestFile(t, v, "unlocks.db")

	if err := f.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("Lock(Shared): %v", err)
	}
	// Unlock to the level already held is a no-op.
	if err := f.Unlock(sqlite3vfs.LockShared); err != nil {
		t.Errorf("Unlock(Shared) while Shared = %v, want nil", err)
	}
	if err := f.Unlock(sqlite3vfs.LockNone); err != nil {
		t.Errorf("Unlock(None) from Shared = %v, want nil", err)
	}
	if got := lockState(t, fcs, f); got != sqlite3vfs.LockNone.String() {
		t.Errorf("lock after Unlock(None) = %q, want %q", got, sqlite3vfs.LockNone.String())
	}

	// Reserved drops straight back to Shared, then to None.
	if err := f.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("Lock(Shared): %v", err)
	}
	if err := f.Lock(sqlite3vfs.LockReserved); err != nil {
		t.Fatalf("Lock(Reserved): %v", err)
	}
	if err := f.Unlock(sqlite3vfs.LockShared); err != nil {
		t.Errorf("Unlock(Shared) from Reserved = %v, want nil", err)
	}
	if got := lockState(t, fcs, f); got != sqlite3vfs.LockShared.String() {
		t.Errorf("lock after Unlock(Shared) = %q, want %q", got, sqlite3vfs.LockShared.String())
	}
	if err := f.Unlock(sqlite3vfs.LockNone); err != nil {
		t.Errorf("Unlock(None) = %v, want nil", err)
	}
}

// TestCheckReservedLockSecondProcess simulates a second process by holding a
// second file object over the same (fake) API server: the lock ConfigMap is
// the shared state between them.
func TestCheckReservedLockSecondProcess(t *testing.T) {
	v, _ := newTestVFS(t)
	f1 := openTestFile(t, v, "twoproc.db")
	f2 := NewFile("twoproc.db", v) // "second process", same backing store

	if reserved, err := f2.CheckReservedLock(); err != nil || reserved {
		t.Errorf("CheckReservedLock on unlocked file = (%v, %v), want (false, nil)", reserved, err)
	}

	if err := f1.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("Lock(Shared): %v", err)
	}
	if reserved, err := f2.CheckReservedLock(); err != nil || !reserved {
		t.Errorf("CheckReservedLock while another process holds Shared = (%v, %v), want (true, nil)", reserved, err)
	}

	if err := f1.Unlock(sqlite3vfs.LockNone); err != nil {
		t.Fatalf("Unlock(None): %v", err)
	}
	if reserved, err := f2.CheckReservedLock(); err != nil || reserved {
		t.Errorf("CheckReservedLock after the other process unlocked = (%v, %v), want (false, nil)", reserved, err)
	}

	// The second process observes the released state, so the transition
	// Shared -> Reserved is legal for it now.
	if err := f2.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("second process Lock(Shared): %v", err)
	}
	if err := f2.Lock(sqlite3vfs.LockReserved); err != nil {
		t.Errorf("second process Lock(Reserved) from Shared = %v, want nil", err)
	}
}

// --- Through database/sql ---
//
// TestSQLiteThroughDatabaseSQL keeps the default 4 KiB page size and a small
// row count, so the whole database stays inside one 64 KiB sector.
// TestSQLiteMultiSector64KiBPages then runs the cmd tools' configuration —
// 64 KiB SQLite pages — and inserts enough rows to span several 64 KiB
// sectors, closing and reopening the database, so the multi-sector path is
// exercised end to end through real SQLite (cgo), which no other test does.

func TestSQLiteThroughDatabaseSQL(t *testing.T) {
	v, _ := newTestVFS(t)
	vfsName := registerUniqueVFS(t, v)
	fileName := newSQLFileName()

	db := openSQLDB(t, vfsName, fileName)
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE books (
		id text NOT NULL PRIMARY KEY,
		title text
	)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}

	const rows = 25
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	stmt, err := tx.Prepare("INSERT INTO books (id, title) VALUES (?, ?)")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	for i := 0; i < rows; i++ {
		if _, err := stmt.Exec(fmt.Sprintf("id-%03d", i), fmt.Sprintf("title %d", i)); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	if err := stmt.Close(); err != nil {
		t.Fatalf("stmt.Close: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM books").Scan(&count); err != nil {
		t.Fatalf("COUNT(*): %v", err)
	}
	if count != rows {
		t.Fatalf("COUNT(*) = %d, want %d", count, rows)
	}

	r, err := db.Query("SELECT id, title FROM books ORDER BY id LIMIT 5")
	if err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	for i := 0; r.Next(); i++ {
		var id, title string
		if err := r.Scan(&id, &title); err != nil {
			t.Fatalf("scan row %d: %v", i, err)
		}
		wantID, wantTitle := fmt.Sprintf("id-%03d", i), fmt.Sprintf("title %d", i)
		if id != wantID || title != wantTitle {
			t.Errorf("row %d = (%q, %q), want (%q, %q)", i, id, title, wantID, wantTitle)
		}
	}
	if err := r.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("rows close: %v", err)
	}

	// This small database stays within one 64 KiB sector; the multi-sector
	// path through database/sql is covered by TestSQLiteMultiSector64KiBPages.
	if size, err := NewFile(fileName, v).FileSize(); err != nil {
		t.Fatalf("FileSize of the sqlite file: %v", err)
	} else if size > SectorSize {
		t.Errorf("sqlite database grew to %d bytes, past the one-sector limit", size)
	}

	// The database survives closing and reopening through the same VFS.
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	db2 := openSQLDB(t, vfsName, fileName)
	defer db2.Close()
	var count2 int
	if err := db2.QueryRow("SELECT COUNT(*) FROM books").Scan(&count2); err != nil {
		t.Fatalf("COUNT(*) after reopen: %v", err)
	}
	if count2 != rows {
		t.Errorf("COUNT(*) after reopen = %d, want %d", count2, rows)
	}
}

// TestSQLiteMultiSector64KiBPages runs the cmd tools' SQLite configuration —
// 64 KiB pages, SQLite's largest page size and exactly one VFS sector — and
// inserts enough rows to push the database across several 64 KiB sectors.
// The imported code panicked (through cgo, unrecoverably) the moment a
// database outgrew its first sector, so every database/sql test was capped
// at one sector; this pins the multi-sector write/read path end to end
// through database/sql: create, fill, close, reopen, verify.
func TestSQLiteMultiSector64KiBPages(t *testing.T) {
	v, _ := newTestVFS(t)
	vfsName := registerUniqueVFS(t, v)
	fileName := newSQLFileName()

	db := openSQLDB(t, vfsName, fileName)
	defer db.Close()

	// 64 KiB pages, as main.go and the cmd tools use. page_size is only
	// honoured while the database is still empty, so it must come before
	// the first CREATE TABLE.
	if _, err := db.Exec("PRAGMA page_size=65536"); err != nil {
		t.Fatalf("PRAGMA page_size=65536: %v", err)
	}
	var pageSize int
	if err := db.QueryRow("PRAGMA page_size").Scan(&pageSize); err != nil {
		t.Fatalf("read back PRAGMA page_size: %v", err)
	}
	if pageSize != int(SectorSize) {
		t.Fatalf("PRAGMA page_size read back = %d, want %d; the pragma did not take effect on the connection that builds the database", pageSize, SectorSize)
	}

	if _, err := db.Exec(`CREATE TABLE books (
		id text NOT NULL PRIMARY KEY,
		title text
	)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}

	// ~200 bytes per row: 1000 rows land well past three 64 KiB sectors.
	const rows = 1000
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	stmt, err := tx.Prepare("INSERT INTO books (id, title) VALUES (?, ?)")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	for i := 0; i < rows; i++ {
		if _, err := stmt.Exec(fmt.Sprintf("id-%06d", i), fmt.Sprintf("title %06d %0180d", i, i)); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	if err := stmt.Close(); err != nil {
		t.Fatalf("stmt.Close: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// The database must now span multiple 64 KiB sectors on the cluster.
	size, err := NewFile(fileName, v).FileSize()
	if err != nil {
		t.Fatalf("FileSize: %v", err)
	}
	t.Logf("database size with 64 KiB pages: %d bytes (%d sectors)", size, (size+SectorSize-1)/SectorSize)
	if size <= SectorSize {
		t.Fatalf("database stayed at %d bytes: it never left the first 64 KiB sector, so this test would not exercise the multi-sector path", size)
	}

	// Rows must read back correctly before and after the file is closed:
	// the reopen forces every page through the VFS's multi-sector read path.
	verifyRows := func(db *sql.DB, label string) {
		t.Helper()
		var count int
		if err := db.QueryRow("SELECT COUNT(*) FROM books").Scan(&count); err != nil {
			t.Fatalf("%s: COUNT(*): %v", label, err)
		}
		if count != rows {
			t.Fatalf("%s: COUNT(*) = %d, want %d", label, count, rows)
		}
		for _, i := range []int{0, 1, rows / 2, rows - 2, rows - 1} {
			var id, title string
			wantID := fmt.Sprintf("id-%06d", i)
			wantTitle := fmt.Sprintf("title %06d %0180d", i, i)
			if err := db.QueryRow("SELECT id, title FROM books WHERE id = ?", wantID).Scan(&id, &title); err != nil {
				t.Fatalf("%s: row %d: %v", label, i, err)
			}
			if id != wantID || title != wantTitle {
				t.Errorf("%s: row %d = (%q, %q), want (%q, %q)", label, i, id, title, wantID, wantTitle)
			}
		}
	}
	verifyRows(db, "before close")

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	db2 := openSQLDB(t, vfsName, fileName)
	defer db2.Close()
	verifyRows(db2, "after reopen")
}
