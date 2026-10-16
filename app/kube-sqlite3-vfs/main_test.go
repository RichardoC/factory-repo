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
	// over the same VFS: 1000 rows on the books table.
	db, err := sql.Open("sqlite3", "file2.db?_query_only=true&_journal=MEMORY&mode=ro&sqlite_os_trace=&vfs=kube-sqlite3-vfs")
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
