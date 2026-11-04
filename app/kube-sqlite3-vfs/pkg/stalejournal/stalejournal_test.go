// Unit tests for DeleteIfPresent's hotness classification: each branch of
// the switch (hot, cold, hotness unknown) and the two error paths. The
// behaviour tests in cmd/db-download and cmd/db-upload drive the unknown
// path end to end against the fake clientset; these tests pin the
// classification itself, including the cold branch those tests never reach
// with a readable journal (review finding on the issue #509 PR).
package stalejournal

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/RichardoC/kube-sqlite3-vfs/internal/logcapture"
	"github.com/psanford/sqlite3vfs"
	// Pulls in the SQLite amalgamation the psanford/sqlite3vfs cgo shim
	// links against (the same blank import pkg/vfs's tests carry).
	_ "github.com/mattn/go-sqlite3"
)

// fakeVFS answers Access and Delete from fields, and records deletions.
type fakeVFS struct {
	exists    bool
	accessErr error
	deleteErr error
	deleted   int
}

func (f *fakeVFS) Access(string, sqlite3vfs.AccessFlag) (bool, error) {
	return f.exists, f.accessErr
}

func (f *fakeVFS) Delete(string, bool) error {
	f.deleted++
	return f.deleteErr
}

// fakeJournal answers ReadAt from fields: a non-nil err is returned
// verbatim with n bytes read, otherwise data is copied out.
type fakeJournal struct {
	data []byte
	n    int
	err  error
}

func (f *fakeJournal) ReadAt(p []byte, _ int64) (int, error) {
	if f.err != nil {
		return f.n, f.err
	}
	return copy(p, f.data), nil
}

// journalMagic is SQLite's rollback-journal magic, the non-zero first bytes
// of a hot journal's header.
var journalMagic = []byte{0xd9, 0xd5, 0x05, 0xf9, 0x20, 0xa1, 0x63, 0xd7}

func TestDeleteIfPresentNoJournal(t *testing.T) {
	fv := &fakeVFS{}
	logger, captured := logcapture.NewCapture()
	if err := DeleteIfPresent(fv, &fakeJournal{}, "f.db-journal", "download", logger); err != nil {
		t.Fatalf("DeleteIfPresent with no journal present: %v", err)
	}
	if fv.deleted != 0 {
		t.Errorf("Delete called %d times, want 0", fv.deleted)
	}
	if text := captured.Text(); text != "(no log records captured)" {
		t.Errorf("logged without a journal present:\n%s", text)
	}
}

func TestDeleteIfPresentHotJournal(t *testing.T) {
	fv := &fakeVFS{exists: true}
	logger, captured := logcapture.NewCapture()
	if err := DeleteIfPresent(fv, &fakeJournal{data: journalMagic}, "f.db-journal", "download", logger); err != nil {
		t.Fatalf("DeleteIfPresent over a hot journal: %v", err)
	}
	if fv.deleted != 1 {
		t.Fatalf("Delete called %d times, want 1", fv.deleted)
	}
	rec, ok := captured.Find("Deleted a HOT rollback journal")
	if !ok {
		t.Fatalf("no hot-journal record; log:\n%s", captured.Text())
	}
	if rec.Level != slog.LevelWarn {
		t.Errorf("hot journal logged at %s, want warn", rec.Level)
	}
}

// The cold branch: a readable, zeroed header classifies the journal as
// cold and logs it at info — the classification an unknown-hotness warning
// must never be allowed to imitate.
func TestDeleteIfPresentColdJournal(t *testing.T) {
	fv := &fakeVFS{exists: true}
	logger, captured := logcapture.NewCapture()
	if err := DeleteIfPresent(fv, &fakeJournal{data: make([]byte, 8)}, "f.db-journal", "upload", logger); err != nil {
		t.Fatalf("DeleteIfPresent over a cold journal: %v", err)
	}
	if fv.deleted != 1 {
		t.Fatalf("Delete called %d times, want 1", fv.deleted)
	}
	rec, ok := captured.Find("Deleted a stale (cold) rollback journal")
	if !ok {
		t.Fatalf("no cold-journal record; log:\n%s", captured.Text())
	}
	if rec.Level != slog.LevelInfo {
		t.Errorf("cold journal logged at %s, want info", rec.Level)
	}
}

// A read that ends at EOF before any byte arrives is still a usable probe:
// SQLite treats a missing header as a cold journal.
func TestDeleteIfPresentColdJournalAtEOF(t *testing.T) {
	fv := &fakeVFS{exists: true}
	logger, captured := logcapture.NewCapture()
	if err := DeleteIfPresent(fv, &fakeJournal{n: 0, err: io.EOF}, "f.db-journal", "upload", logger); err != nil {
		t.Fatalf("DeleteIfPresent over a journal that reads as EOF: %v", err)
	}
	if fv.deleted != 1 {
		t.Fatalf("Delete called %d times, want 1", fv.deleted)
	}
	if _, ok := captured.Find("Deleted a stale (cold) rollback journal"); !ok {
		t.Fatalf("no cold-journal record; log:\n%s", captured.Text())
	}
}

// The unknown branch: a probe error that is neither nil nor io.EOF leaves
// the hotness unestablished — the warning must carry the read error, and no
// record may claim a hot or cold journal (issue #509).
func TestDeleteIfPresentProbeReadErrorLogsUnknownHotness(t *testing.T) {
	fv := &fakeVFS{exists: true}
	probeErr := sqlite3vfs.IOErrorRead
	logger, captured := logcapture.NewCapture()
	if err := DeleteIfPresent(fv, &fakeJournal{n: 0, err: probeErr}, "f.db-journal", "download", logger); err != nil {
		t.Fatalf("DeleteIfPresent with a failing probe: %v", err)
	}
	if fv.deleted != 1 {
		t.Fatalf("Delete called %d times, want 1: an unreadable journal must still be deleted", fv.deleted)
	}
	rec, ok := captured.Find("hotness unknown")
	if !ok {
		t.Fatalf("no \"hotness unknown\" record; log:\n%s", captured.Text())
	}
	if rec.Level != slog.LevelWarn {
		t.Errorf("\"hotness unknown\" logged at %s, want warn", rec.Level)
	}
	if got := rec.Attr("error"); !strings.Contains(got, probeErr.Error()) {
		t.Errorf("warning carries error %q, want it to contain %q", got, probeErr.Error())
	}
	if _, ok := captured.Find("cold"); ok {
		t.Errorf("log claims a cold journal although the probe could not read it; log:\n%s", captured.Text())
	}
	if _, ok := captured.Find("HOT rollback journal"); ok {
		t.Errorf("log claims a hot journal although the probe could not read it; log:\n%s", captured.Text())
	}
}

func TestDeleteIfPresentAccessError(t *testing.T) {
	fv := &fakeVFS{accessErr: errors.New("cluster unreachable")}
	logger, captured := logcapture.NewCapture()
	err := DeleteIfPresent(fv, &fakeJournal{}, "f.db-journal", "download", logger)
	if err == nil || !strings.Contains(err.Error(), "look for a stale journal") {
		t.Fatalf("DeleteIfPresent with a failing Access = %v, want a look-for error", err)
	}
	if fv.deleted != 0 {
		t.Errorf("Delete called %d times, want 0", fv.deleted)
	}
	if text := captured.Text(); text != "(no log records captured)" {
		t.Errorf("logged on the Access error path:\n%s", text)
	}
}

func TestDeleteIfPresentDeleteError(t *testing.T) {
	fv := &fakeVFS{exists: true, deleteErr: errors.New("delete rejected")}
	logger, captured := logcapture.NewCapture()
	err := DeleteIfPresent(fv, &fakeJournal{data: journalMagic}, "f.db-journal", "download", logger)
	if err == nil || !strings.Contains(err.Error(), "delete stale journal") {
		t.Fatalf("DeleteIfPresent with a failing Delete = %v, want a delete error", err)
	}
	if text := captured.Text(); text != "(no log records captured)" {
		t.Errorf("logged a deletion that failed:\n%s", text)
	}
}
