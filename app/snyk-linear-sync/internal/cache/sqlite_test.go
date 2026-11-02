package cache

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestSnykIgnoresTTLEvictsStaleEntries(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "test-cache.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	projectID := "project-a"
	issueKey := "SNYK-1"

	// Save an entry that is already older than the TTL.
	staleUpdatedAt := time.Now().UTC().Add(-ignoreEntryTTL - time.Hour)
	if err := store.SaveIgnores(ctx, projectID, map[string]IgnoreMeta{
		issueKey: {
			IssueKey:           issueKey,
			ExpiresAt:          time.Now().UTC().Add(24 * time.Hour),
			DisregardIfFixable: false,
			CreatedAt:          time.Now().UTC().Add(-48 * time.Hour),
			UpdatedAt:          staleUpdatedAt,
		},
	}); err != nil {
		t.Fatalf("SaveIgnores() error = %v", err)
	}

	loaded, err := store.LoadIgnores(ctx, projectID)
	if err != nil {
		t.Fatalf("LoadIgnores() error = %v", err)
	}
	if _, ok := loaded[issueKey]; ok {
		t.Fatalf("stale ignore entry %q was not evicted", issueKey)
	}

	// Save a fresh entry and verify it survives.
	if err := store.SaveIgnores(ctx, projectID, map[string]IgnoreMeta{
		issueKey: {
			IssueKey:           issueKey,
			ExpiresAt:          time.Now().UTC().Add(24 * time.Hour),
			DisregardIfFixable: false,
			CreatedAt:          time.Now().UTC(),
			UpdatedAt:          time.Now().UTC(),
		},
	}); err != nil {
		t.Fatalf("SaveIgnores() fresh error = %v", err)
	}

	loaded, err = store.LoadIgnores(ctx, projectID)
	if err != nil {
		t.Fatalf("LoadIgnores() fresh error = %v", err)
	}
	if _, ok := loaded[issueKey]; !ok {
		t.Fatalf("fresh ignore entry %q was evicted", issueKey)
	}
}

// TestSaveAndLoadRoundTripsHashes pins that Load reads back exactly what Save
// wrote against a real SQLite store, so the fast-path comparison never starts
// from a wrong view of the cache (the stub-based Load tests cannot see this).
func TestSaveAndLoadRoundTripsHashes(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "test-cache.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	snapshot := Snapshot{
		SchemaSignature: "test-sig-v1",
		SnykHashes: map[string]string{
			"snyk:project-a:issue-1": "hash-a",
			"snyk:project-a:issue-2": "hash-b",
		},
		LinearHashes: map[string]string{
			"snyk:project-a:issue-1": "linear-a",
		},
	}
	if err := store.Save(ctx, snapshot); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	loaded, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.SchemaSignature != "test-sig-v1" {
		t.Fatalf("SchemaSignature = %q, want %q", loaded.SchemaSignature, "test-sig-v1")
	}
	if len(loaded.SnykHashes) != len(snapshot.SnykHashes) {
		t.Fatalf("SnykHashes = %v, want %v", loaded.SnykHashes, snapshot.SnykHashes)
	}
	for fingerprint, hash := range snapshot.SnykHashes {
		if got := loaded.SnykHashes[fingerprint]; got != hash {
			t.Fatalf("SnykHashes[%q] = %q, want %q", fingerprint, got, hash)
		}
	}
	if len(loaded.LinearHashes) != len(snapshot.LinearHashes) {
		t.Fatalf("LinearHashes = %v, want %v", loaded.LinearHashes, snapshot.LinearHashes)
	}
	for fingerprint, hash := range snapshot.LinearHashes {
		if got := loaded.LinearHashes[fingerprint]; got != hash {
			t.Fatalf("LinearHashes[%q] = %q, want %q", fingerprint, got, hash)
		}
	}
}
