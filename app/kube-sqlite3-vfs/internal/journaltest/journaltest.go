// Package journaltest holds helpers shared by the behaviour tests of the
// db-download and db-upload commands: HeaderSectorName, which reports the
// journal's sector-0 ConfigMap name. No test package can build that name
// directly (the VFS's name helper is unexported), so the helper lists the
// journal's sectors and takes the lowest index.
//
// The log-capture handler these tests assert with lives in
// internal/logcapture.
package journaltest

import (
	"context"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"

	"github.com/RichardoC/kube-sqlite3-vfs/pkg/vfs"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// HeaderSectorName returns the name of the journal's sector-0 ConfigMap,
// the one the hotness probe GETs. The VFS's name helper is unexported, so
// the helper lists the journal's sector ConfigMaps and takes the lowest
// index.
func HeaderSectorName(t *testing.T, fcs *fake.Clientset) string {
	t.Helper()
	vfsN := vfs.NewVFS(context.Background(), fcs, "test", slog.New(slog.NewTextHandler(io.Discard, nil)), 1)
	jf := vfs.NewFile("file2.db-journal", vfsN)
	cms, err := fcs.CoreV1().ConfigMaps("test").List(context.Background(), metav1.ListOptions{LabelSelector: "data=sector,relevant-file=" + jf.SectorLabels["relevant-file"]})
	if err != nil {
		t.Fatalf("list journal sector configmaps: %v", err)
	}
	name := ""
	var lowest int64
	for _, cm := range cms.Items {
		idx, ok := sectorIndexFromName(cm.Name)
		if !ok {
			continue
		}
		if name == "" || idx < lowest {
			name, lowest = cm.Name, idx
		}
	}
	if name == "" {
		t.Fatal("setup: the journal has no sector ConfigMaps")
	}
	return name
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
