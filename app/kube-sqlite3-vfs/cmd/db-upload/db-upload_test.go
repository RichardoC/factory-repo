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
	lists := 0
	fcs.PrependReactor("list", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
		lists++
		if lists != 3 {
			return false, nil, nil
		}
		listAction, ok := action.(clienttesting.ListAction)
		if !ok {
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
