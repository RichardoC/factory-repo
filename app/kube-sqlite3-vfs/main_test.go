// Behaviour tests for the demo command, driven with the fake kubernetes
// clientset: the happy path inserts and commits 1000 rows through sqlite
// over the VFS, and a failed sector write must fail the run at the failed
// insert instead of being logged and committed anyway.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"

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
// and update: handle receives the sector index and the sector bytes and
// returns an error to fail the write, or replacement bytes to store
// instead. Non-sector ConfigMaps (the lockfile) pass through untouched.
func interceptSectorWrites(fcs *fake.Clientset, handle func(index int64, data []byte) ([]byte, error)) {
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
		replacement, err := handle(index, cm.BinaryData["sector"])
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

func TestRunWithClientInsertsAndCommits1000Rows(t *testing.T) {
	fcs := fake.NewSimpleClientset()

	if err := runWithClient(fcs, Options{Retries: 1}, testLogger()); err != nil {
		t.Fatalf("runWithClient: %v", err)
	}

	// The committed database must be readable through a fresh connection
	// over the same VFS: 1000 rows on the books table. The reader opens the
	// same way the writer now does (issue #68): _journal=DELETE, so a hot
	// journal left by a crashed writer would be rolled back rather than
	// read through as if committed.
	db, err := sql.Open("sqlite3", "file2.db?_query_only=true&_journal=DELETE&mode=ro&sqlite_os_trace=&vfs=kube-sqlite3-vfs")
	if err != nil {
		t.Fatalf("reopen database over the VFS: %v", err)
	}
	defer db.Close()

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM books").Scan(&count); err != nil {
		t.Fatalf("count books over the VFS: %v", err)
	}
	if count != 1000 {
		t.Errorf("row count after run = %d, want 1000", count)
	}
	if sectors := listConfigMaps(t, fcs, "data=sector"); len(sectors) == 0 {
		t.Error("no sector ConfigMaps were written")
	}
	if lockfiles := listConfigMaps(t, fcs, "data=lockfile"); len(lockfiles) != 1 {
		t.Errorf("lockfile ConfigMaps = %d, want 1", len(lockfiles))
	}

	// A clean commit leaves NO journal behind (issue #68): _journal=DELETE
	// removes "<db>-journal" at commit, so after runWithClient has
	// committed and closed, the on-cluster layout must contain no sector
	// ConfigMap of the journal file — no stray "<base32(file2.db-journal)>"
	// sectors. The layout is readable: journal sectors carry the same
	// data=sector label as the database's own, and their ConfigMap name is
	// the base32 encoding of the journal file name, which the database's
	// sectors never share. A leftover journal would mean the commit never
	// deleted it, and the next opener would pay a spurious recovery.
	journalSectors := 0
	for _, cm := range listConfigMaps(t, fcs, "data=sector") {
		if idx, ok := sectorIndexFromName(cm.Name); !ok {
			t.Errorf("sector ConfigMap %q has no trailing -<index>", cm.Name)
			continue
		} else {
			_ = idx
		}
		if cm.Data["filename"] == "file2.db-journal" {
			journalSectors++
		}
	}
	if journalSectors != 0 {
		t.Errorf("journal sector ConfigMaps left after a clean commit = %d, want 0 (DELETE must delete the journal at commit)", journalSectors)
	}
	// The journal file must not exist by cluster truth either, which is
	// what the VFS's Access reports for a "-journal" name: it exists
	// exactly when it has at least one sector ConfigMap.
	if journal := listConfigMaps(t, fcs, "data=sector"); len(journal) != 0 {
		for _, cm := range journal {
			if cm.Data["filename"] != "file2.db" {
				t.Errorf("unexpected sector ConfigMap for %q after a clean commit, want only file2.db", cm.Data["filename"])
			}
		}
	}
}

func TestRunWithClientFailsOnInsertError(t *testing.T) {
	fcs := fake.NewSimpleClientset()

	// SQLite takes the RESERVED lock at the first write of every write
	// transaction: once for CREATE TABLE, once for the insert transaction.
	// A failure on the second RESERVED lock write therefore lands inside
	// the insert loop on its first statement — the one hundred percent
	// deterministic way to fail an insert through the VFS, since with this
	// row size every sector write of the insert transaction happens at its
	// commit.
	reservedWrites := 0
	lockReactor := func(action clienttesting.Action) (bool, runtime.Object, error) {
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
		if !ok || cm.Labels["data"] != "lockfile" || cm.Data["lock"] != sqlite3vfs.LockReserved.String() {
			return false, nil, nil
		}
		reservedWrites++
		if reservedWrites >= 2 {
			return true, nil, fmt.Errorf("injected lock write failure")
		}
		return false, nil, nil
	}
	fcs.PrependReactor("create", "configmaps", lockReactor)
	fcs.PrependReactor("update", "configmaps", lockReactor)

	err := runWithClient(fcs, Options{Retries: 1}, testLogger())
	if err == nil {
		t.Fatal("runWithClient succeeded despite injected sector write failures")
	}
	if !strings.Contains(err.Error(), "insert row") {
		t.Errorf("error = %v, want it to report the failed insert (not an earlier or later step)", err)
	}
}

func TestRunWithClientFailsOnCommitError(t *testing.T) {
	fcs := fake.NewSimpleClientset()

	// The VFS opens the file with an empty sector 0, so CREATE TABLE's
	// writes (an UPDATE of sector 0 and 1) go through; a failure on the
	// CREATE of every sector past 1 fails the insert transaction's commit.
	// The run must report the failure, not exit zero with a partial
	// database.
	interceptSectorWrites(fcs, func(index int64, data []byte) ([]byte, error) {
		if index >= 2 {
			return nil, fmt.Errorf("injected sector %d write failure", index)
		}
		return nil, nil
	})

	if err := runWithClient(fcs, Options{Retries: 1}, testLogger()); err == nil {
		t.Fatal("runWithClient succeeded despite injected sector write failures")
	}
}

// TestJournalModeValidation covers --journal-mode's validation: the modes a
// writer may run under (DELETE, TRUNCATE, PERSIST, case-insensitively, and
// the flag's empty default, which is DELETE), the modes it must refuse with
// the crash-safety reason from issue #68 (MEMORY, OFF — their journal never
// reaches the VFS, so a crashed writer cannot be rolled back), WAL (needs
// the shared-memory -shm file this VFS cannot store) and an unknown value.
func TestJournalModeValidation(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr string
	}{
		{in: "", want: "DELETE"},                            // the flag's default
		{in: "DELETE", want: "DELETE"},                      // accepted, canonical case
		{in: "delete", want: "DELETE"},                      // accepted, sqlite is case-insensitive
		{in: "TRUNCATE", want: "TRUNCATE"},                  // accepted
		{in: "truncate", want: "TRUNCATE"},                  // accepted, lower case
		{in: "PERSIST", want: "PERSIST"},                    // accepted
		{in: "persist", want: "PERSIST"},                    // accepted, lower case
		{in: "MEMORY", wantErr: "cannot be rolled back"},    // refused: crash-safety (issue #68)
		{in: "memory", wantErr: "cannot be rolled back"},    // refused, lower case
		{in: "OFF", wantErr: "cannot be rolled back"},       // refused: crash-safety (issue #68)
		{in: "WAL", wantErr: "shared-memory -shm"},          // refused: no -shm through this VFS
		{in: "bogus", wantErr: "unknown --journal-mode"},    // refused: not a mode
		{in: "bogus", wantErr: "DELETE, TRUNCATE, PERSIST"}, // refused: names the accepted modes
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

// TestWriterDSNCarriesJournalMode shows that the chosen mode reaches the
// SQLite DSN: the DSN the writer opens carries _journal=<mode>, and a
// refused mode produces no DSN at all.
func TestWriterDSNCarriesJournalMode(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{in: "", want: "file2.db?_journal=DELETE"}, // the flag's default
		{in: "DELETE", want: "file2.db?_journal=DELETE"},
		{in: "TRUNCATE", want: "file2.db?_journal=TRUNCATE"},
		{in: "PERSIST", want: "file2.db?_journal=PERSIST"},
	} {
		dsn, err := writerDSN(tc.in)
		if err != nil {
			t.Fatalf("writerDSN(%q): %v", tc.in, err)
		}
		if dsn != tc.want+"&_cache_size=-256&vfs=kube-sqlite3-vfs" {
			t.Errorf("writerDSN(%q) = %q, want it to carry %q", tc.in, dsn, tc.want)
		}
	}
	for _, mode := range []string{"MEMORY", "OFF", "WAL", "bogus"} {
		if dsn, err := writerDSN(mode); err == nil {
			t.Errorf("writerDSN(%q) = %q, want an error: a refused mode must not produce a DSN", mode, dsn)
		}
	}
}

// TestRunWithClientHonoursJournalModeFlag drives the writer with
// --journal-mode=TRUNCATE end to end over the fake clientset: the mode in
// the DSN is one SQLite accepts over this VFS (a refused or mangled DSN
// parameter fails the open), the 1000 rows still commit, and the committed
// database reads back through a fresh TRUNCATE-mode connection.
func TestRunWithClientHonoursJournalModeFlag(t *testing.T) {
	fcs := fake.NewSimpleClientset()

	if err := runWithClient(fcs, Options{Retries: 1, JournalMode: "TRUNCATE"}, testLogger()); err != nil {
		t.Fatalf("runWithClient with --journal-mode=TRUNCATE: %v", err)
	}

	db, err := sql.Open("sqlite3", "file2.db?_query_only=true&_journal=TRUNCATE&mode=ro&vfs=kube-sqlite3-vfs")
	if err != nil {
		t.Fatalf("reopen database over the VFS in TRUNCATE mode: %v", err)
	}
	defer db.Close()

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM books").Scan(&count); err != nil {
		t.Fatalf("count books over the VFS: %v", err)
	}
	if count != 1000 {
		t.Errorf("row count after a TRUNCATE-mode run = %d, want 1000", count)
	}
}

// TestRunWithClientRefusesMemoryJournalMode shows the refusal at the run's
// front door: a writer asked for _journal=MEMORY fails before any cluster
// work, with the crash-safety reason from issue #68 in the error.
func TestRunWithClientRefusesMemoryJournalMode(t *testing.T) {
	fcs := fake.NewSimpleClientset()

	err := runWithClient(fcs, Options{Retries: 1, JournalMode: "MEMORY"}, testLogger())
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
