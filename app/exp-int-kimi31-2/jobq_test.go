package main

import (
	"bytes"
	"os"
	"strings"
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

func TestDigestDefaultsAndValidation(t *testing.T) {
	cfg, err := withDefaults(Config{})
	if err != nil || cfg.Digest.MinBacklog != 2 {
		t.Fatalf("default = %+v, %v", cfg.Digest, err)
	}
	cfg, err = withDefaults(Config{Digest: Digest{MinBacklog: 4}})
	if err != nil || cfg.Digest.MinBacklog != 4 {
		t.Fatalf("configured = %+v, %v", cfg.Digest, err)
	}
	if _, err := withDefaults(Config{Digest: Digest{MinBacklog: -1}}); err == nil {
		t.Fatal("negative min_backlog was accepted")
	}
}

func TestDispatchDigest(t *testing.T) {
	cfg, _ := withDefaults(Config{Queue: Queue{MaxParallel: 3}})
	tests := []struct {
		name string
		st   State
		want []map[string]string
	}{
		{"low backlog", State{Queued: []int{4}, InFlight: 1}, []map[string]string{{"issue": "4"}, {"digest": "true"}}},
		{"backlog at minimum", State{Queued: []int{4, 5}}, []map[string]string{{"issue": "4"}, {"issue": "5"}}},
		{"no free slot", State{InFlight: 3}, nil},
		{"already in flight", State{DigestInFlight: true}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Dispatch(cfg, tt.st)
			if len(got) != len(tt.want) {
				t.Fatalf("Dispatch = %v, want %v", got, tt.want)
			}
			for i := range got {
				for key, value := range tt.want[i] {
					if got[i][key] != value || len(got[i]) != len(tt.want[i]) {
						t.Fatalf("Dispatch = %v, want %v", got, tt.want)
					}
				}
			}
		})
	}
}

func TestDecideDigest(t *testing.T) {
	d := Decide(Event{Inputs: map[string]string{"digest": "true"}})
	if !d.Run || d.Mode != "digest" || d.Number != 0 {
		t.Fatalf("Decide = %+v", d)
	}
	for _, inputs := range []map[string]string{
		{"digest": "false"},
		{"digest": "TRUE"},
		{"digest": "true", "issue": "7"},
		{"digest": "true", "review": "8"},
	} {
		if d := Decide(Event{Inputs: inputs}); d.Run {
			t.Fatalf("invalid inputs ran: %v: %+v", inputs, d)
		}
	}
}

func TestExecuteDigest(t *testing.T) {
	if got, err := Execute(Decision{Run: true, Mode: "digest"}); err != nil || got != "wrote digest" {
		t.Fatalf("Execute = %q, %v", got, err)
	}
}

func TestDecideCommandDigestOutput(t *testing.T) {
	path := t.TempDir() + "/event.json"
	if err := os.WriteFile(path, []byte(`{"inputs":{"digest":"true"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run([]string{"decide", path}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "mode=digest\n") || strings.Contains(out.String(), "number=") {
		t.Fatalf("output = %q", out.String())
	}
}
