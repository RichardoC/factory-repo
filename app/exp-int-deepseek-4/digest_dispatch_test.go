package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Tests for issue #279, CONFIG and DISPATCH half. Table tests covering the
// acceptance list of the task. They only exercise config defaults/errors and
// Dispatch; decide/execute are covered by the digest_decide_test.go file.

func TestDigestMinBacklogDefaultsTable(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want int
	}{
		{"absent digest section", Config{}, 2},
		{"min_backlog zero", Config{Digest: Digest{MinBacklog: 0}}, 2},
		{"min_backlog explicit 1", Config{Digest: Digest{MinBacklog: 1}}, 1},
		{"min_backlog explicit 4", Config{Digest: Digest{MinBacklog: 4}}, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := withDefaults(tc.cfg)
			if err != nil {
				t.Fatalf("withDefaults(%+v): %v", tc.cfg, err)
			}
			if cfg.Digest.MinBacklog != tc.want {
				t.Fatalf("MinBacklog = %d, want %d", cfg.Digest.MinBacklog, tc.want)
			}
		})
	}
}

func TestDigestMinBacklogFromFileTable(t *testing.T) {
	cases := []struct {
		name string
		json string
		want int
	}{
		{"digest section absent", `{}`, 2},
		{"min_backlog zero", `{"digest":{"min_backlog":0}}`, 2},
		{"min_backlog set", `{"digest":{"min_backlog":3}}`, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "jobq.json")
			if err := os.WriteFile(path, []byte(tc.json), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := loadConfig(path)
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			if cfg.Digest.MinBacklog != tc.want {
				t.Fatalf("MinBacklog = %d, want %d", cfg.Digest.MinBacklog, tc.want)
			}
		})
	}
}

func TestDigestMinBacklogNegativeIsErrorTable(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"only digest negative", Config{Digest: Digest{MinBacklog: -1}}},
		{"with other settings", Config{MaxRetries: 3, Queue: Queue{MaxParallel: 2}, Digest: Digest{MinBacklog: -5}}},
		{"negative read from file", Config{Digest: Digest{MinBacklog: -100}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := withDefaults(tc.cfg); err == nil {
				t.Fatalf("withDefaults(%+v) = nil error, want error", tc.cfg)
			}
		})
	}

	path := filepath.Join(t.TempDir(), "jobq.json")
	if err := os.WriteFile(path, []byte(`{"digest":{"min_backlog":-1}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err == nil {
		t.Fatal("loadConfig with negative min_backlog = nil error, want error")
	}
}

func TestDigestMinBacklogErrorMessage(t *testing.T) {
	_, err := withDefaults(Config{Digest: Digest{MinBacklog: -1}})
	if err == nil {
		t.Fatal("withDefaults = nil error, want error")
	}
	if got := err.Error(); got != "digest.min_backlog must not be negative" {
		t.Fatalf("error = %q, want %q", got, "digest.min_backlog must not be negative")
	}
}

func TestDispatchDigestTable(t *testing.T) {
	lowBacklog := Queue{MaxParallel: 4}
	cases := []struct {
		name      string
		cfg       Config
		st        State
		issueRuns int
		wantDig   bool
	}{
		{
			name: "slot free, low backlog, not in flight",
			cfg:  Config{MaxRetries: 3, Queue: Queue{MaxParallel: 4}, Digest: Digest{MinBacklog: 2}},
			st:   State{Queued: []int{7}},
			// one issue + digest
			issueRuns: 1, wantDig: true,
		},
		{
			name: "empty backlog starts digest",
			cfg:  Config{MaxRetries: 3, Queue: lowBacklog, Digest: Digest{MinBacklog: 2}},
			st:   State{},
			// no issues at all
			issueRuns: 0, wantDig: true,
		},
		{
			name: "backlog at MinBacklog blocks digest",
			cfg:  Config{MaxRetries: 3, Queue: Queue{MaxParallel: 4}, Digest: Digest{MinBacklog: 2}},
			st:   State{Queued: []int{7, 8}},
			// two issues queued, len(Queued) == MinBacklog
			issueRuns: 2, wantDig: false,
		},
		{
			name: "backlog above MinBacklog blocks digest",
			cfg:  Config{MaxRetries: 3, Queue: Queue{MaxParallel: 6}, Digest: Digest{MinBacklog: 2}},
			st:   State{Queued: []int{7, 8, 9}},
			// three issues queued, len(Queued) > MinBacklog
			issueRuns: 3, wantDig: false,
		},
		{
			name: "digest already in flight blocks a second",
			cfg:  Config{MaxRetries: 3, Queue: Queue{MaxParallel: 4}, Digest: Digest{MinBacklog: 2}},
			st:   State{Queued: []int{7}, DigestInFlight: true},
			// digest in flight, so only the issue run starts
			issueRuns: 1, wantDig: false,
		},
		{
			name: "no free slot after issues blocks digest",
			cfg:  Config{MaxRetries: 3, Queue: Queue{MaxParallel: 2}, Digest: Digest{MinBacklog: 3}},
			st:   State{Queued: []int{7}, InFlight: 1},
			// MaxParallel 2, InFlight 1, 1 queued issue: only the issue run
			issueRuns: 1, wantDig: false,
		},
		{
			name:      "no free slot in flight and empty backlog",
			cfg:       Config{MaxRetries: 3, Queue: Queue{MaxParallel: 2}, Digest: Digest{MinBacklog: 2}},
			st:        State{InFlight: 2},
			issueRuns: 0, wantDig: false,
		},
		{
			name:      "more issues than slots keeps runs issue only",
			cfg:       Config{MaxRetries: 3, Queue: Queue{MaxParallel: 2}, Digest: Digest{MinBacklog: 9}},
			st:        State{Queued: []int{1, 2, 3}},
			issueRuns: 2, wantDig: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Dispatch(tc.cfg, tc.st)
			digestCount := 0
			for i, in := range got {
				if in["digest"] != "" {
					digestCount++
					if i != len(got)-1 {
						t.Fatalf("digest run is not the last element: %v", got)
					}
				}
			}
			issueCount := 0
			for _, in := range got {
				if in["issue"] != "" {
					issueCount++
				}
			}
			if issueCount != tc.issueRuns {
				t.Fatalf("issue runs = %d, want %d (%v)", issueCount, tc.issueRuns, got)
			}
			if digestCount > 1 {
				t.Fatalf("more than one digest run: %v", got)
			}
			if (digestCount == 1) != tc.wantDig {
				t.Fatalf("digest started = %t, want %t (%v)", digestCount == 1, tc.wantDig, got)
			}
		})
	}
}

func TestDispatchDigestInputsExactly(t *testing.T) {
	cfg := Config{MaxRetries: 3, Queue: Queue{MaxParallel: 2}, Digest: Digest{MinBacklog: 3}}
	got := Dispatch(cfg, State{Queued: []int{4}})
	if len(got) != 2 {
		t.Fatalf("Dispatch = %v, want issue run then digest run", got)
	}
	last := got[len(got)-1]
	if last["digest"] != "true" {
		t.Fatalf("last run is not a digest run: %v", got)
	}
	if len(last) != 1 {
		t.Fatalf("digest inputs = %v, want exactly one key", last)
	}
	// The digest run consumes a slot: two runs on MaxParallel 2 fill the queue.
	if got[0]["issue"] != "4" || len(got[0]) != 1 {
		t.Fatalf("issue run = %v", got[0])
	}
}

func TestDispatchDigestConsumesSlot(t *testing.T) {
	cfg := Config{MaxRetries: 3, Queue: Queue{MaxParallel: 2}, Digest: Digest{MinBacklog: 2}}
	// One slot used by an in-flight run, one queued issue fills the other:
	// the digest would need a third slot, so it must not start.
	got := Dispatch(cfg, State{Queued: []int{1}, InFlight: 1})
	if len(got) != 1 || got[0]["issue"] != "1" {
		t.Fatalf("Dispatch = %v, want only the issue run", got)
	}
}

func TestDispatchIssuesUnchangedTable(t *testing.T) {
	cfg := Config{MaxRetries: 3, Queue: Queue{MaxParallel: 3}, Digest: Digest{MinBacklog: 0}}
	// MinBacklog 0 is normalised by withDefaults; use it raw here to show that
	// issue behaviour does not depend on the digest settings.
	cases := []struct {
		name string
		st   State
		want []string
	}{
		{"oldest first while slots last", State{Queued: []int{4, 5, 6, 7}}, []string{"4", "5", "6"}},
		{"in flight reduces slots", State{Queued: []int{4, 5, 6}, InFlight: 2}, []string{"4"}},
		{"empty queue", State{}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Dispatch(cfg, tc.st)
			if len(got) != len(tc.want) {
				t.Fatalf("Dispatch = %v, want %v", got, tc.want)
			}
			for i, n := range tc.want {
				if got[i]["issue"] != n {
					t.Fatalf("run %d = %v, want issue %s", i, got[i], n)
				}
			}
		})
	}
}
