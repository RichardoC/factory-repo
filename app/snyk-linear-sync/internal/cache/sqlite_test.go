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
