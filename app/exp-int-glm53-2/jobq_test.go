package main

import (
	"encoding/json"
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

func TestDigestConfigDefaults(t *testing.T) {
	cfg, err := withDefaults(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Digest.MinBacklog != 2 {
		t.Fatalf("default MinBacklog = %d, want 2", cfg.Digest.MinBacklog)
	}
	if cfg, err := withDefaults(Config{Digest: Digest{MinBacklog: 5}}); err != nil || cfg.Digest.MinBacklog != 5 {
		t.Fatalf("explicit MinBacklog = %d, %v; want 5, nil", cfg.Digest.MinBacklog, err)
	}
	if _, err := withDefaults(Config{Digest: Digest{MinBacklog: -1}}); err == nil {
		t.Fatal("negative digest.min_backlog was accepted")
	}
	if _, err := loadConfig(digestConfigFile(t, `"digest": {"min_backlog": 0}`)); err != nil {
		t.Fatalf("unset-style 0 not defaulted: %v", err)
	}
}

func digestConfigFile(t *testing.T, digest string) string {
	t.Helper()
	path := t.TempDir() + "/jobq.json"
	if err := os.WriteFile(path, []byte("{"+digest+"}"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDispatchDigestRun(t *testing.T) {
	cfg, _ := withDefaults(Config{}) // MaxParallel 2, MinBacklog 2

	// Free slot after queued issues started, backlog below MinBacklog: one digest run.
	got := Dispatch(cfg, State{Queued: []int{1}, InFlight: 0})
	if len(got) != 2 || got[0]["issue"] != "1" || got[1]["digest"] != "true" {
		t.Fatalf("Dispatch = %v, want issue 1 plus a digest run", got)
	}

	// No free slot after the queued issues: no digest run.
	got = Dispatch(cfg, State{Queued: []int{4, 5, 6}, InFlight: 1})
	if len(got) != 1 || got[0]["issue"] != "4" {
		t.Fatalf("Dispatch = %v, want only issue 4", got)
	}

	// Backlog at or above MinBacklog: no digest run.
	got = Dispatch(cfg, State{Queued: []int{1, 2}})
	if len(got) != 2 || got[1]["digest"] == "true" {
		t.Fatalf("Dispatch = %v, want only the two issues", got)
	}

	// A digest run already in progress blocks another.
	got = Dispatch(cfg, State{Queued: []int{1}, InFlight: 1, DigestInFlight: true})
	if len(got) != 1 || got[0]["issue"] != "1" || got[0]["digest"] == "true" {
		t.Fatalf("Dispatch = %v, want only issue 1", got)
	}

	// Custom MinBacklog of 1: one queued issue is already too much backlog.
	cfg, _ = withDefaults(Config{Digest: Digest{MinBacklog: 1}})
	if got := Dispatch(cfg, State{Queued: []int{1}}); len(got) != 1 || got[0]["issue"] != "1" {
		t.Fatalf("Dispatch = %v, want only issue 1", got)
	}
	if got := Dispatch(cfg, State{}); len(got) != 1 || got[0]["digest"] != "true" {
		t.Fatalf("Dispatch = %v, want one digest run", got)
	}
}

func TestDecideDigest(t *testing.T) {
	d := Decide(Event{Inputs: map[string]string{"digest": "true"}})
	if !d.Run || d.Mode != "digest" || d.Number != 0 || d.Reason != "digest dispatch" {
		t.Fatalf("Decide = %+v, want a digest run", d)
	}
	for bad, want := range map[string]string{
		"false": `digest must be exactly "true"`,
		"TRUE":  `digest must be exactly "true"`,
		" true": `digest must be exactly "true"`,
		"yes":   `digest must be exactly "true"`,
	} {
		if d := Decide(Event{Inputs: map[string]string{"digest": bad}}); d.Run || d.Reason != want {
			t.Fatalf("Decide(digest=%q) = %+v, want skip %q", bad, d, want)
		}
	}
	for key := range map[string]struct{}{"issue": {}, "review": {}} {
		ev := Event{Inputs: map[string]string{"digest": "true", key: "7"}}
		if d := Decide(ev); d.Run || d.Reason != "digest cannot be combined with issue or review" {
			t.Fatalf("Decide = %+v, want refused combination", d)
		}
	}
}

func TestExecuteDigest(t *testing.T) {
	d := Decide(Event{Inputs: map[string]string{"digest": "true"}})
	if got, err := Execute(d); err != nil || got != "wrote digest" {
		t.Fatalf("Execute = %q, %v; want \"wrote digest\", nil", got, err)
	}
}

func TestDecideUnchangedForIssueAndReview(t *testing.T) {
	if got, err := Execute(Decision{Run: true, Mode: "issue", Number: 9}); err != nil || got != "worked issue #9" {
		t.Fatalf("Execute issue = %q, %v", got, err)
	}
	d := Decide(Event{Inputs: map[string]string{"review": "3"}})
	if !d.Run || d.Mode != "review" || d.Number != 3 {
		t.Fatalf("Decide = %+v", d)
	}
}

func TestDecideCommandOutputForDigest(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/event.json"
	ev := `{"inputs": {"digest": "true"}}`
	if err := os.WriteFile(path, []byte(ev), 0o644); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := run([]string{"decide", path}, &b); err != nil {
		t.Fatal(err)
	}
	want := "run=true\nreason=digest dispatch\nmode=digest\n"
	if b.String() != want {
		t.Fatalf("decide output = %q, want %q", b.String(), want)
	}
}

// A state file refutes as the spec defines it: the digest-in-flight flag is
// read from the documented key "digest_in_flight" (issue #531 fix).
func TestDigestInFlightJSONKey(t *testing.T) {
	cfg, _ := withDefaults(Config{})
	var st State
	if err := json.Unmarshal([]byte(`{"queued":[],"in_flight":0,"digest_in_flight":true}`), &st); err != nil {
		t.Fatal(err)
	}
	if !st.DigestInFlight {
		t.Fatal(`state key "digest_in_flight" not parsed, DigestInFlight false`)
	}
	if got := Dispatch(cfg, st); len(got) != 0 {
		t.Fatalf("Dispatch = %v, want no runs while a digest run is in flight", got)
	}

	// And the state marshals back with the same key.
	b, err := json.Marshal(State{DigestInFlight: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"digest_in_flight":true`) {
		t.Fatalf(`marshalled state %s lacks "digest_in_flight"`, b)
	}
}
