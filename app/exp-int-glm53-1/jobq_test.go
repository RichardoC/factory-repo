package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func jsonUnmarshalState(body string, st *State) error { return json.Unmarshal([]byte(body), st) }

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

func TestWithDefaultsDigest(t *testing.T) {
	cfg, err := withDefaults(Config{})
	if err != nil || cfg.Digest.MinBacklog != 2 {
		t.Fatalf("digest.min_backlog default = %d, %v; want 2, <nil>", cfg.Digest.MinBacklog, err)
	}
	cfg, err = withDefaults(Config{Digest: Digest{MinBacklog: 5}})
	if err != nil || cfg.Digest.MinBacklog != 5 {
		t.Fatalf("digest.min_backlog kept = %d, %v; want 5, <nil>", cfg.Digest.MinBacklog, err)
	}
	if _, err := withDefaults(Config{Digest: Digest{MinBacklog: -1}}); err == nil {
		t.Fatal("negative digest.min_backlog accepted")
	}
}

func TestLoadConfigDigest(t *testing.T) {
	write := func(body string) string {
		path := filepath.Join(t.TempDir(), "jobq.json")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	cfg, err := loadConfig(write(`{"digest": {"min_backlog": 4}}`))
	if err != nil || cfg.Digest.MinBacklog != 4 {
		t.Fatalf("loadConfig digest = %d, %v; want 4, <nil>", cfg.Digest.MinBacklog, err)
	}
	if _, err := loadConfig(write(`{"digest": {"min_backlog": -2}}`)); err == nil {
		t.Fatal("loadConfig accepted negative digest.min_backlog")
	}
}

func TestDispatchDigestOnlyWhenSpare(t *testing.T) {
	cfg, _ := withDefaults(Config{Digest: Digest{MinBacklog: 2}})
	isDigest := func(in map[string]string) bool { return in["digest"] == "true" }

	// No slot free after queued issues: no digest.
	got := Dispatch(cfg, State{Queued: []int{1, 2}, InFlight: 0})
	if len(got) != 2 || got[0]["issue"] != "1" || got[1]["issue"] != "2" {
		t.Fatalf("queued issues displaced: %v", got)
	}

	// Slot free but backlog >= MinBacklog: no digest.
	got = Dispatch(cfg, State{Queued: []int{1, 2}, InFlight: 1})
	if len(got) != 1 || got[0]["issue"] != "1" || isDigest(got[0]) {
		t.Fatalf("digest dispatched with a full backlog: %v", got)
	}

	// Slot free, low backlog: exactly one digest run, and it takes a slot.
	got = Dispatch(cfg, State{Queued: []int{1}, InFlight: 0})
	if len(got) != 2 || got[0]["issue"] != "1" || !isDigest(got[1]) {
		t.Fatalf("expected issue then digest: %v", got)
	}

	// Nothing queued at all: a digest run fills the spare slot.
	got = Dispatch(cfg, State{})
	if len(got) != 1 || !isDigest(got[0]) {
		t.Fatalf("expected one digest run: %v", got)
	}

	// One digest at most per tick: never two.
	cfg2, _ := withDefaults(Config{Digest: Digest{MinBacklog: 1}})
	got = Dispatch(cfg2, State{})
	if len(got) != 1 {
		t.Fatalf("more than one digest per tick: %v", got)
	}

	// A digest already in progress blocks another.
	got = Dispatch(cfg, State{DigestInFlight: true})
	if len(got) != 0 {
		t.Fatalf("second digest dispatched: %v", got)
	}
}

func TestDispatchMinBacklogBoundary(t *testing.T) {
	// Fewer queued than MinBacklog means digest; equal means no digest.
	cfg, _ := withDefaults(Config{Queue: Queue{MaxParallel: 4}, Digest: Digest{MinBacklog: 3}})
	if got := Dispatch(cfg, State{Queued: []int{1, 2, 3}}); len(got) != 3 {
		t.Fatalf("digest at exactly min_backlog: %v", got)
	}
	got := Dispatch(cfg, State{Queued: []int{1, 2}})
	if len(got) != 3 || got[len(got)-1]["digest"] != "true" {
		t.Fatalf("no digest below min_backlog: %v", got)
	}
}

func TestDecideDigest(t *testing.T) {
	d := Decide(Event{Inputs: map[string]string{"digest": "true"}})
	if !d.Run || d.Mode != "digest" || d.Number != 0 {
		t.Fatalf("Decide digest = %+v", d)
	}
	if got, err := Execute(d); err != nil || got != "wrote digest" {
		t.Fatalf("Execute digest = %q, %v; want \"wrote digest\"", got, err)
	}

	for _, ev := range []Event{
		{Inputs: map[string]string{"digest": "false"}},
		{Inputs: map[string]string{"digest": "TRUE"}},
		{Inputs: map[string]string{"digest": "1"}},
		{Inputs: map[string]string{"digest": "true", "issue": "7"}},
		{Inputs: map[string]string{"digest": "true", "review": "2"}},
		{Inputs: map[string]string{"digest": "yes", "issue": "7"}},
	} {
		if d := Decide(ev); d.Run {
			t.Fatalf("bad digest input ran: %+v from %v", d, ev.Inputs)
		} else if _, err := Execute(d); err == nil {
			t.Fatalf("execute of a skipped decision succeeded: %+v", d)
		}
	}
}

func TestRunDecideDigestOutput(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "event.json")
	if err := os.WriteFile(path, []byte(`{"inputs": {"digest": "true"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := run([]string{"decide", path}, &b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, "run=true\n") || !strings.Contains(out, "mode=digest\n") {
		t.Fatalf("decide digest output = %q", out)
	}
	if strings.Contains(out, "number=") {
		t.Fatalf("digest output has a number line: %q", out)
	}
}

func TestRunDecideIssueStillHasNumber(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "event.json")
	if err := os.WriteFile(path, []byte(`{"inputs": {"issue": "9"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := run([]string{"decide", path}, &b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"run=true\n", "mode=issue\n", "number=9\n"} {
		if !strings.Contains(out, want) {
			t.Fatalf("decide issue output %q lacks %q", out, want)
		}
	}
}

func TestStateDigestInFlightJSON(t *testing.T) {
	// The field must round-trip as "digest_in_flight" in dispatch state JSON.
	var st State
	if err := jsonUnmarshalState(`{"queued": [], "in_flight": 0, "digest_in_flight": true}`, &st); err != nil {
		t.Fatal(err)
	}
	if !st.DigestInFlight {
		t.Fatalf("digest_in_flight not read: %+v", st)
	}
	cfg, _ := withDefaults(Config{})
	if got := Dispatch(cfg, st); len(got) != 0 {
		t.Fatalf("digest dispatched despite digest_in_flight: %v", got)
	}
}
