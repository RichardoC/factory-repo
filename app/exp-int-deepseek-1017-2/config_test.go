package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A zero (unset) digest.min_backlog defaults to 2; explicit values are kept.
func TestDigestMinBacklogDefaults(t *testing.T) {
	cfg, err := withDefaults(Config{})
	if err != nil {
		t.Fatalf("withDefaults: %v", err)
	}
	if cfg.Digest.MinBacklog != 2 {
		t.Fatalf("Digest.MinBacklog = %d, want default 2", cfg.Digest.MinBacklog)
	}
	for _, want := range []int{1, 5} {
		cfg, err := withDefaults(Config{Digest: Digest{MinBacklog: want}})
		if err != nil {
			t.Fatalf("withDefaults(%d): %v", want, err)
		}
		if cfg.Digest.MinBacklog != want {
			t.Fatalf("Digest.MinBacklog = %d, want %d kept", cfg.Digest.MinBacklog, want)
		}
	}
}

// A negative digest.min_backlog is an error from withDefaults.
func TestDigestMinBacklogNegativeWithDefaults(t *testing.T) {
	for _, bad := range []int{-1, -3} {
		if _, err := withDefaults(Config{Digest: Digest{MinBacklog: bad}}); err == nil {
			t.Fatalf("MinBacklog=%d accepted", bad)
		} else if !strings.Contains(err.Error(), "digest.min_backlog must not be negative") {
			t.Fatalf("MinBacklog=%d error = %v", bad, err)
		}
	}
}

// loadConfig applies the default and rejects a negative value read from a file.
func TestLoadConfigDigest(t *testing.T) {
	write := func(body string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "jobq.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	cfg, err := loadConfig(write(`{}`))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Digest.MinBacklog != 2 {
		t.Fatalf("Digest.MinBacklog = %d, want 2", cfg.Digest.MinBacklog)
	}
	// Existing settings are unchanged next to the new section.
	if cfg.MaxRetries != 3 || cfg.Queue.MaxParallel != 2 {
		t.Fatalf("existing defaults changed: %+v", cfg)
	}

	cfg, err = loadConfig(write(`{"digest":{"min_backlog":4}}`))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Digest.MinBacklog != 4 {
		t.Fatalf("Digest.MinBacklog = %d, want 4", cfg.Digest.MinBacklog)
	}

	if _, err := loadConfig(write(`{"digest":{"min_backlog":-2}}`)); err == nil {
		t.Fatal("loadConfig accepted a negative digest.min_backlog")
	} else if !strings.Contains(err.Error(), "digest.min_backlog must not be negative") {
		t.Fatalf("loadConfig error = %v", err)
	}
}
