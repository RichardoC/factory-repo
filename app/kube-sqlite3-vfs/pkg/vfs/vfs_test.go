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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/psanford/sqlite3vfs"
	v1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
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
	return NewVFS(tb.Context(), fcs, testNamespace, slog.New(slog.DiscardHandler), 3), fcs
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
	if _, err := fcs.CoreV1().ConfigMaps(testNamespace).Create(tb.Context(), cm, metav1.CreateOptions{}); err != nil {
		if !kerrors.IsAlreadyExists(err) {
			tb.Fatalf("seed sector %d: %v", index, err)
		}
		// Open() pre-creates an empty sector 0; overwrite it.
		if _, err := fcs.CoreV1().ConfigMaps(testNamespace).Update(tb.Context(), cm, metav1.UpdateOptions{}); err != nil {
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
	cms, err := fcs.CoreV1().ConfigMaps(testNamespace).List(tb.Context(), metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(f.SectorLabels).String(),
	})
	if err != nil {
		tb.Fatalf("list sector configmaps: %v", err)
	}
	return cms.Items
}

func getLockfileConfigMap(tb testing.TB, fcs *fake.Clientset, f *file) *v1.ConfigMap {
	tb.Helper()
	cm, err := fcs.CoreV1().ConfigMaps(testNamespace).Get(tb.Context(), f.LockFileName(), metav1.GetOptions{})
	if err != nil {
		tb.Fatalf("get lockfile %q: %v", f.LockFileName(), err)
	}
	return cm
}

func lockState(tb testing.TB, fcs *fake.Clientset, f *file) string {
	tb.Helper()
	return getLockfileConfigMap(tb, fcs, f).Data["lock"]
}

// sharedHoldersIn parses the lockfile ConfigMap's shared-holders JSON:
// holder id -> lease expiry.
func sharedHoldersIn(tb testing.TB, fcs *fake.Clientset, f *file) map[string]time.Time {
	tb.Helper()
	raw := getLockfileConfigMap(tb, fcs, f).Data["shared-holders"]
	if strings.TrimSpace(raw) == "" {
		return map[string]time.Time{}
	}
	holders := map[string]time.Time{}
	if err := json.Unmarshal([]byte(raw), &holders); err != nil {
		tb.Fatalf("parse shared-holders %q: %v", raw, err)
	}
	return holders
}

// writerIn parses the lockfile ConfigMap's writer JSON; nil when empty.
func writerIn(tb testing.TB, fcs *fake.Clientset, f *file) *writerEntry {
	tb.Helper()
	raw := getLockfileConfigMap(tb, fcs, f).Data["writer"]
	if raw == "" {
		return nil
	}
	w := &writerEntry{}
	if err := json.Unmarshal([]byte(raw), w); err != nil {
		tb.Fatalf("parse writer %q: %v", raw, err)
	}
	return w
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
	if err != nil && !errors.Is(err, io.EOF) {
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
	if got := lf.Data["shared-holders"]; got != "{}" {
		t.Errorf("fresh lockfile shared-holders = %q, want %q (an opened file holds nothing)", got, "{}")
	}
	if got := lf.Data["writer"]; got != "" {
		t.Errorf("fresh lockfile writer = %q, want empty (an opened file holds nothing)", got)
	}
	if got := lf.Labels["data"]; got != "lockfile" {
		t.Errorf("fresh lockfile data label = %q, want lockfile", got)
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
	if !errors.Is(err, io.EOF) {
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
		if !errors.Is(err, io.EOF) {
			t.Errorf("ReadAt(off=%d) err = %v, want io.EOF", off, err)
		}
	}

	// Same for a never-written (empty) file.
	empty := openTestFile(t, v, "pastend-empty.db")
	if n, err := empty.ReadAt(got, 0); n != 0 || !errors.Is(err, io.EOF) {
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
	if n, err := f.ReadAt(got, sectorSize); n != 0 || !errors.Is(err, io.EOF) {
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
	if n, err := f.ReadAt(got, 700); n != 0 || !errors.Is(err, io.EOF) {
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
	if !errors.Is(err, io.EOF) {
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

	all, err := fcs.CoreV1().ConfigMaps(testNamespace).List(t.Context(), metav1.ListOptions{})
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
	if _, err := fcs.CoreV1().ConfigMaps(testNamespace).Get(t.Context(), f.LockFileName(), metav1.GetOptions{}); !kerrors.IsNotFound(err) {
		t.Errorf("lockfile get after delete: err = %v, want NotFound", err)
	}
}

// --- Lock transitions ---
//
// The lock levels the sqlite3vfs shim passes through are None=0, Shared=1,
// Reserved=2, Pending=3, Exclusive=4. SQLite never requests Pending
// explicitly (it is implicit in the OS backend), and this VFS rejects it.
//
// Since issue #47 the lock ConfigMap records WHO holds what: a
// shared-holders map (holder id -> lease expiry) plus one writer entry, so
// two files — two processes — can hold SHARED at once, RESERVED excludes
// other writers only, and CheckReservedLock answers "is any writer held",
// not "is any lock held". The aggregate "lock" key keeps its old meaning
// (the highest level held) for readers of the old format.

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
	if holders := sharedHoldersIn(t, fcs, f); len(holders) != 1 {
		t.Errorf("shared-holders after Lock(Shared) = %v, want exactly this file's entry", holders)
	} else if _, ok := holders[f.holderID]; !ok {
		t.Errorf("shared-holders %v lacks this file's holder id", holders)
	}
	if w := writerIn(t, fcs, f); w != nil {
		t.Errorf("writer after Lock(Shared) = %v, want none", w)
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
	// The EXCLUSIVE write subsumes this file's shared entry: the writer is
	// the only record of the lock now.
	if holders := sharedHoldersIn(t, fcs, f); len(holders) != 0 {
		t.Errorf("shared-holders after Lock(Exclusive) = %v, want empty (subsumed by the writer)", holders)
	}
	if w := writerIn(t, fcs, f); w == nil || w.ID != f.holderID || w.Level != sqlite3vfs.LockExclusive.String() {
		t.Errorf("writer after Lock(Exclusive) = %v, want this file at LockExclusive", w)
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
	if w := writerIn(t, fcs, f); w != nil {
		t.Errorf("failed lock requests wrote a writer entry %v, want none", w)
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
	if holders := sharedHoldersIn(t, fcs, f); len(holders) != 0 {
		t.Errorf("shared-holders after Unlock(None) = %v, want empty (this file's entry released)", holders)
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

// TestUnlockRejectsInvalidRequests pins the panic-to-error change: Unlock
// used to log.Panic on an unlock above SHARED or above the current lock
// level; through the sqlite3vfs shim a panic crashes the whole process, so
// both cases must now be returned as errors, leaving the stored lock level
// untouched.
func TestUnlockRejectsInvalidRequests(t *testing.T) {
	v, fcs := newTestVFS(t)
	f := openTestFile(t, v, "badunlock.db")

	if err := f.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("Lock(Shared): %v", err)
	}
	if err := f.Unlock(sqlite3vfs.LockExclusive); err == nil {
		t.Error("Unlock(Exclusive) while holding Shared = nil, want an error (was a panic)")
	}
	if err := f.Unlock(sqlite3vfs.LockReserved); err == nil {
		t.Error("Unlock(Reserved) above the current lock level = nil, want an error (was a panic)")
	}
	if got := lockState(t, fcs, f); got != sqlite3vfs.LockShared.String() {
		t.Errorf("failed unlock requests changed the stored lock to %q, want %q", got, sqlite3vfs.LockShared.String())
	}
}

// TestCheckReservedLockSecondProcess simulates a second process by holding a
// second file object over the same (fake) API server: the lock ConfigMap is
// the shared state between them. Since the holder-aware format
// (issue #47), CheckReservedLock reports whether any WRITER (RESERVED or
// above) is held: shared holders are readers, and another process's plain
// SHARED lock must not read as "someone has reserved the database".
// TestCheckReservedLockSecondProcess pins the Go-level answer of
// CheckReservedLock across two files — WITH THE KNOWN INVERSION the upstream
// dependency forces on it. CheckReservedLock returns the NEGATION of "a
// reserved-or-greater lock is held" (false means held), because upstream
// github.com/psanford/sqlite3vfs writes *pResOut = 0 for a true answer and 1
// for false — see the comment on file.CheckReservedLock and the README's
// "Known upstream bug" section. The value SQLite finally sees is pinned end
// to end, across the cgo boundary, by
// TestCheckReservedLockCrossesCGOBoundary in vfs_concurrent_test.go.
func TestCheckReservedLockSecondProcess(t *testing.T) {
	v, _ := newTestVFS(t)
	f1 := openTestFile(t, v, "twoproc.db")
	f2 := NewFile("twoproc.db", v) // "second process", same backing store

	// Nobody holds anything: the true answer is "not held", inverted to true.
	if reserved, err := f2.CheckReservedLock(); err != nil || !reserved {
		t.Errorf("CheckReservedLock on unlocked file = (%v, %v), want (true, nil)", reserved, err)
	}

	if err := f1.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("Lock(Shared): %v", err)
	}
	// A SHARED holder is a reader, not a reserved-lock holder.
	if reserved, err := f2.CheckReservedLock(); err != nil || !reserved {
		t.Errorf("CheckReservedLock while another process holds Shared = (%v, %v), want (true, nil)", reserved, err)
	}

	if err := f1.Lock(sqlite3vfs.LockReserved); err != nil {
		t.Fatalf("Lock(Reserved): %v", err)
	}
	// A reserved lock IS held: the inverted answer is false, which upstream
	// turns into *pResOut = 1 — "locked" — for SQLite.
	if reserved, err := f2.CheckReservedLock(); err != nil || reserved {
		t.Errorf("CheckReservedLock while another process holds Reserved = (%v, %v), want (false, nil)", reserved, err)
	}

	// Downgrading the writer back to SHARED clears the writer entry, and
	// the second process observes that too.
	if err := f1.Unlock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("Unlock(Shared): %v", err)
	}
	if reserved, err := f2.CheckReservedLock(); err != nil || !reserved {
		t.Errorf("CheckReservedLock after the writer downgraded = (%v, %v), want (true, nil)", reserved, err)
	}

	if err := f1.Unlock(sqlite3vfs.LockNone); err != nil {
		t.Fatalf("Unlock(None): %v", err)
	}
	if reserved, err := f2.CheckReservedLock(); err != nil || !reserved {
		t.Errorf("CheckReservedLock after the other process unlocked = (%v, %v), want (true, nil)", reserved, err)
	}

	// The second process observes the released state, so the transitions
	// None -> Shared -> Reserved are legal for it now.
	if err := f2.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("second process Lock(Shared): %v", err)
	}
	if err := f2.Lock(sqlite3vfs.LockReserved); err != nil {
		t.Errorf("second process Lock(Reserved) from Shared = %v, want nil", err)
	}
}

// TestFailedUnlockInvalidatesCaches pins the failed-unlock fix: a transient
// failure of the lock ConfigMap write on Unlock(NONE) must leave no valid
// cache and no trusted lock level, because sqlite largely ignores unlock
// errors and keeps going. The bug this replaces updated the lock state and
// the caches only on write success, so after one failed unlock the next
// Lock(SHARED) made no API call and served cached sectors and size while
// another process had rewritten the database — wrong reads, and lost writes
// once a partial-sector write built on the cached data.
func TestFailedUnlockInvalidatesCaches(t *testing.T) {
	v, fcs := newTestVFS(t)
	fa := openTestFile(t, v, "stale.db")

	old := make([]byte, 1000)
	fillPattern(old)
	mustWriteAt(t, fa, old, 0)
	if err := fa.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("A Lock(Shared): %v", err)
	}
	got := make([]byte, 1000)
	if n := mustReadAt(t, fa, got, 0); n != 1000 {
		t.Fatalf("A warm-up read got %d bytes, want 1000", n)
	}
	if !fa.valid || len(fa.sectors) == 0 {
		t.Fatal("setup: A caches are not warm after a read under SHARED")
	}

	// A's next unlock fails once, at the API layer, on the lock ConfigMap
	// only (sector writes must still go through).
	var failed int32
	boom := errors.New("boom: transient API error")
	fcs.PrependReactor("update", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		ua, isUpdate := action.(k8stesting.UpdateAction)
		if !isUpdate {
			return false, nil, nil
		}
		cm, ok := ua.GetObject().(*v1.ConfigMap)
		if !ok || cm.Name != fa.LockFileName() {
			return false, nil, nil
		}
		if atomic.AddInt32(&failed, 1) > 1 {
			return false, nil, nil
		}
		return true, nil, boom
	})
	if err := fa.Unlock(sqlite3vfs.LockNone); err == nil {
		t.Fatal("Unlock(None) with a failing ConfigMap write = nil, want the error")
	}
	if fa.valid || fa.sectors != nil || fa.lockKnown {
		t.Errorf("failed unlock left state known/caches valid: valid=%v, sectors=%v, lockKnown=%v; caches must be dropped and the lock state unknown whatever the write returns", fa.valid, fa.sectors, fa.lockKnown)
	}

	// B rewrites the database while A no longer trusts anything of its own:
	// new bytes, bigger file.
	fb := openTestFile(t, v, "stale.db")
	latest := make([]byte, 2000)
	fillPatternAlt(latest)
	mustWriteAt(t, fb, latest, 0)

	// A re-locks and reads: it must see B's data and size, and re-locking
	// must cost API calls — the bug served all of it from the stale cache
	// with zero calls.
	fcs.ClearActions()
	if err := fa.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("A re-Lock(Shared): %v", err)
	}
	size, err := fa.FileSize()
	if err != nil {
		t.Fatalf("A FileSize after re-lock: %v", err)
	}
	if size != int64(len(latest)) {
		t.Errorf("A FileSize after re-lock = %d, want %d (B size)", size, len(latest))
	}
	got = make([]byte, len(latest))
	if n := mustReadAt(t, fa, got, 0); n != len(latest) {
		t.Fatalf("A read %d bytes after re-lock, want %d", n, len(latest))
	}
	if !bytes.Equal(got, latest) {
		t.Error("A served stale bytes after a failed unlock: the cache survived an unlock whose write failed")
	}
	if calls := len(fcs.Actions()); calls == 0 {
		t.Error("A re-lock and read made 0 API calls: the failed unlock left the cache valid")
	}
}

// TestOpenDoesNotAdoptAnotherProcessLock pins the no-adoption fix: the lock
// level stored in the ConfigMap may be another process's, so a file opened
// through the VFS must start at NONE locally and take every one of its own
// transitions through the ConfigMap. The bug this replaces copied the
// stored level into the new file object's lockLevel, so a newly opened A
// believed it held B's EXCLUSIVE: its CheckReservedLock answered from that
// belief, its Lock(SHARED) made no call, its Unlock(NONE) wrote NONE while
// B still held EXCLUSIVE, and its later Lock(RESERVED)/Lock(EXCLUSIVE) were
// silent no-ops that let it write while the cluster showed NONE.
func TestOpenDoesNotAdoptAnotherProcessLock(t *testing.T) {
	v, fcs := newTestVFS(t)
	fb := openTestFile(t, v, "held.db")
	if err := fb.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("B Lock(Shared): %v", err)
	}
	if err := fb.Lock(sqlite3vfs.LockExclusive); err != nil {
		t.Fatalf("B Lock(Exclusive): %v", err)
	}
	if got := lockState(t, fcs, fb); got != sqlite3vfs.LockExclusive.String() {
		t.Fatalf("setup: stored lock = %q, want exclusive", got)
	}

	// A opens while B holds EXCLUSIVE.
	fa := openTestFile(t, v, "held.db")
	if !fa.lockKnown || fa.lockLevel != sqlite3vfs.LockNone {
		t.Errorf("opened file lock state = (known=%v, level=%v), want (true, NONE): the stored level must never be adopted", fa.lockKnown, fa.lockLevel)
	}

	// A must still learn, from the cluster, that a lock is held.
	if reserved, err := fa.CheckReservedLock(); err != nil || !reserved {
		t.Errorf("A CheckReservedLock while B holds Exclusive = (%v, %v), want (true, nil)", reserved, err)
	}

	// A holds nothing, so unlocking to NONE is a no-op that must not write:
	// the stored level stays B's EXCLUSIVE.
	fcs.ClearActions()
	if err := fa.Unlock(sqlite3vfs.LockNone); err != nil {
		t.Fatalf("A Unlock(None): %v", err)
	}
	if calls := len(fcs.Actions()); calls != 0 {
		t.Errorf("A no-op Unlock(None) made %d API calls, want 0", calls)
	}
	if got := lockState(t, fcs, fa); got != sqlite3vfs.LockExclusive.String() {
		t.Errorf("A Unlock(None) changed the stored lock to %q; B still holds Exclusive", got)
	}

	// From the un-adopted NONE, climbing straight to RESERVED or EXCLUSIVE
	// is an error, not a silent no-op: A cannot believe it already holds a
	// higher lock than it ever asked for.
	if err := fa.Lock(sqlite3vfs.LockReserved); err == nil {
		t.Error("A Lock(Reserved) from the un-adopted None = nil, want an error")
	}
	if err := fa.Lock(sqlite3vfs.LockExclusive); err == nil {
		t.Error("A Lock(Exclusive) from the un-adopted None = nil, want an error")
	}
	if got := lockState(t, fcs, fa); got != sqlite3vfs.LockExclusive.String() {
		t.Errorf("rejected lock requests changed the stored lock to %q, want exclusive", got)
	}
}

// TestTwoProcessesMutualExclusion is the core issue #47 test: two vfs
// instances — two processes — over ONE fake clientset, each file with its
// own holder id. Both may hold SHARED; only one may hold RESERVED; the
// other's attempt reports sqlite3vfs.BusyError; EXCLUSIVE waits (BUSY)
// while any other holder remains and succeeds once it releases.
func TestTwoProcessesMutualExclusion(t *testing.T) {
	fcs := fake.NewSimpleClientset()
	va := NewVFS(t.Context(), fcs, testNamespace, slog.New(slog.DiscardHandler), 3)
	vb := NewVFS(t.Context(), fcs, testNamespace, slog.New(slog.DiscardHandler), 3)

	fa := openTestFile(t, va, "mutual.db")
	fb := openTestFile(t, vb, "mutual.db")
	if fa.holderID == "" || fa.holderID == fb.holderID {
		t.Fatalf("holder ids must be distinct and non-empty, got %q twice", fa.holderID)
	}

	// Both take SHARED: any number of shared holders may coexist.
	if err := fa.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("A Lock(Shared): %v", err)
	}
	if err := fb.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("B Lock(Shared) while A holds Shared: %v, want nil (readers coexist)", err)
	}
	holders := sharedHoldersIn(t, fcs, fa)
	if len(holders) != 2 {
		t.Fatalf("shared-holders = %v, want entries for both holders", holders)
	}
	for _, f := range []*file{fa, fb} {
		if _, ok := holders[f.holderID]; !ok {
			t.Errorf("shared-holders %v lacks %s's entry", holders, f.holderID)
		}
	}
	if got := lockState(t, fcs, fa); got != sqlite3vfs.LockShared.String() {
		t.Errorf("aggregate lock with two shared holders = %q, want %q", got, sqlite3vfs.LockShared.String())
	}

	// Only one writer: A takes RESERVED, B is refused with BusyError.
	if err := fa.Lock(sqlite3vfs.LockReserved); err != nil {
		t.Fatalf("A Lock(Reserved): %v", err)
	}
	if err := fb.Lock(sqlite3vfs.LockReserved); !errors.Is(err, sqlite3vfs.BusyError) {
		t.Fatalf("B Lock(Reserved) while A holds it = %v, want sqlite3vfs.BusyError", err)
	}
	if w := writerIn(t, fcs, fa); w == nil || w.ID != fa.holderID || w.Level != sqlite3vfs.LockReserved.String() {
		t.Fatalf("writer after A takes Reserved = %v, want A at LockReserved", w)
	}
	// B keeps its SHARED through the refused attempt, and the aggregate
	// still names the highest held level.
	if holders := sharedHoldersIn(t, fcs, fb); len(holders) != 2 {
		t.Errorf("shared-holders after B's refused Reserved = %v, want both entries intact", holders)
	}
	if got := lockState(t, fcs, fb); got != sqlite3vfs.LockReserved.String() {
		t.Errorf("aggregate with A Reserved + both Shared = %q, want %q", got, sqlite3vfs.LockReserved.String())
	}

	// A downgrades to SHARED; B can take RESERVED now.
	if err := fa.Unlock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("A Unlock(Shared): %v", err)
	}
	if w := writerIn(t, fcs, fa); w != nil {
		t.Errorf("writer after A downgraded = %v, want none", w)
	}
	if err := fb.Lock(sqlite3vfs.LockReserved); err != nil {
		t.Fatalf("B Lock(Reserved) after A downgraded: %v, want nil", err)
	}

	// EXCLUSIVE waits (BusyError) while another holder still holds SHARED.
	if err := fb.Lock(sqlite3vfs.LockExclusive); !errors.Is(err, sqlite3vfs.BusyError) {
		t.Fatalf("B Lock(Exclusive) while A holds Shared = %v, want sqlite3vfs.BusyError", err)
	}
	// The refused attempt leaves B holding PENDING, which blocks new
	// readers from now on.
	if w := writerIn(t, fcs, fb); w == nil || w.ID != fb.holderID || w.Level != sqlite3vfs.LockPending.String() {
		t.Fatalf("writer after B's refused Exclusive = %v, want B at LockPending", w)
	}
	if got := lockState(t, fcs, fb); got != sqlite3vfs.LockPending.String() {
		t.Errorf("aggregate with B Pending = %q, want %q", got, sqlite3vfs.LockPending.String())
	}
	fc := openTestFile(t, va, "mutual.db")
	if err := fc.Lock(sqlite3vfs.LockShared); !errors.Is(err, sqlite3vfs.BusyError) {
		t.Fatalf("C Lock(Shared) while B holds Pending = %v, want sqlite3vfs.BusyError (pending blocks readers)", err)
	}

	// A releases; B's EXCLUSIVE now succeeds and subsumes B's own entry.
	if err := fa.Unlock(sqlite3vfs.LockNone); err != nil {
		t.Fatalf("A Unlock(None): %v", err)
	}
	if err := fb.Lock(sqlite3vfs.LockExclusive); err != nil {
		t.Fatalf("B Lock(Exclusive) after A released: %v, want nil", err)
	}
	if holders := sharedHoldersIn(t, fcs, fb); len(holders) != 0 {
		t.Errorf("shared-holders after B Exclusive = %v, want empty (B's own entry subsumed)", holders)
	}
	if w := writerIn(t, fcs, fb); w == nil || w.Level != sqlite3vfs.LockExclusive.String() {
		t.Errorf("writer after B Exclusive = %v, want LockExclusive", w)
	}
	if got := lockState(t, fcs, fb); got != sqlite3vfs.LockExclusive.String() {
		t.Errorf("aggregate after B Exclusive = %q, want %q", got, sqlite3vfs.LockExclusive.String())
	}
}

// TestCASRetriesOnConflict pins the compare-and-swap: one injected 409 on
// the lock ConfigMap Update is retried — the CAS re-reads the state
// (including the other holder's entry) and re-decides, converging without a
// lost update.
//
// The 409 has to be injected by a hand-written reactor because the bare fake
// clientset cannot arbitrate it natively: its tracker (k8s.io/client-go/
// testing, fixture.go tracker.add) replaces the stored object on every
// Update without ever comparing the submitted resourceVersion against the
// stored one, and it keeps resourceVersions per GVR rather than per object,
// so no stale write is ever refused. newArbitratingFakeClientset in
// vfs_concurrent_test.go restores the real per-object
// resourceVersion/409/AlreadyExists semantics with reactors for the
// end-to-end two-instance tests; there is no configuration of the plain fake
// that produces a Conflict on its own.
func TestCASRetriesOnConflict(t *testing.T) {
	v, fcs := newTestVFS(t)
	fa := openTestFile(t, v, "conflict.db")
	fb := openTestFile(t, v, "conflict.db")

	if err := fb.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("B Lock(Shared): %v", err)
	}
	if err := fa.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("A Lock(Shared): %v", err)
	}

	// The next lock ConfigMap Update fails once with a real 409.
	var attempts, injected int32
	fcs.PrependReactor("update", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		ua, ok := action.(k8stesting.UpdateAction)
		if !ok {
			return false, nil, nil
		}
		cm, ok := ua.GetObject().(*v1.ConfigMap)
		if !ok || cm.Name != fa.LockFileName() {
			return false, nil, nil
		}
		if atomic.AddInt32(&attempts, 1) > 1 {
			return false, nil, nil
		}
		atomic.AddInt32(&injected, 1)
		return true, nil, kerrors.NewConflict(v1.Resource("configmaps"), fa.LockFileName(), errors.New("injected 409"))
	})
	// Count only the updates of the transition under test.
	fcs.ClearActions()

	if err := fa.Lock(sqlite3vfs.LockReserved); err != nil {
		t.Fatalf("A Lock(Reserved) under an injected 409 = %v, want nil (the CAS retries)", err)
	}
	if injected != 1 {
		t.Errorf("injected conflict count = %d, want exactly 1", injected)
	}
	if attempts != 2 {
		t.Errorf("lock update attempts seen by the reactor = %d, want 2 (one 409 + one retry)", attempts)
	}

	// The converged state carries BOTH B's shared entry (not lost by the
	// retry) and A's writer.
	holders := sharedHoldersIn(t, fcs, fa)
	if _, ok := holders[fb.holderID]; !ok {
		t.Errorf("shared-holders %v lost B's entry: the retry overwrote instead of re-deciding", holders)
	}
	if _, ok := holders[fa.holderID]; !ok {
		t.Errorf("shared-holders %v lost A's entry", holders)
	}
	if w := writerIn(t, fcs, fa); w == nil || w.ID != fa.holderID || w.Level != sqlite3vfs.LockReserved.String() {
		t.Errorf("writer after retried Reserved = %v, want A at LockReserved", w)
	}

	// Exactly two Update attempts were made against the lock ConfigMap:
	// the conflicted one and the successful retry.
	updates := 0
	for _, a := range fcs.Actions() {
		if a.GetVerb() == "update" && a.GetResource().Resource == "configmaps" {
			updates++
		}
	}
	if updates != 2 {
		t.Errorf("lock ConfigMap updates = %d, want 2 (one 409 + one retry)", updates)
	}
}

// TestStaleHolderPruned pins lease expiry: a holder that stops renewing —
// a crashed process — is pruned by the next CAS of another process, so its
// shared entry and writer stop blocking RESERVED and EXCLUSIVE even though
// it never unlocked.
func TestStaleHolderPruned(t *testing.T) {
	fcs := fake.NewSimpleClientset()
	lease := 80 * time.Millisecond
	va := NewVFS(t.Context(), fcs, testNamespace, slog.New(slog.DiscardHandler), 3, WithLockLease(lease))
	vb := NewVFS(t.Context(), fcs, testNamespace, slog.New(slog.DiscardHandler), 3) // default 30s lease

	fa := openTestFile(t, va, "stale-holder.db")
	if err := fa.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("A Lock(Shared): %v", err)
	}
	if err := fa.Lock(sqlite3vfs.LockReserved); err != nil {
		t.Fatalf("A Lock(Reserved): %v", err)
	}

	// B (with its long default lease) joins as a reader; A's writer blocks
	// B's RESERVED.
	fb := openTestFile(t, vb, "stale-holder.db")
	if err := fb.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("B Lock(Shared): %v", err)
	}
	if err := fb.Lock(sqlite3vfs.LockReserved); !errors.Is(err, sqlite3vfs.BusyError) {
		t.Fatalf("B Lock(Reserved) while A (live) holds it = %v, want sqlite3vfs.BusyError", err)
	}

	// A dies: no more renewals. Sleep past its lease.
	time.Sleep(lease + 80*time.Millisecond)

	// B's next CAS prunes A's expired entries: the RESERVED is free again
	// even though A never unlocked, and B can climb to EXCLUSIVE.
	if err := fb.Lock(sqlite3vfs.LockReserved); err != nil {
		t.Fatalf("B Lock(Reserved) after A's lease expired: %v, want nil (stale holder pruned)", err)
	}
	if err := fb.Lock(sqlite3vfs.LockExclusive); err != nil {
		t.Fatalf("B Lock(Exclusive) after A's lease expired: %v, want nil", err)
	}
	holders := sharedHoldersIn(t, fcs, fb)
	if _, ok := holders[fa.holderID]; ok {
		t.Errorf("A's expired entry survived B's CAS: %v", holders)
	}
	if w := writerIn(t, fcs, fb); w == nil || w.ID != fb.holderID || w.Level != sqlite3vfs.LockExclusive.String() {
		t.Errorf("writer after B Exclusive = %v, want B at LockExclusive (A's stale writer pruned)", w)
	}
	if got := lockState(t, fcs, fb); got != sqlite3vfs.LockExclusive.String() {
		t.Errorf("aggregate after B Exclusive = %q, want %q", got, sqlite3vfs.LockExclusive.String())
	}
}

// TestLeaseRenewedOnLockPathOps pins renewal: a no-op Lock on the level
// already held renews a lease that is under half spent, and the renewed
// entry is still live — unpruned by another process's CAS — after the
// original expiry has passed. The renewal never touches the sector read
// path, which is separately pinned by TestSectorReadPathMakesNoLockCalls.
func TestLeaseRenewedOnLockPathOps(t *testing.T) {
	fcs := fake.NewSimpleClientset()
	lease := 120 * time.Millisecond
	va := NewVFS(t.Context(), fcs, testNamespace, slog.New(slog.DiscardHandler), 3, WithLockLease(lease))
	vb := NewVFS(t.Context(), fcs, testNamespace, slog.New(slog.DiscardHandler), 3)

	fa := openTestFile(t, va, "renewal.db")
	if err := fa.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("A Lock(Shared): %v", err)
	}
	first := sharedHoldersIn(t, fcs, fa)[fa.holderID]

	// Past half the lease, still before expiry: the no-op Lock renews.
	time.Sleep(lease/2 + 20*time.Millisecond)
	if err := fa.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("A no-op Lock(Shared) (should renew): %v", err)
	}
	second := sharedHoldersIn(t, fcs, fa)[fa.holderID]
	if !second.After(first) {
		t.Fatalf("renewal did not move the lease forward: %v -> %v", first, second)
	}

	// The original expiry is now past. Another process's CAS must find A's
	// renewed entry live and leave it alone.
	time.Sleep(lease/2 + 20*time.Millisecond)
	fb := openTestFile(t, vb, "renewal.db")
	if err := fb.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("B Lock(Shared): %v", err)
	}
	if err := fb.Lock(sqlite3vfs.LockReserved); err != nil {
		t.Fatalf("B Lock(Reserved): %v", err)
	}
	holders := sharedHoldersIn(t, fcs, fa)
	if _, ok := holders[fa.holderID]; !ok {
		t.Error("A's renewed entry was pruned although it never lapsed")
	}
}

// TestLeaseLossDetected pins the losing side of a lapsed lease: a file that
// made no lock-path call past its own lease discovers, on its next no-op
// lock operation, that its entries are gone — the local belief is reset and
// the loss is reported instead of writing under a false belief.
func TestLeaseLossDetected(t *testing.T) {
	fcs := fake.NewSimpleClientset()
	lease := 40 * time.Millisecond
	v := NewVFS(t.Context(), fcs, testNamespace, slog.New(slog.DiscardHandler), 3, WithLockLease(lease))

	fa := openTestFile(t, v, "lost.db")
	if err := fa.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("A Lock(Shared): %v", err)
	}
	time.Sleep(2 * lease) // the lease lapses with no lock-path call

	err := fa.Lock(sqlite3vfs.LockShared)
	if !errors.Is(err, errLockLeaseLost) {
		t.Fatalf("Lock after lapsed lease = %v, want errLockLeaseLost", err)
	}
	if fa.lockLevel != sqlite3vfs.LockNone || !fa.lockKnown {
		t.Errorf("after lease loss local state = (level %v, known %v), want (NONE, true)", fa.lockLevel, fa.lockKnown)
	}

	// The file re-acquires cleanly, as a fresh holder.
	if err := fa.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("re-Lock(Shared) after the loss: %v", err)
	}
	if holders := sharedHoldersIn(t, fcs, fa); len(holders) != 1 {
		t.Errorf("shared-holders after re-acquire = %v, want exactly one entry", holders)
	}
}

// fakeClock is the injectable clock the lease tests drive: the test advances
// it instead of sleeping, and the VFS's whole lock path stamps, prunes and
// renews against it through vfs.clock. It is called from the test goroutine
// only.
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

// TestLeaseRenewedFromIOPathWhenNearExpiry pins lease renewal from the
// sector read/write path: a read or write stretch longer than half the lease
// must not outrun renewal, or the lease lapses while WriteAt keeps writing
// (the review finding on the #47 locking). The injected clock moves time
// without sleeps; the API-call counts pin that a live lease still costs the
// read/write path nothing at all and a due renewal exactly one CAS.
func TestLeaseRenewedFromIOPathWhenNearExpiry(t *testing.T) {
	fcs := fake.NewSimpleClientset()
	const lease = time.Minute
	clock := &fakeClock{now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	v := NewVFS(t.Context(), fcs, testNamespace, slog.New(slog.DiscardHandler), 3, WithLockLease(lease))
	v.clock = clock.Now

	f := openTestFile(t, v, "io-renewal.db")
	if err := f.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("Lock(Shared): %v", err)
	}
	first := sharedHoldersIn(t, fcs, f)[f.holderID]
	buf := make([]byte, 64)
	fillPattern(buf)

	// A write well inside the lease renews nothing: exactly one API call
	// (the sector write), and the stored lease does not move.
	clock.Advance(lease / 4) // 15s in: 45s of the 60s lease remain
	before := len(fcs.Actions())
	if n, err := f.WriteAt(buf, 0); err != nil || n != len(buf) {
		t.Fatalf("WriteAt at quarter lease = (%d, %v), want (%d, nil)", n, err, len(buf))
	}
	if got := len(fcs.Actions()) - before; got != 1 {
		t.Errorf("write at quarter lease caused %d API calls, want exactly 1 (the sector write; no lock calls)", got)
	}
	if got := sharedHoldersIn(t, fcs, f)[f.holderID]; !got.Equal(first) {
		t.Errorf("stored lease moved at quarter lease: %v -> %v, want unchanged", first, got)
	}

	// Past half the lease the same write renews first: the renewal CAS is
	// one Get plus one Update on top of the sector write, and the stored
	// expiry is now+lease as of the renewal.
	clock.Advance(lease / 3) // 35s in: 25s remain, under half
	before = len(fcs.Actions())
	if n, err := f.WriteAt(buf, 128); err != nil || n != len(buf) {
		t.Fatalf("WriteAt past half lease = (%d, %v), want (%d, nil)", n, err, len(buf))
	}
	second := sharedHoldersIn(t, fcs, f)[f.holderID]
	if want := first.Add(35 * time.Second); !second.Equal(want) {
		t.Errorf("stored lease after write past half = %v, want %v (renewed to now+lease at the renewal)", second, want)
	}
	if got := len(fcs.Actions()) - before; got != 3 {
		t.Errorf("write past half the lease caused %d API calls, want 3 (renewal Get + Update, then the sector write)", got)
	}

	// The read path renews too, on the same half-spent rule.
	clock.Advance(lease/2 + time.Second) // 66s after the renewal: 29s remain
	if _, err := f.ReadAt(make([]byte, 8), 0); err != nil {
		t.Fatalf("ReadAt past half lease: %v", err)
	}
	third := sharedHoldersIn(t, fcs, f)[f.holderID]
	if want := second.Add(31 * time.Second); !third.Equal(want) {
		t.Errorf("stored lease after read past half = %v, want %v", third, want)
	}
}

// TestLeaseLossFailsSectorIOUntilRelock pins the losing side of the gate:
// once the lease has lapsed, WriteAt is refused instead of writing without
// the lock, the loss sticks so ReadAt is refused too (instead of serving
// sectors cached under the lost lock), and a successful Lock clears it —
// after which the file reads exactly the bytes committed before the loss,
// proving the refused write never landed.
func TestLeaseLossFailsSectorIOUntilRelock(t *testing.T) {
	fcs := fake.NewSimpleClientset()
	const lease = time.Minute
	clock := &fakeClock{now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	v := NewVFS(t.Context(), fcs, testNamespace, slog.New(slog.DiscardHandler), 3, WithLockLease(lease))
	v.clock = clock.Now

	f := openTestFile(t, v, "io-loss.db")
	if err := f.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("Lock(Shared): %v", err)
	}
	kept := make([]byte, 64)
	fillPattern(kept)
	if n, err := f.WriteAt(kept, 0); err != nil || n != len(kept) {
		t.Fatalf("WriteAt while live = (%d, %v), want (%d, nil)", n, err, len(kept))
	}

	clock.Advance(2 * lease) // the lease lapses with no renewal
	if _, err := f.WriteAt(make([]byte, 64), 0); !errors.Is(err, sqlite3vfs.BusyError) {
		t.Fatalf("WriteAt with a lapsed lease = %v, want sqlite3vfs.BusyError", err)
	}
	if f.lockLevel != sqlite3vfs.LockNone || !f.lockKnown || !f.leaseLost {
		t.Errorf("after lease loss local state = (level %v, known %v, lost %v), want (NONE, true, true)", f.lockLevel, f.lockKnown, f.leaseLost)
	}

	if _, err := f.ReadAt(make([]byte, 64), 0); !errors.Is(err, sqlite3vfs.BusyError) {
		t.Fatalf("ReadAt with a lost lease = %v, want sqlite3vfs.BusyError (sticky)", err)
	}

	if err := f.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("re-Lock(Shared) after the loss: %v", err)
	}
	if f.leaseLost {
		t.Error("leaseLost survived a successful Lock; the read/write path would stay gated forever")
	}
	got := make([]byte, len(kept))
	n, err := f.ReadAt(got, 0)
	if err != nil && err != io.EOF {
		t.Fatalf("ReadAt after re-lock: %v", err)
	}
	if n != len(kept) || !bytes.Equal(got, kept) {
		t.Errorf("bytes after re-lock = (%d, %v), want the pre-loss %d bytes (the refused write must not have landed)", n, got, len(kept))
	}
}

// ctxSensitiveProbe wraps a kubernetes.Interface and fails every ConfigMap
// Get/Update/Create whose CALL context is already done, the way a real
// clientset honours per-call context cancellation. The bare fake ignores the
// context entirely — its methods never look at ctx — so without this probe a
// test cannot tell which context a lock-path call was made with, and a
// cancelled-context bug would pass unnoticed.
type ctxSensitiveProbe struct {
	kubernetes.Interface
}

func (p *ctxSensitiveProbe) CoreV1() corev1client.CoreV1Interface {
	return &ctxSensitiveCoreV1{CoreV1Interface: p.Interface.CoreV1()}
}

type ctxSensitiveCoreV1 struct {
	corev1client.CoreV1Interface
}

func (c *ctxSensitiveCoreV1) ConfigMaps(namespace string) corev1client.ConfigMapInterface {
	return &ctxSensitiveConfigMaps{ConfigMapInterface: c.CoreV1Interface.ConfigMaps(namespace)}
}

type ctxSensitiveConfigMaps struct {
	corev1client.ConfigMapInterface
}

func requireLiveCtx(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("call made with a done context: %w", err)
	}
	return nil
}

func (m *ctxSensitiveConfigMaps) Get(ctx context.Context, name string, opts metav1.GetOptions) (*v1.ConfigMap, error) {
	if err := requireLiveCtx(ctx); err != nil {
		return nil, err
	}
	return m.ConfigMapInterface.Get(ctx, name, opts)
}

func (m *ctxSensitiveConfigMaps) Update(ctx context.Context, cm *v1.ConfigMap, opts metav1.UpdateOptions) (*v1.ConfigMap, error) {
	if err := requireLiveCtx(ctx); err != nil {
		return nil, err
	}
	return m.ConfigMapInterface.Update(ctx, cm, opts)
}

func (m *ctxSensitiveConfigMaps) Create(ctx context.Context, cm *v1.ConfigMap, opts metav1.CreateOptions) (*v1.ConfigMap, error) {
	if err := requireLiveCtx(ctx); err != nil {
		return nil, err
	}
	return m.ConfigMapInterface.Create(ctx, cm, opts)
}

// TestLockPathSurvivesCancelledContext pins the context finding on the #47
// locking: the construction context belongs to the caller's registration
// scope, and cancelling it after Open must not break a single lock-path
// call. Every lock ConfigMap compare-and-swap — and the CheckReservedLock
// read — runs on a cancellation-detached context (context.WithoutCancel)
// with its own timeout, so the whole locking protocol still works here even
// though every call made with the registration context itself would fail.
// The probe above makes the fake reject done contexts like a real clientset,
// and the negative control at the end proves it would have caught a leaked
// cancelled context.
func TestLockPathSurvivesCancelledContext(t *testing.T) {
	fcs := fake.NewSimpleClientset()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	probe := &ctxSensitiveProbe{Interface: fcs}
	v := NewVFS(ctx, probe, testNamespace, slog.New(slog.DiscardHandler), 3)

	fa := openTestFile(t, v, "cancel-ctx.db") // opened while the ctx is live
	fb := openTestFile(t, v, "cancel-ctx.db")

	cancel() // the registration context dies; the lock path must not care

	if err := fa.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("Lock(Shared) after the registration context was cancelled: %v", err)
	}
	if err := fb.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("second holder Lock(Shared) after cancellation: %v", err)
	}
	if reserved, err := fa.CheckReservedLock(); err != nil || !reserved {
		t.Fatalf("CheckReservedLock after cancellation = (%v, %v), want (true, nil)", reserved, err)
	}
	if err := fa.Lock(sqlite3vfs.LockReserved); err != nil {
		t.Fatalf("Lock(Reserved) after cancellation: %v", err)
	}
	if err := fa.Unlock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("Unlock(Shared) after cancellation: %v", err)
	}
	if err := fa.Unlock(sqlite3vfs.LockNone); err != nil {
		t.Fatalf("Unlock(None) after cancellation: %v", err)
	}
	if err := fb.Unlock(sqlite3vfs.LockNone); err != nil {
		t.Fatalf("second holder Unlock(None) after cancellation: %v", err)
	}
	if holders := sharedHoldersIn(t, fcs, fa); len(holders) != 0 {
		t.Errorf("shared-holders after all unlocks = %v, want empty (every CAS landed)", holders)
	}

	// Negative control: the probe really does reject a call made with the
	// cancelled context, so the passes above mean the lock path avoided it.
	if _, err := probe.CoreV1().ConfigMaps(testNamespace).Get(ctx, fa.LockFileName(), metav1.GetOptions{}); err == nil {
		t.Fatal("probe accepted a call made with the cancelled context; this test proves nothing")
	}
}

// TestCloseReleasesHolder pins Close (= Unlock(NONE)): the closing file's
// shared entry and writer are removed and every other holder's entries
// survive.
func TestCloseReleasesHolder(t *testing.T) {
	v, fcs := newTestVFS(t)
	fa := openTestFile(t, v, "closing.db")
	fb := openTestFile(t, v, "closing.db")

	if err := fa.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("A Lock(Shared): %v", err)
	}
	if err := fb.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("B Lock(Shared): %v", err)
	}
	if err := fa.Lock(sqlite3vfs.LockReserved); err != nil {
		t.Fatalf("A Lock(Reserved): %v", err)
	}

	if err := fa.Close(); err != nil {
		t.Fatalf("A Close: %v", err)
	}
	holders := sharedHoldersIn(t, fcs, fa)
	if len(holders) != 1 {
		t.Fatalf("shared-holders after A Close = %v, want only B's entry", holders)
	}
	if _, ok := holders[fb.holderID]; !ok {
		t.Errorf("B's entry did not survive A's Close: %v", holders)
	}
	if w := writerIn(t, fcs, fa); w != nil {
		t.Errorf("writer after A Close = %v, want none", w)
	}
	if got := lockState(t, fcs, fa); got != sqlite3vfs.LockShared.String() {
		t.Errorf("aggregate after A Close = %q, want %q (B still holds Shared)", got, sqlite3vfs.LockShared.String())
	}

	// Closing an already-closed file is a no-op.
	if err := fa.Close(); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
	if got := lockState(t, fcs, fa); got != sqlite3vfs.LockShared.String() {
		t.Errorf("second Close changed the aggregate to %q, want %q", got, sqlite3vfs.LockShared.String())
	}
}

// TestAggregateLockKeyStaysMaxHeldLevel walks mixed states over two holders
// and asserts the aggregate "lock" key — the only key readers of the old
// format see — always names the highest level actually held.
func TestAggregateLockKeyStaysMaxHeldLevel(t *testing.T) {
	v, fcs := newTestVFS(t)
	fa := openTestFile(t, v, "aggregate.db")
	fb := openTestFile(t, v, "aggregate.db")

	assertAggregate := func(want sqlite3vfs.LockType, label string) {
		t.Helper()
		if got := lockState(t, fcs, fa); got != want.String() {
			t.Errorf("%s: aggregate = %q, want %q", label, got, want.String())
		}
	}

	if err := fa.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("A Lock(Shared): %v", err)
	}
	if err := fb.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("B Lock(Shared): %v", err)
	}
	assertAggregate(sqlite3vfs.LockShared, "two shared holders")

	if err := fa.Lock(sqlite3vfs.LockReserved); err != nil {
		t.Fatalf("A Lock(Reserved): %v", err)
	}
	assertAggregate(sqlite3vfs.LockReserved, "A reserved over two shared")

	if err := fa.Unlock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("A Unlock(Shared): %v", err)
	}
	assertAggregate(sqlite3vfs.LockShared, "A back to shared")

	if err := fa.Lock(sqlite3vfs.LockReserved); err != nil {
		t.Fatalf("A Lock(Reserved) again: %v", err)
	}
	if err := fa.Lock(sqlite3vfs.LockExclusive); !errors.Is(err, sqlite3vfs.BusyError) {
		t.Fatalf("A Lock(Exclusive) while B holds Shared = %v, want sqlite3vfs.BusyError", err)
	}
	assertAggregate(sqlite3vfs.LockPending, "A stuck in Pending")

	if err := fb.Unlock(sqlite3vfs.LockNone); err != nil {
		t.Fatalf("B Unlock(None): %v", err)
	}
	assertAggregate(sqlite3vfs.LockPending, "B released, A still Pending")

	if err := fa.Lock(sqlite3vfs.LockExclusive); err != nil {
		t.Fatalf("A Lock(Exclusive) after B released: %v", err)
	}
	assertAggregate(sqlite3vfs.LockExclusive, "A exclusive")

	if err := fa.Unlock(sqlite3vfs.LockNone); err != nil {
		t.Fatalf("A Unlock(None): %v", err)
	}
	assertAggregate(sqlite3vfs.LockNone, "all released")
}

// TestSectorReadPathMakesNoLockCalls pins the zero-API-call rule for the
// hot sector-read path: once the cache is warm under a held lock, reads
// make no Kubernetes calls at all — the lease machinery must never sneak a
// renewal into ReadAt.
func TestSectorReadPathMakesNoLockCalls(t *testing.T) {
	v, fcs := newTestVFS(t)
	f := openTestFile(t, v, "hotread.db")

	data := make([]byte, 128)
	fillPattern(data)
	mustWriteAt(t, f, data, 0)
	if err := f.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("Lock(Shared): %v", err)
	}
	if _, err := f.FileSize(); err != nil {
		t.Fatalf("FileSize: %v", err)
	}

	fcs.ClearActions()
	if n := mustReadAt(t, f, make([]byte, 128), 0); n != 128 {
		t.Fatalf("read %d bytes, want 128", n)
	}
	for _, a := range fcs.Actions() {
		t.Errorf("read under a held SHARED lock made a %s %s call, want none (the hot read path must be cache-only)", a.GetVerb(), a.GetResource().Resource)
	}
}

// TestTwoVFSInstancesSeeEachOthersWritesAndTruncate covers the cross-
// instance handoff the sector cache exists to serve: two VFS instances over
// one fake clientset — two processes. A reads under SHARED and lets the
// lock go; B, seeing none of the state A warmed, rewrites the file larger
// and then truncates it; A re-locks and must see B's bytes and B's size,
// revalidated by FileSize's LIST rather than anything cached under A's
// previous lock. (The lock ConfigMap provides no real mutual exclusion —
// see the README — so B takes no lock here; that does not matter for the
// data and size visibility this test pins.)
func TestTwoVFSInstancesSeeEachOthersWritesAndTruncate(t *testing.T) {
	_, fcs := newTestVFS(t)
	va := NewVFS(t.Context(), fcs, testNamespace, slog.New(slog.DiscardHandler), 3)
	vb := NewVFS(t.Context(), fcs, testNamespace, slog.New(slog.DiscardHandler), 3)

	fa := openTestFile(t, va, "shared-file.db")
	first := make([]byte, 3000)
	fillPattern(first)
	mustWriteAt(t, fa, first, 0)

	// A reads under a held SHARED lock — warming every cache it has — then
	// lets the lock go.
	if err := fa.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("A Lock(Shared): %v", err)
	}
	got := make([]byte, len(first))
	if n := mustReadAt(t, fa, got, 0); n != len(first) {
		t.Fatalf("A read %d bytes, want %d", n, len(first))
	}
	if err := fa.Unlock(sqlite3vfs.LockNone); err != nil {
		t.Fatalf("A Unlock(None): %v", err)
	}

	// B writes new bytes — the file grows across two sectors — and then
	// truncates the file back down.
	fb := openTestFile(t, vb, "shared-file.db")
	second := make([]byte, 2*int(sectorSize)+100)
	fillPatternAlt(second)
	mustWriteAt(t, fb, second, 0)
	if err := fb.Truncate(45000); err != nil {
		t.Fatalf("B Truncate(45000): %v", err)
	}

	// A re-locks: its caches were dropped at Unlock, so it must revalidate
	// and see exactly the file B left behind.
	if err := fa.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("A re-Lock(Shared): %v", err)
	}
	size, err := fa.FileSize()
	if err != nil {
		t.Fatalf("A FileSize after B rewrite: %v", err)
	}
	if size != 45000 {
		t.Errorf("A FileSize = %d, want 45000 (B truncated size)", size)
	}
	got = make([]byte, 45000)
	if n := mustReadAt(t, fa, got, 0); n != 45000 {
		t.Fatalf("A read %d bytes, want 45000", n)
	}
	if !bytes.Equal(got, second[:45000]) {
		t.Error("A sees bytes other than B truncated data")
	}
	if sectors := listSectorConfigMaps(t, fcs, fa); len(sectors) != 1 {
		t.Errorf("%d sector configmaps remain after B write+truncate, want 1", len(sectors))
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
