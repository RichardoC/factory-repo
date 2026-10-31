// Package journalmode validates the --journal-mode flag shared by the
// commands that open a database through the kube-sqlite3-vfs VFS: the
// writer (the root main) and the reader (cmd/db-download, issue #318).
// The mode is carried to SQLite as the _journal DSN parameter, exactly the
// way the flag reaches the database.
package journalmode

import (
	"fmt"
	"strings"
)

// Modes the commands accept. DELETE, TRUNCATE and PERSIST all store
// the rollback journal through this VFS as sector ConfigMaps, so a crash
// mid-transaction leaves a journal the next opener can roll back. MEMORY
// and OFF do not, and WAL needs the shared-memory -shm file this VFS cannot
// store, so all three are refused.
const Modes = "DELETE, TRUNCATE, PERSIST"

// Name validates a --journal-mode value and returns the canonical
// (upper-case) name for the DSN. The empty value is the flag's default,
// DELETE, so a programmatically built Options behaves like the parsed one.
// MEMORY and OFF are refused with the crash-safety reason from issue #68:
// their journal never reaches the VFS, so a crash — an evicted pod, a
// killed process — leaves a half-written database that no opener can roll
// back.
func Name(mode string) (string, error) {
	name := strings.ToUpper(strings.TrimSpace(mode))
	switch name {
	case "":
		return "DELETE", nil
	case "DELETE", "TRUNCATE", "PERSIST":
		return name, nil
	case "MEMORY", "OFF":
		return "", fmt.Errorf("--journal-mode %s is not allowed: its rollback journal never reaches the VFS, so a crash mid-transaction leaves a half-written database that cannot be rolled back — the database is very likely corrupted (issue #68); use one of %s", strings.TrimSpace(mode), Modes)
	case "WAL":
		return "", fmt.Errorf("--journal-mode WAL is not supported: it needs SQLite's shared-memory -shm file, which this ConfigMap-backed VFS cannot store; use one of %s", Modes)
	default:
		return "", fmt.Errorf("unknown --journal-mode %q (accepted: %s)", mode, Modes)
	}
}
