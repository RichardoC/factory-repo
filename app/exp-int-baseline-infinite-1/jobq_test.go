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

func TestDigestMinBacklogDefaultsToTwo(t *testing.T) {
	for _, cfg := range []Config{{}, {Digest: Digest{MinBacklog: 0}}} {
		got, err := withDefaults(cfg)
		if err != nil {
			t.Fatalf("withDefaults(%v) = %v", cfg, err)
		}
		if got.Digest.MinBacklog != 2 {
			t.Fatalf("MinBacklog = %d, want 2", got.Digest.MinBacklog)
		}
	}
	if _, err := withDefaults(Config{Digest: Digest{MinBacklog: -1}}); err == nil {
		t.Fatal("negative digest.min_backlog was accepted")
	}
}

func TestDispatchStartsDigestRunWhenBacklogLow(t *testing.T) {
	cfg, _ := withDefaults(Config{})
	got := Dispatch(cfg, State{})
	if len(got) != 1 || got[0]["digest"] != "true" || len(got[0]) != 1 {
		t.Fatalf("Dispatch = %v, want exactly [{digest:true}]", got)
	}
}

func TestDispatchDigestTakesLeftoverSlot(t *testing.T) {
	cfg, _ := withDefaults(Config{}) // MaxParallel 2
	got := Dispatch(cfg, State{Queued: []int{4}})
	// The queued issue is started first; a slot is still free and the backlog
	// (1) is below MinBacklog (2), so the leftover slot goes to a digest run.
	if len(got) != 2 || got[0]["issue"] != "4" || got[1]["digest"] != "true" {
		t.Fatalf("Dispatch = %v, want [issue 4, digest]", got)
	}
}

func TestDispatchNoDigestWhenBacklogAtMin(t *testing.T) {
	cfg, _ := withDefaults(Config{Queue: Queue{MaxParallel: 3}})
	got := Dispatch(cfg, State{Queued: []int{4, 5}})
	// A slot is free after the queued issues, but the backlog (2) is not
	// below MinBacklog (2), so the slot stays idle.
	if len(got) != 2 || got[0]["issue"] != "4" || got[1]["issue"] != "5" {
		t.Fatalf("Dispatch = %v, want the queued issues only at MinBacklog", got)
	}
}

func TestDispatchNoDigestWithoutFreeSlot(t *testing.T) {
	cfg, _ := withDefaults(Config{})
	if got := Dispatch(cfg, State{InFlight: 2}); len(got) != 0 {
		t.Fatalf("Dispatch = %v, want no runs without a free slot", got)
	}
}

func TestDispatchNoDigestWhenOneInFlight(t *testing.T) {
	cfg, _ := withDefaults(Config{})
	if got := Dispatch(cfg, State{DigestInFlight: true}); len(got) != 0 {
		t.Fatalf("Dispatch = %v, want no second digest run", got)
	}
}

func TestDispatchAtMostOneDigestPerTick(t *testing.T) {
	cfg, _ := withDefaults(Config{}) // MaxParallel 2, both slots free
	got := Dispatch(cfg, State{})
	if len(got) != 1 {
		t.Fatalf("Dispatch = %v, want at most one digest run", got)
	}
}

func TestDecideDigest(t *testing.T) {
	d := Decide(Event{Inputs: map[string]string{"digest": "true"}})
	if !d.Run || d.Mode != "digest" || d.Number != 0 {
		t.Fatalf("Decide = %+v, want a digest run with no number", d)
	}
	if d := Decide(Event{Inputs: map[string]string{"digest": "yes"}}); d.Run {
		t.Fatalf("digest=%q ran: %+v", "yes", d)
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

func TestRunDecideDigestOutput(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/event.json"
	if err := os.WriteFile(path, []byte(`{"inputs":{"digest":"true"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run([]string{"decide", path}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "mode=digest\n") || strings.Contains(out.String(), "number=") {
		t.Fatalf("decide output = %q, want mode=digest and no number line", out.String())
	}
}
