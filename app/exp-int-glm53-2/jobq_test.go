package main

import (
	"encoding/json"
	"os"
	"path/filepath"
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

func TestDigestConfigDefaults(t *testing.T) {
	cfg, err := withDefaults(Config{})
	if err != nil || cfg.Digest.MinBacklog != 2 {
		t.Fatalf("default MinBacklog = %d, err = %v; want 2, nil", cfg.Digest.MinBacklog, err)
	}
}

func TestDigestConfigNegative(t *testing.T) {
	if _, err := withDefaults(Config{Digest: Digest{MinBacklog: -1}}); err == nil {
		t.Fatal("negative min_backlog accepted")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	if err := os.WriteFile(path, []byte(`{"digest": {"min_backlog": -5}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err == nil {
		t.Fatal("negative min_backlog from file accepted")
	}
}

func TestDispatchDigestOnlyInSpareCapacity(t *testing.T) {
	cfg, _ := withDefaults(Config{}) // MaxParallel 2, MinBacklog 2
	tests := []struct {
		name string
		st   State
		want bool // last output is a digest run
	}{
		{"free slot, low backlog", State{Queued: []int{4}, InFlight: 0}, true},
		{"no free slot", State{Queued: []int{4, 5}, InFlight: 0}, false},
		{"backlog at minimum", State{Queued: []int{4, 5}, InFlight: 2}, false},
		{"already digesting", State{Queued: []int{4}, InFlight: 0, DigestInFlight: true}, false},
		{"nothing queued", State{}, true},
	}
	for _, tt := range tests {
		got := Dispatch(cfg, tt.st)
		has := len(got) > 0 && got[len(got)-1]["digest"] == "true"
		if has != tt.want {
			t.Fatalf("%s: Dispatch = %v; digest=%t want %t", tt.name, got, has, tt.want)
		}
	}
}

func TestDispatchDigestCountsIssuesFirst(t *testing.T) {
	// One free slot after the queue is drained: the digest run takes it.
	cfg, _ := withDefaults(Config{}) // MaxParallel 2, MinBacklog 2
	got := Dispatch(cfg, State{Queued: []int{4}, InFlight: 0})
	if len(got) != 2 || got[0]["issue"] != "4" || got[1]["digest"] != "true" {
		t.Fatalf("Dispatch = %v", got)
	}
}

func TestDispatchOneDigestPerTick(t *testing.T) {
	cfg, _ := withDefaults(Config{})
	cfg.Queue.MaxParallel = 4
	got := Dispatch(cfg, State{})
	n := 0
	for _, in := range got {
		if in["digest"] == "true" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("Dispatch = %v; %d digest runs, want 1", got, n)
	}
}

func TestDispatchDigestInputsExactlyTrue(t *testing.T) {
	cfg, _ := withDefaults(Config{})
	got := Dispatch(cfg, State{})
	if len(got) != 1 || len(got[0]) != 1 || got[0]["digest"] != "true" {
		t.Fatalf("Dispatch = %v; want exactly one digest input {digest:true}", got)
	}
}

func TestDecideDigest(t *testing.T) {
	d := Decide(Event{Inputs: map[string]string{"digest": "true"}})
	if !d.Run || d.Mode != "digest" || d.Reason != "digest dispatch" {
		t.Fatalf("Decide = %+v", d)
	}
	for _, in := range []map[string]string{
		{"digest": "false"},
		{"digest": "yes"},
		{"digest": "TRUE"},
		{"digest": "true", "issue": "7"},
		{"digest": "true", "review": "8"},
		{"digest": "true", "issue": "7", "review": "8"},
	} {
		if d := Decide(Event{Inputs: in}); d.Run {
			t.Fatalf("Decide(%v) ran: %+v", in, d)
		}
	}
}

func TestDecideDigestReasons(t *testing.T) {
	if got := Decide(Event{Inputs: map[string]string{"digest": "false"}}).Reason; got != "digest must be true" {
		t.Fatalf("reason = %q", got)
	}
	if got := Decide(Event{Inputs: map[string]string{"digest": "true", "issue": "1"}}).Reason; got != "digest is exclusive with issue and review" {
		t.Fatalf("reason = %q", got)
	}
}

func TestDecideIssueAndReviewUnchanged(t *testing.T) {
	if got, err := Execute(Decision{Run: true, Mode: "issue", Number: 9}); err != nil || got != "worked issue #9" {
		t.Fatalf("Execute issue = %q, %v", got, err)
	}
	d := Decide(Event{Inputs: map[string]string{"review": "12"}})
	if !d.Run || d.Mode != "review" || d.Number != 12 {
		t.Fatalf("Decide review = %+v", d)
	}
}

func TestExecuteDigest(t *testing.T) {
	if got, err := Execute(Decision{Run: true, Mode: "digest"}); err != nil || got != "wrote digest" {
		t.Fatalf("Execute digest = %q, %v", got, err)
	}
}

func TestDecideCommandDigestHasNoNumber(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ev.json")
	if err := os.WriteFile(path, []byte(`{"inputs": {"digest": "true"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := run([]string{"decide", path}, &b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, "run=true\n") || !strings.Contains(out, "mode=digest\n") {
		t.Fatalf("output = %q", out)
	}
	if strings.Contains(out, "number=") {
		t.Fatalf("digest decision printed a number: %q", out)
	}
}

func TestDispatchDigestInFlightFromJSON(t *testing.T) {
	// Regression: State.DigestInFlight must round-trip as digest_in_flight,
	// so a state file that says a digest run is in progress suppresses
	// the dispatch of another one.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "cfg.json")
	if err := os.WriteFile(cfgPath, []byte(`{"queue": {"max_parallel": 4}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	stPath := filepath.Join(dir, "state.json")
	if err := os.WriteFile(stPath, []byte(`{"queued": [], "in_flight": 1, "digest_in_flight": true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := run([]string{"dispatch", cfgPath, stPath}, &b); err != nil {
		t.Fatal(err)
	}
	if out := b.String(); strings.Contains(out, "digest") {
		t.Fatalf("dispatch with digest_in_flight=true emitted %q", out)
	}
	// The tag also encodes: an in-flight digest state must serialise under
	// the spec's key.
	data, err := json.Marshal(State{DigestInFlight: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"digest_in_flight":true`) {
		t.Fatalf("Marshal(State) = %s; want digest_in_flight key", data)
	}
}
