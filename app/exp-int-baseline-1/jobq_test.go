package main

import (
	"bytes"
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

func TestDigestMinBacklogDefaultsAndValidation(t *testing.T) {
	cfg, err := withDefaults(Config{})
	if err != nil {
		t.Fatalf("withDefaults: %v", err)
	}
	if cfg.Digest.MinBacklog != 2 {
		t.Fatalf("default MinBacklog = %d, want 2", cfg.Digest.MinBacklog)
	}
	if _, err := withDefaults(Config{Digest: Digest{MinBacklog: -1}}); err == nil {
		t.Fatal("negative min_backlog: want error")
	}
	path := filepath.Join(t.TempDir(), "jobq.json")
	if err := os.WriteFile(path, []byte(`{"digest":{"min_backlog":-3}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err == nil {
		t.Fatal("loadConfig with negative min_backlog: want error")
	}
}

func TestDispatchStartsDigestRun(t *testing.T) {
	cfg, _ := withDefaults(Config{Queue: Queue{MaxParallel: 3}})
	got := Dispatch(cfg, State{Queued: []int{4}})
	if len(got) != 2 || got[0]["issue"] != "4" || got[1]["digest"] != "true" {
		t.Fatalf("Dispatch = %v", got)
	}
	if got := Dispatch(cfg, State{}); len(got) != 1 || got[0]["digest"] != "true" {
		t.Fatalf("Dispatch = %v", got)
	}
}

func TestDispatchDigestConditions(t *testing.T) {
	cfg, _ := withDefaults(Config{Queue: Queue{MaxParallel: 2}})
	// Backlog at MinBacklog: no digest run.
	if got := Dispatch(cfg, State{Queued: []int{4, 5}}); len(got) != 2 {
		t.Fatalf("Dispatch = %v, want only the two issues", got)
	}
	// Backlog below MinBacklog, but the queued issues already fill the slots.
	cfg, _ = withDefaults(Config{Queue: Queue{MaxParallel: 1}})
	if got := Dispatch(cfg, State{Queued: []int{4}}); len(got) != 1 || got[0]["digest"] != "" {
		t.Fatalf("Dispatch = %v, want only the issue", got)
	}
	// A digest run is already in progress.
	if got := Dispatch(cfg, State{DigestInFlight: true}); got != nil {
		t.Fatalf("Dispatch = %v, want none", got)
	}
	// At most one digest run per tick even with two free slots.
	cfg, _ = withDefaults(Config{Queue: Queue{MaxParallel: 4}})
	got := Dispatch(cfg, State{})
	if len(got) != 1 || got[0]["digest"] != "true" {
		t.Fatalf("Dispatch = %v, want exactly one digest run", got)
	}
}

func TestDispatchDigestInFlightTakesSlot(t *testing.T) {
	cfg, _ := withDefaults(Config{}) // max_parallel 2, min_backlog 2
	got := Dispatch(cfg, State{Queued: []int{4}, InFlight: 1})
	if len(got) != 1 || got[0]["issue"] != "4" {
		t.Fatalf("Dispatch = %v, want only the issue", got)
	}
}

func TestDecideDigest(t *testing.T) {
	d := Decide(Event{Inputs: map[string]string{"digest": "true"}})
	if !d.Run || d.Mode != "digest" || d.Number != 0 {
		t.Fatalf("Decide = %+v", d)
	}
	if d := Decide(Event{Inputs: map[string]string{"digest": "yes"}}); d.Run {
		t.Fatalf("digest=yes ran: %+v", d)
	}
	if d := Decide(Event{Inputs: map[string]string{"digest": "true", "issue": "7"}}); d.Run {
		t.Fatalf("digest with issue ran: %+v", d)
	}
	if d := Decide(Event{Inputs: map[string]string{"digest": "true", "review": "8"}}); d.Run {
		t.Fatalf("digest with review ran: %+v", d)
	}
}

func TestExecuteDigest(t *testing.T) {
	got, err := Execute(Decision{Run: true, Mode: "digest"})
	if err != nil || got != "wrote digest" {
		t.Fatalf("Execute = %q, %v", got, err)
	}
}

func TestDecideCommandDigestOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "event.json")
	if err := os.WriteFile(path, []byte(`{"inputs":{"digest":"true"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := run([]string{"decide", path}, &buf); err != nil {
		t.Fatal(err)
	}
	want := "run=true\nreason=digest dispatch\nmode=digest\n"
	if buf.String() != want {
		t.Fatalf("decide output = %q, want %q", buf.String(), want)
	}
	if strings.Contains(buf.String(), "number=") {
		t.Fatalf("digest decision printed a number: %q", buf.String())
	}
}
