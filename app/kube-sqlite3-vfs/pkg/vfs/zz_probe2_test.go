package vfs

import (
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"
)

func TestZZProbeBugWorldBRead(t *testing.T) {
	fcs := newArbitratingFakeClientset(t)
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
	lease := 15 * time.Second
	v1 := NewVFS(t.Context(), fcs, testNamespace, logger, 3, WithLockLease(lease))
	v2 := NewVFS(t.Context(), fcs, testNamespace, logger, 3, WithLockLease(lease))
	vfs1Name := registerUniqueVFS(t, v1)
	vfs2Name := registerUniqueVFS(t, journalVisibleVFS{v2})
	fileName := newSQLFileName()

	dbA := openRollbackJournalSQLDB(t, vfs1Name, fileName)
	defer dbA.Close()
	dbB := openRollbackJournalSQLDB(t, vfs2Name, fileName)
	defer dbB.Close()

	if _, err := dbA.Exec(`CREATE TABLE books (id text NOT NULL PRIMARY KEY, title text)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	const seededRows = 10
	insertBatchWithBusyRetry(t, dbA, "a", seededRows)
	if t.Failed() {
		return
	}

	tx, err := dbA.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	stmt, err := tx.Prepare("INSERT INTO books (id, title) VALUES (?, ?)")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	for i := seededRows; i < seededRows+5; i++ {
		if _, err := stmt.Exec(fmt.Sprintf("a-%03d", i), fmt.Sprintf("title a %d", i)); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	stmt.Close()

	fmt.Printf("=== B FIRST READ STARTS (bug world) ===\n")
	var n int
	err = dbB.QueryRow("SELECT COUNT(*) FROM books").Scan(&n)
	fmt.Printf("=== B READ RESULT: n=%d err=%v ===\n", n, err)
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
}
