package main

import (
	"bytes"
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

func TestWithDefaultsDigestMinBacklog(t *testing.T) {
	cfg, err := withDefaults(Config{})
	if err != nil || cfg.Digest.MinBacklog != 2 {
		t.Fatalf("withDefaults(Config{}) = %+v, %v", cfg, err)
	}
	if cfg, _ = withDefaults(Config{Digest: Digest{MinBacklog: 5}}); cfg.Digest.MinBacklog != 5 {
		t.Fatalf("MinBacklog = %d, want 5", cfg.Digest.MinBacklog)
	}
	if _, err = withDefaults(Config{Digest: Digest{MinBacklog: -1}}); err == nil {
		t.Fatal("negative digest.min_backlog was accepted")
	}
}

func TestLoadConfigRejectsNegativeMinBacklog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobq.json")
	if err := os.WriteFile(path, []byte(`{"digest":{"min_backlog":-3}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err == nil {
		t.Fatal("loadConfig accepted a negative min_backlog")
	}
}

func TestDispatchStartsDigestRunWhenIdle(t *testing.T) {
	cfg, _ := withDefaults(Config{Queue: Queue{MaxParallel: 2}})
	got := Dispatch(cfg, State{Queued: []int{4}})
	if len(got) != 2 || got[0]["issue"] != "4" || got[1]["digest"] != "true" {
		t.Fatalf("Dispatch = %v", got)
	}
}

func TestDispatchDigestConditions(t *testing.T) {
	cfg, _ := withDefaults(Config{Queue: Queue{MaxParallel: 2}})
	// No slot free after the queued issues have started.
	if got := Dispatch(cfg, State{Queued: []int{4, 5}}); len(got) != 2 {
		t.Fatalf("full slots: Dispatch = %v", got)
	}
	// Backlog at MinBacklog: no digest, even though a slot stays free.
	if got := Dispatch(cfg, State{Queued: []int{4, 5, 6}, InFlight: 1}); len(got) != 1 || got[0]["digest"] != "" {
		t.Fatalf("backlog at min: Dispatch = %v", got)
	}
	// Digest already in flight: no digest.
	if got := Dispatch(cfg, State{Queued: []int{4}, DigestInFlight: true}); len(got) != 1 {
		t.Fatalf("digest in flight: Dispatch = %v", got)
	}
	// At most one digest per tick even with several free slots.
	cfg.Queue.MaxParallel = 4
	if got := Dispatch(cfg, State{}); len(got) != 1 || got[0]["digest"] != "true" {
		t.Fatalf("several free slots: Dispatch = %v", got)
	}
}

func TestDecideDigest(t *testing.T) {
	d := Decide(Event{Inputs: map[string]string{"digest": "true"}})
	if !d.Run || d.Mode != "digest" || d.Number != 0 {
		t.Fatalf("Decide = %+v", d)
	}
	for _, inputs := range []map[string]string{
		{"digest": "yes"},
		{"digest": "false"},
		{"digest": "True"},
		{"digest": "true", "issue": "7"},
		{"digest": "true", "review": "8"},
	} {
		if d := Decide(Event{Inputs: inputs}); d.Run {
			t.Fatalf("Decide(%v) ran: %+v", inputs, d)
		}
	}
}

func TestExecuteDigest(t *testing.T) {
	got, err := Execute(Decision{Run: true, Mode: "digest"})
	if err != nil || got != "wrote digest" {
		t.Fatalf("Execute = %q, %v", got, err)
	}
}

func TestRunDecideDigestOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "event.json")
	if err := os.WriteFile(path, []byte(`{"inputs":{"digest":"true"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run([]string{"decide", path}, &out); err != nil {
		t.Fatal(err)
	}
	want := "run=true\nreason=digest dispatch\nmode=digest\n"
	if out.String() != want {
		t.Fatalf("decide output = %q, want %q", out.String(), want)
	}
}
