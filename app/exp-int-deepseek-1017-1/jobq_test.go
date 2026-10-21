package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDispatchFillsFreeSlots(t *testing.T) {
	cfg, _ := withDefaults(Config{})
	got := Dispatch(cfg, State{Queued: []int{4, 5, 6}, InFlight: 1})
	if len(got) != 1 || got[0]["issue"] != "4" {
		t.Fatalf("Dispatch = %v", got)
	}
}

func TestDecideIssue(t *testing.T) {
	d := Decide(Event{Inputs: map[string]string{"issue": "7"}})
	if !d.Run || d.Mode != "issue" || d.Number != 7 {
		t.Fatalf("Decide = %+v", d)
	}
	if d := Decide(Event{Inputs: map[string]string{"issue": "7", "review": "8"}}); d.Run {
		t.Fatalf("both inputs ran: %+v", d)
	}
}

func TestExecute(t *testing.T) {
	if got, err := Execute(Decision{Run: true, Mode: "review", Number: 3}); err != nil || got != "reviewed PR #3" {
		t.Fatalf("Execute = %q, %v", got, err)
	}
}

func mustWithDefaults(t *testing.T, cfg Config) Config {
	t.Helper()
	got, err := withDefaults(cfg)
	if err != nil {
		t.Fatalf("withDefaults(%+v): %v", cfg, err)
	}
	return got
}

// TestDispatchDigest is table-driven and covers when the dispatcher starts the
// at most one digest run and when it must not.
func TestDispatchDigest(t *testing.T) {
	tests := []struct {
		name  string
		cfg   Config
		state State
		want  []map[string]string
	}{
		{
			name:  "digest started when free slot and short backlog",
			cfg:   Config{Queue: Queue{MaxParallel: 3}, Digest: Digest{MinBacklog: 2}},
			state: State{Queued: []int{7}},
			want:  []map[string]string{{"issue": "7"}, {"digest": "true"}},
		},
		{
			name:  "digest started on an empty queue",
			cfg:   Config{Queue: Queue{MaxParallel: 2}},
			state: State{},
			want:  []map[string]string{{"digest": "true"}},
		},
		{
			name:  "no digest when backlog above MinBacklog",
			cfg:   Config{Queue: Queue{MaxParallel: 5}, Digest: Digest{MinBacklog: 2}},
			state: State{Queued: []int{1, 2, 3}},
			want:  []map[string]string{{"issue": "1"}, {"issue": "2"}, {"issue": "3"}},
		},
		{
			name:  "no digest when backlog equals MinBacklog",
			cfg:   Config{Queue: Queue{MaxParallel: 5}, Digest: Digest{MinBacklog: 2}},
			state: State{Queued: []int{1, 2}},
			want:  []map[string]string{{"issue": "1"}, {"issue": "2"}},
		},
		{
			name:  "no digest when no free slot after issue dispatches",
			cfg:   Config{Queue: Queue{MaxParallel: 3}, Digest: Digest{MinBacklog: 3}},
			state: State{Queued: []int{4}, InFlight: 2},
			want:  []map[string]string{{"issue": "4"}},
		},
		{
			name:  "no digest when all slots are in flight",
			cfg:   Config{Queue: Queue{MaxParallel: 2}},
			state: State{InFlight: 2},
			want:  nil,
		},
		{
			name:  "no digest when a digest is already in flight",
			cfg:   Config{Queue: Queue{MaxParallel: 3}, Digest: Digest{MinBacklog: 2}},
			state: State{Queued: []int{5}, DigestInFlight: true},
			want:  []map[string]string{{"issue": "5"}},
		},
		{
			name:  "digest started alongside issue dispatches, issues first, digest last",
			cfg:   Config{Queue: Queue{MaxParallel: 4}, Digest: Digest{MinBacklog: 4}},
			state: State{Queued: []int{10, 11, 12}},
			want: []map[string]string{
				{"issue": "10"}, {"issue": "11"}, {"issue": "12"}, {"digest": "true"},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Dispatch(mustWithDefaults(t, tc.cfg), tc.state)
			if len(got) != len(tc.want) {
				t.Fatalf("Dispatch = %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if len(got[i]) != len(tc.want[i]) {
					t.Fatalf("Dispatch[%d] = %v, want %v", i, got[i], tc.want[i])
				}
				for k, v := range tc.want[i] {
					if got[i][k] != v {
						t.Fatalf("Dispatch[%d] = %v, want %v", i, got[i], tc.want[i])
					}
				}
			}
		})
	}
}

func TestDigestMinBacklogDefault(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{name: "unset", cfg: Config{}},
		{name: "zero", cfg: Config{Digest: Digest{MinBacklog: 0}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := mustWithDefaults(t, tc.cfg).Digest.MinBacklog; got != 2 {
				t.Fatalf("MinBacklog = %d, want default 2", got)
			}
		})
	}
	if got := mustWithDefaults(t, Config{Digest: Digest{MinBacklog: 5}}).Digest.MinBacklog; got != 5 {
		t.Fatalf("MinBacklog = %d, want 5 kept", got)
	}
}

func TestDigestMinBacklogNegative(t *testing.T) {
	const want = "digest.min_backlog must not be negative"
	if _, err := withDefaults(Config{Digest: Digest{MinBacklog: -1}}); err == nil || err.Error() != want {
		t.Fatalf("withDefaults error = %v, want %q", err, want)
	}
	path := filepath.Join(t.TempDir(), "jobq.json")
	if err := os.WriteFile(path, []byte(`{"digest":{"min_backlog":-1}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err == nil || err.Error() != want {
		t.Fatalf("loadConfig error = %v, want %q", err, want)
	}
}

func TestLoadConfigDigest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobq.json")
	if err := os.WriteFile(path, []byte(`{"queue":{"max_parallel":4},"digest":{"min_backlog":3}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Digest.MinBacklog != 3 || cfg.Queue.MaxParallel != 4 {
		t.Fatalf("loadConfig = %+v", cfg)
	}
	// Unset digest section takes the default.
	path2 := filepath.Join(t.TempDir(), "jobq.json")
	if err := os.WriteFile(path2, []byte(`{"queue":{"max_parallel":4}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg2, err := loadConfig(path2); err != nil || cfg2.Digest.MinBacklog != 2 {
		t.Fatalf("loadConfig without digest = %+v, %v", cfg2, err)
	}
}
