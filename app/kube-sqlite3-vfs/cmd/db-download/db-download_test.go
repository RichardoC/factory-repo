// Behaviour tests for the db-download command against the fake kubernetes
// clientset: the transfer and its verification on the happy path, and the
// failure paths (missing source, failed write, size mismatch, byte
// mismatch) that used to be logged and still exit zero.
package main

import (
	"bytes"
	"context"
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

// buildSQLiteFile creates a real SQLite database with a books table and
// rows rows, big enough to span more than one 64 KiB sector.
func buildSQLiteFile(t *testing.T, rows int) (string, []byte) {
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
	return path, data
}

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
	if lockfiles := listConfigMaps(t, fcs, "data=lockfile"); len(lockfiles) != 1 {
		t.Errorf("lockfile ConfigMaps = %d, want 1", len(lockfiles))
	}
}

func TestRunWithClientTransfersAndVerifies(t *testing.T) {
	path, data := buildSQLiteFile(t, 2000)
	fcs := fake.NewSimpleClientset()

	if err := runWithClient(fcs, Options{FileName: path, Retries: 1}, testLogger()); err != nil {
		t.Fatalf("runWithClient: %v", err)
	}

	assertSectorsMatch(t, fcs, data)
}

func TestRunWithClientFailsWhenLocalFileMissing(t *testing.T) {
	if err := runWithClient(fake.NewSimpleClientset(), Options{FileName: filepath.Join(t.TempDir(), "no-such.db"), Retries: 1}, testLogger()); err == nil {
		t.Fatal("runWithClient succeeded without the local file")
	}
}

func TestRunWithClientFailsWhenWriteFails(t *testing.T) {
	path, _ := buildSQLiteFile(t, 2000)
	fcs := fake.NewSimpleClientset()
	// The VFS's Open (CREATE of the empty sector 0) goes through; the
	// transfer's rewrite of sector 0 (UPDATE) fails.
	interceptSectorWrites(fcs, func(verb string, index int64, data []byte) ([]byte, error) {
		if verb == "update" {
			return nil, errors.New("injected sector write failure")
		}
		return nil, nil
	})

	err := runWithClient(fcs, Options{FileName: path, Retries: 1}, testLogger())
	if err == nil {
		t.Fatal("runWithClient succeeded despite an injected sector write failure")
	}
	if !strings.Contains(err.Error(), "failed to write the source file") {
		t.Errorf("error = %v, want it to report the failed write", err)
	}
}

func TestRunWithClientFailsOnSizeMismatch(t *testing.T) {
	path, _ := buildSQLiteFile(t, 2000)
	fcs := fake.NewSimpleClientset()
	// Store every sector past 0 truncated to 10 bytes: the write API calls
	// all succeed, so the transfer completes, but the cluster's file is
	// shorter than the local one.
	interceptSectorWrites(fcs, func(verb string, index int64, data []byte) ([]byte, error) {
		if verb == "create" && index >= 1 && len(data) > 10 {
			return data[:10], nil
		}
		return nil, nil
	})

	err := runWithClient(fcs, Options{FileName: path, Retries: 1}, testLogger())
	if err == nil {
		t.Fatal("runWithClient succeeded despite the truncated remote sectors")
	}
	if !strings.Contains(err.Error(), "size mismatch") {
		t.Errorf("error = %v, want it to report the size mismatch", err)
	}
}

func TestRunWithClientFailsOnByteMismatch(t *testing.T) {
	path, _ := buildSQLiteFile(t, 2000)
	fcs := fake.NewSimpleClientset()
	// Corrupt the first byte of every sector past 0 as it is stored: the
	// size stays right, so the size check passes and the byte comparison
	// must catch the damage.
	interceptSectorWrites(fcs, func(verb string, index int64, data []byte) ([]byte, error) {
		if verb == "create" && index >= 1 && len(data) > 0 {
			corrupt := append([]byte(nil), data...)
			corrupt[0] ^= 0xff
			return corrupt, nil
		}
		return nil, nil
	})

	err := runWithClient(fcs, Options{FileName: path, Retries: 1}, testLogger())
	if err == nil {
		t.Fatal("runWithClient succeeded despite the corrupted remote sectors")
	}
	if !strings.Contains(err.Error(), "differ from the local file") {
		t.Errorf("error = %v, want it to report the byte mismatch", err)
	}
}

// TestJournalModeValidation covers the reader's --journal-mode validation,
// which delegates to the same shared journalmode package as the writer's
// (issue #318): the modes a reader may run under (DELETE, TRUNCATE,
// PERSIST, case-insensitively, and the flag's empty default, DELETE), and
// the modes it must refuse — MEMORY and OFF for the crash-safety reason
// from issue #68, WAL for the -shm file this VFS cannot store, and anything
// else as unknown.
func TestJournalModeValidation(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr string
	}{
		{in: "", want: "DELETE"},                         // the flag's default
		{in: "DELETE", want: "DELETE"},                   // accepted, canonical case
		{in: "TRUNCATE", want: "TRUNCATE"},               // accepted
		{in: "truncate", want: "TRUNCATE"},               // accepted, lower case
		{in: "PERSIST", want: "PERSIST"},                 // accepted
		{in: " truncate ", want: "TRUNCATE"},             // accepted, surrounding space trimmed
		{in: "MEMORY", wantErr: "cannot be rolled back"}, // refused: crash-safety (issue #68)
		{in: "OFF", wantErr: "cannot be rolled back"},    // refused: crash-safety (issue #68)
		{in: "WAL", wantErr: "shared-memory -shm"},       // refused: no -shm through this VFS
		{in: "bogus", wantErr: "unknown --journal-mode"}, // refused: not a mode
	}
	for _, tc := range cases {
		got, err := journalModeName(tc.in)
		if tc.wantErr != "" {
			if err == nil {
				t.Errorf("journalModeName(%q) = %q, want error mentioning %q", tc.in, got, tc.wantErr)
				continue
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("journalModeName(%q) error = %q, want it to mention %q", tc.in, err, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("journalModeName(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("journalModeName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestReaderDSNCarriesJournalMode shows that the requested mode reaches the
// SQLite DSN the reader opens (issue #318): the DSN carries
// _journal=<mode> alongside the read-only parameters, the flag's empty
// default behaves as DELETE, and a refused mode produces no DSN at all.
func TestReaderDSNCarriesJournalMode(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{in: "", want: "file2.db?_query_only=true&_journal=DELETE"}, // the flag's default
		{in: "DELETE", want: "file2.db?_query_only=true&_journal=DELETE"},
		{in: "TRUNCATE", want: "file2.db?_query_only=true&_journal=TRUNCATE"},
		{in: "PERSIST", want: "file2.db?_query_only=true&_journal=PERSIST"},
	} {
		dsn, err := readerDSN(tc.in)
		if err != nil {
			t.Fatalf("readerDSN(%q): %v", tc.in, err)
		}
		if dsn != tc.want+"&mode=ro&sqlite_os_trace=&vfs=kube-sqlite3-vfs" {
			t.Errorf("readerDSN(%q) = %q, want it to carry %q", tc.in, dsn, tc.want)
		}
	}
	for _, mode := range []string{"MEMORY", "OFF", "WAL", "bogus"} {
		if dsn, err := readerDSN(mode); err == nil {
			t.Errorf("readerDSN(%q) = %q, want an error: a refused mode must not produce a DSN", mode, dsn)
		}
	}
}

// TestRunWithClientHonoursJournalModeFlag drives the reader with
// --journal-mode=TRUNCATE end to end over the fake clientset: the mode in
// the DSN is one SQLite accepts over this VFS (a refused or mangled DSN
// parameter fails the open), so the transfer and its read-back COUNT query
// succeed through a TRUNCATE-mode connection.
func TestRunWithClientHonoursJournalModeFlag(t *testing.T) {
	path, _ := buildSQLiteFile(t, 500)
	fcs := fake.NewSimpleClientset()

	if err := runWithClient(fcs, Options{FileName: path, Retries: 1, JournalMode: "TRUNCATE"}, testLogger()); err != nil {
		t.Fatalf("runWithClient with --journal-mode=TRUNCATE: %v", err)
	}
}

// TestRunWithClientRefusesMemoryJournalMode shows the refusal at the run's
// front door: a reader asked for _journal=MEMORY fails before any cluster
// work, with the crash-safety reason from issue #68 in the error.
func TestRunWithClientRefusesMemoryJournalMode(t *testing.T) {
	fcs := fake.NewSimpleClientset()

	err := runWithClient(fcs, Options{FileName: filepath.Join(t.TempDir(), "no-such.db"), Retries: 1, JournalMode: "MEMORY"}, testLogger())
	if err == nil {
		t.Fatal("runWithClient succeeded with --journal-mode=MEMORY, want a crash-safety refusal")
	}
	if !strings.Contains(err.Error(), "cannot be rolled back") {
		t.Errorf("error = %q, want it to give the crash-safety reason (issue #68)", err)
	}
	if cms := listConfigMaps(t, fcs, "data=sector"); len(cms) != 0 {
		t.Errorf("a refused run wrote %d sector ConfigMaps, want 0: the refusal must come before any cluster work", len(cms))
	}
}

// --- issue #352: a crashed writer's hot rollback journal must not survive
// --- a download, or the next read-write opener replays the OLD page
// --- contents over the freshly written image.

var testVFSNameCounter atomic.Uint64

// pinnedWriters keeps the abandoned "crashed" writer connections of
// seedHotJournal reachable for the whole test binary: see the comment there.
var pinnedWriters []any

// registerTestVFS registers v under a process-unique name:
// sqlite3vfs.RegisterVFS is process-global, so every test needs its own
// (the command itself registers "kube-sqlite3-vfs").
func registerTestVFS(t *testing.T, v sqlite3vfs.VFS) string {
	t.Helper()
	name := fmt.Sprintf("db-download-test-vfs-%d", testVFSNameCounter.Add(1))
	if err := sqlite3vfs.RegisterVFS(name, v); err != nil {
		t.Fatalf("RegisterVFS(%q): %v", name, err)
	}
	return name
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
// The download must delete it BEFORE writing the fresh image — the journal
// describes the old page contents, so a journal that survives the download
// is replayed by the next read-write opener and resurrects the old database
// (issue #352). After the download the reopened database must be exactly
// the uploaded file, with no journal ConfigMap left on the cluster.
func TestRunWithClientDeletesHotJournalBeforeDownload(t *testing.T) {
	// The cluster holds an old database image.
	_, oldData := buildSQLiteFile(t, 500)
	fcs := fake.NewSimpleClientset()
	vfsN := vfs.NewVFS(context.Background(), fcs, "test", testLogger(), 1)
	if _, err := vfs.NewFile("file2.db", vfsN).WriteAt(oldData, 0); err != nil {
		t.Fatalf("seed the old image: %v", err)
	}

	// A writer crashed mid-transaction over it: a hot journal sits beside
	// the old image, describing the OLD pages.
	selector := seedHotJournal(t, fcs)

	// The fresh image the run under test writes to the cluster.
	newPath, _ := buildSQLiteFile(t, 2000)
	if err := runWithClient(fcs, Options{FileName: newPath, Retries: 1}, testLogger()); err != nil {
		t.Fatalf("runWithClient over a hot journal: %v", err)
	}

	// The journal must be gone: checked BEFORE the reopen below, because a
	// read-write opener would itself replay and then delete a journal the
	// download failed to remove.
	if cms := listConfigMaps(t, fcs, selector); len(cms) != 0 {
		t.Fatalf("journal sector ConfigMaps after the download = %d, want 0 (a surviving journal would be replayed over the fresh image)", len(cms))
	}

	// Reopen the downloaded database read-only, as the command's own reader
	// does, and check it IS the uploaded file: the new image's row count,
	// and a clean integrity check.
	readerVFS := vfs.NewVFS(context.Background(), fcs, "test", testLogger(), 1)
	vfsName := registerTestVFS(t, readerVFS)
	db, err := sql.Open("sqlite3", fmt.Sprintf("file:file2.db?_query_only=true&_journal=DELETE&mode=ro&vfs=%s", vfsName))
	if err != nil {
		t.Fatalf("sql.Open over the downloaded database: %v", err)
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

// With no journal on the cluster the download must behave exactly as
// before: the stored bytes match the local file and no journal appears.
func TestRunWithClientDownloadsWithoutJournal(t *testing.T) {
	path, data := buildSQLiteFile(t, 500)
	fcs := fake.NewSimpleClientset()

	if err := runWithClient(fcs, Options{FileName: path, Retries: 1}, testLogger()); err != nil {
		t.Fatalf("runWithClient: %v", err)
	}

	assertSectorsMatch(t, fcs, data)

	vfsN := vfs.NewVFS(context.Background(), fcs, "test", testLogger(), 1)
	jf := vfs.NewFile("file2.db-journal", vfsN)
	if cms := listConfigMaps(t, fcs, "data=sector,relevant-file="+jf.SectorLabels["relevant-file"]); len(cms) != 0 {
		t.Errorf("journal sector ConfigMaps = %d, want 0: a download with no journal must not create one", len(cms))
	}
}
