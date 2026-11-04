// Package stalejournal removes the rollback journal a crashed writer left
// beside a database stored through the kube-sqlite3-vfs VFS. The db-upload
// and db-download commands both delete such a journal before they replace
// the database; sharing this code keeps their messages and their hotness
// classification identical (issue #509).
package stalejournal

import (
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/psanford/sqlite3vfs"
)

// VFS is the slice of the kube-sqlite3-vfs VFS this cleanup needs.
type VFS interface {
	// Access answers whether journalName exists through the VFS.
	Access(name string, flags sqlite3vfs.AccessFlag) (bool, error)
	// Delete removes journalName through the VFS xDelete, which removes
	// the journal header sector first.
	Delete(name string, syncDir bool) error
}

// Journal is the handle the hotness probe reads the journal magic from.
// *vfs.File satisfies it.
type Journal interface {
	ReadAt(p []byte, off int64) (int, error)
}

// DeleteIfPresent deletes journalName when the cluster still holds it, and
// logs the deletion with the hotness the probe established:
//
//   - the header read and is non-zero: a HOT journal (warn);
//   - the header read and is zero, or the read ended at EOF: a cold
//     journal (info);
//   - the header could not be read at all: hotness UNKNOWN (warn, carrying
//     the read error). The journal is still deleted, because the command
//     replaces the database outright, but the log must not claim a cold
//     journal it never verified (issue #509).
//
// action names the command for the log messages, e.g. "download".
func DeleteIfPresent(v VFS, journal Journal, journalName, action string, logger *slog.Logger) error {
	exists, err := v.Access(journalName, sqlite3vfs.AccessExists)
	if err != nil {
		return fmt.Errorf("look for a stale journal %q: %w", journalName, err)
	}
	if !exists {
		return nil
	}

	hot, probeErr := probeHot(journal)
	if err := v.Delete(journalName, false); err != nil {
		return fmt.Errorf("delete stale journal %q: %w", journalName, err)
	}

	switch {
	case probeErr != nil:
		// The deletion is safe (the caller overwrites the database), but
		// the audit trail must say what it knows: the probe failed, so
		// neither a hot nor a cold journal was established.
		logger.Warn("Deleted a stale rollback journal, hotness unknown: the probe read failed, so a HOT journal can be neither confirmed nor ruled out; the "+action+" replaces the database outright, so the journal was removed rather than replayed", "journal", journalName, "error", probeErr)
	case hot:
		logger.Warn("Deleted a HOT rollback journal left by a crashed writer: the "+action+" replaces the database outright, so the journal was removed rather than replayed", "journal", journalName)
	default:
		logger.Info("Deleted a stale (cold) rollback journal", "journal", journalName)
	}
	return nil
}

// probeHot reports whether the journal's first bytes (the journal magic)
// are non-zero. A read that ends at EOF is still usable: SQLite treats a
// missing or zeroed header as a cold journal. Any other read error means
// the hotness is unknown, and is returned to the caller rather than
// swallowed.
func probeHot(journal Journal) (bool, error) {
	var head [8]byte
	n, err := journal.ReadAt(head[:], 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	for _, b := range head[:n] {
		if b != 0 {
			return true, nil
		}
	}
	return false, nil
}
