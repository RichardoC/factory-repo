package vfs

import (
	"fmt"
	"testing"
)

func TestZZProbeJournalContents(t *testing.T) {
	v1, v2 := newContendingVFSInstances(t)
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

	jname := fileName + "-journal"
	cms, err := NewFile(jname, v2).listSectorConfigMaps()
	if err != nil {
		t.Fatalf("list journal sectors: %v", err)
	}
	fmt.Printf("JOURNAL SECTORS: %d\n", len(cms))
	for _, cm := range cms {
		fmt.Printf("  %s: %d bytes\n", cm.Name, len(cm.BinaryData["sector"]))
	}
	ok, err := journalVisibleVFS{v2}.Access(jname, 1)
	fmt.Printf("WRAPPER ACCESS(journal) = %v, %v\n", ok, err)

	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
}
