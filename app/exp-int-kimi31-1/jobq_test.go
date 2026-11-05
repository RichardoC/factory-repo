package main

import (
	"os"
	"path/filepath"
	"reflect"
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

func TestDigestConfigDefaultsAndValidation(t *testing.T) {
	for _, cfg := range []Config{{}, {Digest: Digest{MinBacklog: 0}}} {
		got, err := withDefaults(cfg)
		if err != nil || got.Digest.MinBacklog != 2 {
			t.Fatalf("withDefaults(%+v) = %+v, %v", cfg, got, err)
		}
	}
	if _, err := withDefaults(Config{Digest: Digest{MinBacklog: -1}}); err == nil {
		t.Fatal("negative digest.min_backlog was accepted")
	}

	path := filepath.Join(t.TempDir(), "jobq.json")
	if err := os.WriteFile(path, []byte(`{"digest":{"min_backlog":-1}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err == nil {
		t.Fatal("loadConfig accepted negative digest.min_backlog")
	}
}

func TestDispatchDigestUsesOnlySpareSlotBelowBacklog(t *testing.T) {
	cfg, _ := withDefaults(Config{Queue: Queue{MaxParallel: 3}, Digest: Digest{MinBacklog: 2}})
	got := Dispatch(cfg, State{Queued: []int{4}, InFlight: 1})
	want := []map[string]string{{"issue": "4"}, {"digest": "true"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Dispatch = %v, want %v", got, want)
	}
}

func TestDispatchDigestConditions(t *testing.T) {
	cfg, _ := withDefaults(Config{Queue: Queue{MaxParallel: 2}, Digest: Digest{MinBacklog: 2}})
	tests := []struct {
		name string
		cfg  Config
		st   State
	}{
		{name: "no free slot after queued issues", st: State{Queued: []int{4, 5}}},
		{name: "backlog at minimum", st: State{Queued: []int{4, 5}, InFlight: 1}},
		{name: "digest already in flight", st: State{DigestInFlight: true}},
		{name: "no free slot before issues", st: State{InFlight: 2}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, inputs := range Dispatch(tt.cfg, tt.st) {
				if _, ok := inputs["digest"]; ok {
					t.Fatalf("unexpected digest dispatch: %v", inputs)
				}
			}
		})
	}

	cfg.Digest.MinBacklog = 1
	got := Dispatch(cfg, State{})
	if len(got) != 1 || !reflect.DeepEqual(got[0], map[string]string{"digest": "true"}) {
		t.Fatalf("zero backlog with threshold one = %v", got)
	}
}

func TestDecideDigest(t *testing.T) {
	d := Decide(Event{Inputs: map[string]string{"digest": "true"}})
	if !d.Run || d.Mode != "digest" || d.Number != 0 {
		t.Fatalf("Decide = %+v", d)
	}
	for _, inputs := range []map[string]string{
		{"digest": "True"},
		{"digest": "false"},
		{"digest": "true", "issue": "7"},
		{"digest": "true", "review": "8"},
	} {
		if d := Decide(Event{Inputs: inputs}); d.Run {
			t.Fatalf("invalid digest inputs ran: %v => %+v", inputs, d)
		}
	}
}

func TestExecuteDigest(t *testing.T) {
	if got, err := Execute(Decision{Run: true, Mode: "digest"}); err != nil || got != "wrote digest" {
		t.Fatalf("Execute = %q, %v", got, err)
	}
}

func TestRunDecideDigestOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "event.json")
	if err := os.WriteFile(path, []byte(`{"inputs":{"digest":"true"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := run([]string{"decide", path}, &out); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "run=true\nreason=digest dispatch\nmode=digest\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}
