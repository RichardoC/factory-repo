// Behaviour tests for the db-upload command against the fake kubernetes
// clientset: the sector-by-sector upload's stored bytes, and the failure
// paths (missing local file, failed sector write) that must return an
// error instead of exiting zero with a partial upload.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/RichardoC/kube-sqlite3-vfs/pkg/vfs"
	_ "github.com/mattn/go-sqlite3"
	"github.com/psanford/sqlite3vfs"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// sectorIndexFromName parses the sector index out of a sector ConfigMap
// name ("<base32 filename>-<index>"); the base32 alphabet holds no '-'.
func sectorIndexFromName(name string) (int64, bool) {
	i := strings.LastIndex(name, "-")
	if i < 0 {
		return 0, false
	}
	idx, err := strconv.ParseInt(name[i+1:], 10, 64)
	if err != nil {
		return 0, false
	}
	return idx, true
}

// interceptSectorWrites installs a reactor on every sector ConfigMap create
// and update: handle receives the verb, the sector index and the sector
// bytes, and returns an error to fail the write or replacement bytes to
// store instead. Non-sector ConfigMaps (the lockfile) pass through
// untouched.
func interceptSectorWrites(fcs *fake.Clientset, handle func(verb string, index int64, data []byte) ([]byte, error)) {
	reaction := func(action clienttesting.Action) (bool, runtime.Object, error) {
		var obj runtime.Object
		switch a := action.(type) {
		case clienttesting.CreateAction:
			obj = a.GetObject()
		case clienttesting.UpdateAction:
			obj = a.GetObject()
		default:
			return false, nil, nil
		}
		cm, ok := obj.(*corev1.ConfigMap)
		if !ok || cm.Labels["data"] != "sector" {
			return false, nil, nil
		}
		index, ok := sectorIndexFromName(cm.Name)
		if !ok {
			return false, nil, nil
		}
		replacement, err := handle(action.GetVerb(), index, cm.BinaryData["sector"])
		if err != nil {
			return true, nil, err
		}
		if replacement != nil {
			cm.BinaryData["sector"] = replacement
		}
		return false, nil, nil
	}
	fcs.PrependReactor("create", "configmaps", reaction)
	fcs.PrependReactor("update", "configmaps", reaction)
}

func listConfigMaps(t *testing.T, fcs *fake.Clientset, labelSelector string) []corev1.ConfigMap {
	t.Helper()
	cms, err := fcs.CoreV1().ConfigMaps("test").List(context.Background(), metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		t.Fatalf("list configmaps: %v", err)
	}
	return cms.Items
}

func TestRunWithClientUploadsSectorBySector(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	want := make([]byte, 2*vfs.SectorSize+123)
	if _, err := rand.Read(want); err != nil {
		t.Fatalf("read random bytes: %v", err)
	}
	if err := os.WriteFile("file2.db", want, 0o644); err != nil {
		t.Fatalf("write local file: %v", err)
	}

	fcs := fake.NewSimpleClientset()
	if err := runWithClient(fcs, Options{Retries: 1}, testLogger()); err != nil {
		t.Fatalf("runWithClient: %v", err)
	}

	sectors := listConfigMaps(t, fcs, "data=sector")
	if len(sectors) != 3 {
		t.Fatalf("sector ConfigMaps = %d, want 3", len(sectors))
	}
	sort.Slice(sectors, func(i, j int) bool {
		a, _ := sectorIndexFromName(sectors[i].Name)
		b, _ := sectorIndexFromName(sectors[j].Name)
		return a < b
	})
	for i, cm := range sectors {
		start := int64(i) * vfs.SectorSize
		end := min(start+vfs.SectorSize, int64(len(want)))
		if got := cm.BinaryData["sector"]; !bytes.Equal(got, want[start:end]) {
			t.Errorf("sector %d holds %d bytes, want %d matching bytes", i, len(got), end-start)
		}
	}
	if lockfiles := listConfigMaps(t, fcs, "data=lockfile"); len(lockfiles) != 1 {
		t.Errorf("lockfile ConfigMaps = %d, want 1", len(lockfiles))
	}
}

func TestRunWithClientFailsWhenLocalFileMissing(t *testing.T) {
	t.Chdir(t.TempDir()) // no file2.db here

	if err := runWithClient(fake.NewSimpleClientset(), Options{Retries: 1}, testLogger()); err == nil {
		t.Fatal("runWithClient succeeded without the local file")
	}
}

func TestRunWithClientFailsWhenSectorWriteFails(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("file2.db", make([]byte, 100), 0o644); err != nil {
		t.Fatalf("write local file: %v", err)
	}

	fcs := fake.NewSimpleClientset()
	// Open's own CREATE of the empty sector 0 goes through; the upload
	// then rewrites sector 0 (CREATE, falling back to UPDATE because the
	// sector exists) and that UPDATE fails.
	interceptSectorWrites(fcs, func(verb string, index int64, data []byte) ([]byte, error) {
		if verb == "update" && index == 0 {
			return nil, errors.New("injected sector write failure")
		}
		return nil, nil
	})

	err := runWithClient(fcs, Options{Retries: 1}, testLogger())
	if err == nil {
		t.Fatal("runWithClient succeeded despite an injected sector write failure")
	}
	if !strings.Contains(err.Error(), "write sector") {
		t.Errorf("error = %v, want it to report the failed sector write", err)
	}
}

// Uploading a smaller database over a larger remote used to leave the old
// database's trailing sectors in place (issue #332): WriteSector only
// creates and overwrites, so nothing shortened the remote. After the fix
// the re-upload truncates, and the cluster ends up holding exactly the
// smaller file's sectors and size — nothing of the old, larger database.
func TestRunWithClientUploadsSmallerFileOverLargerRemote(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	big := make([]byte, 3*vfs.SectorSize+123)
	if _, err := rand.Read(big); err != nil {
		t.Fatalf("read random bytes: %v", err)
	}
	if err := os.WriteFile("file2.db", big, 0o644); err != nil {
		t.Fatalf("write local file: %v", err)
	}

	fcs := fake.NewSimpleClientset()
	if err := runWithClient(fcs, Options{Retries: 1}, testLogger()); err != nil {
		t.Fatalf("first (large) upload: %v", err)
	}

	small := make([]byte, vfs.SectorSize+5)
	if _, err := rand.Read(small); err != nil {
		t.Fatalf("read random bytes: %v", err)
	}
	if err := os.WriteFile("file2.db", small, 0o644); err != nil {
		t.Fatalf("write local file: %v", err)
	}

	if err := runWithClient(fcs, Options{Retries: 1}, testLogger()); err != nil {
		t.Fatalf("second (small) upload over the larger remote: %v", err)
	}

	sectors := listConfigMaps(t, fcs, "data=sector")
	wantSectors := (int64(len(small)) + vfs.SectorSize - 1) / vfs.SectorSize
	if int64(len(sectors)) != wantSectors {
		t.Fatalf("sector ConfigMaps after the smaller upload = %d, want %d (the old database's trailing sectors must be gone)", len(sectors), wantSectors)
	}
	sort.Slice(sectors, func(i, j int) bool {
		a, _ := sectorIndexFromName(sectors[i].Name)
		b, _ := sectorIndexFromName(sectors[j].Name)
		return a < b
	})
	for i, cm := range sectors {
		start := int64(i) * vfs.SectorSize
		end := min(start+vfs.SectorSize, int64(len(small)))
		if got := cm.BinaryData["sector"]; !bytes.Equal(got, small[start:end]) {
			t.Errorf("sector %d holds %d bytes, want %d bytes of the smaller file", i, len(got), end-start)
		}
	}

	// The size the cluster reports must be the smaller file's, too.
	vfsN := vfs.NewVFS(context.Background(), fcs, "test", testLogger(), 1)
	if _, _, err := vfsN.Open("file2.db", sqlite3vfs.OpenFlag(0)); err != nil {
		t.Fatalf("open uploaded file: %v", err)
	}
	size, err := vfs.NewFile("file2.db", vfsN).FileSize()
	if err != nil {
		t.Fatalf("remote FileSize: %v", err)
	}
	if size != int64(len(small)) {
		t.Errorf("remote FileSize = %d, want %d", size, len(small))
	}
}

// The upload's final size check must fail the run when the remote does not
// match the local file, rather than exiting zero over a stale remote. The
// upload issues exactly three sector LISTs — one in Open, one for
// Truncate's FileSize and one for the final verification — so the reactor
// below lets the first two through and injects a phantom trailing sector
// into the third: the cluster then reports a size larger than the local
// file, and the command must report the mismatch.
func TestRunWithClientFailsWhenRemoteSizeDoesNotMatchLocal(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	want := make([]byte, vfs.SectorSize+7)
	if _, err := rand.Read(want); err != nil {
		t.Fatalf("read random bytes: %v", err)
	}
	if err := os.WriteFile("file2.db", want, 0o644); err != nil {
		t.Fatalf("write local file: %v", err)
	}

	fcs := fake.NewSimpleClientset()
	// The stale-journal lookup added for issue #352 lists sector
	// ConfigMaps too — but the journal's, not the database's. This reactor
	// counts only the DATABASE's sector LISTs (the upload issues exactly
	// three of them) so the phantom still lands on the size-check list.
	journalLabel := vfs.NewFile("file2.db-journal", vfs.NewVFS(context.Background(), fcs, "test", testLogger(), 1)).SectorLabels["relevant-file"]
	lists := 0
	fcs.PrependReactor("list", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
		listAction, ok := action.(clienttesting.ListAction)
		if !ok {
			return false, nil, nil
		}
		if strings.Contains(listAction.GetListRestrictions().Labels.String(), journalLabel) {
			return false, nil, nil
		}
		lists++
		if lists != 3 {
			return false, nil, nil
		}
		obj, err := fcs.Tracker().List(
			corev1.SchemeGroupVersion.WithResource("configmaps"),
			corev1.SchemeGroupVersion.WithKind("ConfigMap"),
			action.GetNamespace(), metav1.ListOptions{LabelSelector: listAction.GetListRestrictions().Labels.String()})
		if err != nil {
			return true, nil, err
		}
		cmList := obj.(*corev1.ConfigMapList)
		// A phantom sector 9 past the uploaded file's end: stale bytes the
		// truncation is supposed to have removed.
		phantom := *cmList.Items[0].DeepCopy()
		name := phantom.Name
		cut := strings.LastIndex(name, "-")
		phantom.Name = name[:cut+1] + "9"
		phantom.BinaryData = map[string][]byte{"sector": bytes.Repeat([]byte{0xAA}, vfs.SectorSize)}
		cmList.Items = append(cmList.Items, phantom)
		return true, cmList, nil
	})

	err := runWithClient(fcs, Options{Retries: 1}, testLogger())
	if err == nil {
		t.Fatal("runWithClient succeeded despite the remote size not matching the local file")
	}
	if !strings.Contains(err.Error(), "size mismatch") {
		t.Errorf("error = %v, want it to report the size mismatch", err)
	}
}

// --- issue #352: a crashed writer's hot rollback journal must not survive
// --- an upload, or the next read-write opener replays the OLD page
// --- contents over the freshly uploaded image.

var testVFSNameCounter atomic.Uint64

// pinnedWriters keeps the abandoned "crashed" writer connections of
// seedHotJournal reachable for the whole test binary: see the comment there.
var pinnedWriters []any

// registerTestVFS registers v under a process-unique name:
// sqlite3vfs.RegisterVFS is process-global, so every test needs its own.
func registerTestVFS(t *testing.T, v sqlite3vfs.VFS) string {
	t.Helper()
	name := fmt.Sprintf("db-upload-test-vfs-%d", testVFSNameCounter.Add(1))
	if err := sqlite3vfs.RegisterVFS(name, v); err != nil {
		t.Fatalf("RegisterVFS(%q): %v", name, err)
	}
	return name
}

// buildSQLiteFile creates a real SQLite database with a books table and
// rows rows, big enough to span more than one 64 KiB sector.
func buildSQLiteFile(t *testing.T, rows int) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "source.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("open temp sqlite file: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE books (id text NOT NULL PRIMARY KEY, title text)`); err != nil {
		t.Fatalf("create books table: %v", err)
	}
	for i := 0; i < rows; i++ {
		if _, err := db.Exec("INSERT INTO books (id, title) VALUES (?, ?)", fmt.Sprintf("id-%d-of-%d", i, rows), strings.Repeat("t", 200)); err != nil {
			t.Fatalf("insert row %d: %v", i, err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read temp sqlite file: %v", err)
	}
	if int64(len(data)) <= vfs.SectorSize {
		t.Fatalf("built %d rows into %d bytes; want more than one %d-byte sector", rows, len(data), vfs.SectorSize)
	}
	return data
}

// seedHotJournal simulates a writer that crashed mid-transaction over the
// remote database: it opens the remote database through a real SQLite
// connection in DELETE journal mode (so the rollback journal is stored on
// the cluster through the VFS, with its real magic and record checksums),
// starts a transaction big enough to spill the page cache — which is what
// makes SQLite write the hot journal header — and abandons the connection
// without committing, closing or rolling back. What stays behind is
// precisely a crashed writer's state: a hot journal over the old image. The
// abandoned writer's lock ConfigMaps are removed the way expired leases
// would be, so the next opener is not blocked by them. Returns the label
// selector matching the journal's sector ConfigMaps.
func seedHotJournal(t *testing.T, fcs *fake.Clientset) string {
	t.Helper()
	vfsN := vfs.NewVFS(context.Background(), fcs, "test", testLogger(), 1)
	vfsName := registerTestVFS(t, vfsN)
	db, err := sql.Open("sqlite3", fmt.Sprintf("file:file2.db?_journal=DELETE&vfs=%s", vfsName))
	if err != nil {
		t.Fatalf("open the remote database as the doomed writer: %v", err)
	}
	db.SetMaxOpenConns(1)
	// A tiny page cache: SQLite writes the hot journal header at the first
	// cache spill, so a couple of dirty pages is all the crash needs — a
	// bigger transaction would also spill database pages and grow the old
	// image, which this issue is not about.
	if _, err := db.Exec("PRAGMA cache_size = 2"); err != nil {
		t.Fatalf("shrink the doomed writer cache: %v", err)
	}
	// Deliberately never committed, closed or rolled back: the test wants
	// the connection abandoned mid-transaction, like a crashed writer. The
	// references below keep it REACHABLE for the whole test: a collected
	// connection would be finalized, and its deferred close rolls the open
	// transaction back through the writer's still-cached journal handle —
	// a recovery no crashed writer can perform, which would silently undo
	// the crash this test simulates.
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin the doomed transaction: %v", err)
	}
	pinnedWriters = append(pinnedWriters, db, tx)
	for i := 0; i < 50; i++ {
		if _, err := tx.Exec("INSERT INTO books (id, title) VALUES (?, ?)", fmt.Sprintf("uncommitted-%d", i), strings.Repeat("j", 700)); err != nil {
			t.Fatalf("insert row %d: %v", i, err)
		}
	}

	// The journal must be HOT: non-zero magic (the journal header) at
	// offset 0, and at least one sector ConfigMap on the cluster.
	jf := vfs.NewFile("file2.db-journal", vfsN)
	selector := "data=sector,relevant-file=" + jf.SectorLabels["relevant-file"]
	if cms := listConfigMaps(t, fcs, selector); len(cms) == 0 {
		t.Fatal("setup: the abandoned transaction left no journal sector ConfigMaps")
	}
	var head [8]byte
	n, err := jf.ReadAt(head[:], 0)
	if err != nil || n != 8 {
		t.Fatalf("setup: read journal header: n=%d err=%v", n, err)
	}
	if bytes.Equal(head[:], make([]byte, 8)) {
		t.Fatalf("setup: journal header at offset 0 is all zeros: the seeded journal is cold, not hot")
	}

	// The crash: the writer is gone, so its lock holdings must not block
	// the next opener — drop the DATABASE's lock ConfigMap the way an
	// expired lease entry is pruned. The JOURNAL's lockfile stays, exactly
	// as a crashed writer leaves it: it is part of the journal's on-cluster
	// state (a read-only opener must still be able to open the journal to
	// read its magic), and the journal delete below removes it.
	dbLock := vfs.NewFile("file2.db", vfsN).LockFileName()
	if err := fcs.CoreV1().ConfigMaps("test").Delete(context.Background(), dbLock, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("setup: drop abandoned writer lockfile %s: %v", dbLock, err)
	}
	return selector
}

// The regression: a writer that crashed mid-transaction left a HOT journal
// ("<db>-journal", non-zero magic at offset 0) over the remote database.
// The upload must delete it BEFORE writing the fresh image — the journal
// describes the old page contents, so a journal that survives the upload is
// replayed by the next read-write opener and resurrects the old database
// (issue #352). After the upload the reopened database must be exactly the
// uploaded file, with no journal ConfigMap left on the cluster.
func TestRunWithClientDeletesHotJournalBeforeUpload(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	// The cluster holds an old database image, left by an earlier upload.
	if err := os.WriteFile("file2.db", buildSQLiteFile(t, 500), 0o644); err != nil {
		t.Fatalf("write old local file: %v", err)
	}
	fcs := fake.NewSimpleClientset()
	if err := runWithClient(fcs, Options{Retries: 1}, testLogger()); err != nil {
		t.Fatalf("seed upload of the old image: %v", err)
	}

	// A writer crashed mid-transaction over it: a hot journal sits beside
	// the old image, describing the OLD pages.
	selector := seedHotJournal(t, fcs)

	// The fresh image the run under test uploads.
	newData := buildSQLiteFile(t, 2000)
	if err := os.WriteFile("file2.db", newData, 0o644); err != nil {
		t.Fatalf("write new local file: %v", err)
	}
	if err := runWithClient(fcs, Options{Retries: 1}, testLogger()); err != nil {
		t.Fatalf("runWithClient over a hot journal: %v", err)
	}

	// The journal must be gone before (and after) the upload: checked
	// BEFORE the reopen below, because a read-write opener would itself
	// replay and then delete a journal the upload failed to remove.
	if cms := listConfigMaps(t, fcs, selector); len(cms) != 0 {
		t.Fatalf("journal sector ConfigMaps after the upload = %d, want 0 (a surviving journal would be replayed over the fresh image)", len(cms))
	}

	// Reopen the uploaded database and check it IS the uploaded file: the
	// new image's row count, and a clean integrity check.
	vfsN := vfs.NewVFS(context.Background(), fcs, "test", testLogger(), 1)
	vfsName := registerTestVFS(t, vfsN)
	db, err := sql.Open("sqlite3", fmt.Sprintf("file:file2.db?_journal=DELETE&vfs=%s", vfsName))
	if err != nil {
		t.Fatalf("sql.Open over the uploaded database: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM books").Scan(&count); err != nil {
		t.Fatalf("row count over the reopened database: %v", err)
	}
	if count != 2000 {
		t.Errorf("reopened database holds %d rows, want 2000 (the uploaded file's count)", count)
	}
	var integrity string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil {
		t.Fatalf("PRAGMA integrity_check: %v", err)
	}
	if integrity != "ok" {
		t.Errorf("PRAGMA integrity_check = %q, want \"ok\"", integrity)
	}
}

// With no journal on the cluster the upload must behave exactly as before:
// the stored sectors match the local file and no journal appears.
func TestRunWithClientUploadsUnchangedWithoutJournal(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	data := buildSQLiteFile(t, 2000)
	if err := os.WriteFile("file2.db", data, 0o644); err != nil {
		t.Fatalf("write local file: %v", err)
	}

	fcs := fake.NewSimpleClientset()
	if err := runWithClient(fcs, Options{Retries: 1}, testLogger()); err != nil {
		t.Fatalf("runWithClient: %v", err)
	}

	assertSectorsMatch(t, fcs, data)

	vfsN := vfs.NewVFS(context.Background(), fcs, "test", testLogger(), 1)
	jf := vfs.NewFile("file2.db-journal", vfsN)
	if cms := listConfigMaps(t, fcs, "data=sector,relevant-file="+jf.SectorLabels["relevant-file"]); len(cms) != 0 {
		t.Errorf("journal sector ConfigMaps = %d, want 0: an upload with no journal must not create one", len(cms))
	}
}

// assertSectorsMatch sorts the cluster's sector ConfigMaps by index and
// checks each holds the matching slice of want.
func assertSectorsMatch(t *testing.T, fcs *fake.Clientset, want []byte) {
	t.Helper()
	sectors := listConfigMaps(t, fcs, "data=sector")
	wantCount := (int64(len(want)) + vfs.SectorSize - 1) / vfs.SectorSize
	if int64(len(sectors)) != wantCount {
		t.Fatalf("sector ConfigMaps = %d, want %d", len(sectors), wantCount)
	}
	sort.Slice(sectors, func(i, j int) bool {
		a, _ := sectorIndexFromName(sectors[i].Name)
		b, _ := sectorIndexFromName(sectors[j].Name)
		return a < b
	})
	for i, cm := range sectors {
		start := int64(i) * vfs.SectorSize
		end := min(start+vfs.SectorSize, int64(len(want)))
		if got := cm.BinaryData["sector"]; !bytes.Equal(got, want[start:end]) {
			t.Errorf("sector %d holds %d bytes, want %d matching bytes", i, len(got), end-start)
		}
	}
}
