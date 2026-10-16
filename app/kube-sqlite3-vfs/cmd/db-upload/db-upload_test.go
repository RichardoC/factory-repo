// Behaviour tests for the db-upload command against the fake kubernetes
// clientset: the sector-by-sector upload's stored bytes, and the failure
// paths (missing local file, failed sector write) that must return an
// error instead of exiting zero with a partial upload.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/RichardoC/kube-sqlite3-vfs/pkg/vfs"
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
