package main

import (
	"bytes"
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
		t.Fatalf("withDefaults: %v", err)
	}
	if cfg.Digest.MinBacklog != 2 {
		t.Fatalf("MinBacklog = %d, want 2", cfg.Digest.MinBacklog)
	}
	cfg, err = withDefaults(Config{Digest: Digest{MinBacklog: 5}})
	if err != nil || cfg.Digest.MinBacklog != 5 {
		t.Fatalf("explicit MinBacklog = %d, %v", cfg.Digest.MinBacklog, err)
	}
	if _, err := withDefaults(Config{Digest: Digest{MinBacklog: -1}}); err == nil {
		t.Fatal("negative MinBacklog was not an error")
	}
}

func TestLoadConfigDigest(t *testing.T) {
	dir := t.TempDir()
	good := dir + "/good.json"
	if err := os.WriteFile(good, []byte(`{"digest": {"min_backlog": -3}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(good); err == nil {
		t.Fatal("loadConfig accepted negative min_backlog")
	}
	unset := dir + "/unset.json"
	if err := os.WriteFile(unset, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(unset)
	if err != nil || cfg.Digest.MinBacklog != 2 {
		t.Fatalf("loadConfig = %+v, %v", cfg, err)
	}
}

func TestDispatchStartsDigestRun(t *testing.T) {
	cfg, _ := withDefaults(Config{})
	// Low backlog, free slot, no digest in flight: issues then one digest run.
	got := Dispatch(cfg, State{Queued: []int{4}, InFlight: 0})
	if len(got) != 2 || got[0]["issue"] != "4" || got[1]["digest"] != "true" {
		t.Fatalf("Dispatch = %v", got)
	}
	// Backlog at MinBacklog: no digest run.
	got = Dispatch(cfg, State{Queued: []int{4, 5}, InFlight: 0})
	if len(got) != 2 || got[0]["issue"] != "4" || got[1]["issue"] != "5" {
		t.Fatalf("Dispatch = %v", got)
	}
	// No free slot after queued issues started: no digest run.
	cfg.Queue.MaxParallel = 1
	got = Dispatch(cfg, State{Queued: []int{4}, InFlight: 0})
	if len(got) != 1 || got[0]["issue"] != "4" {
		t.Fatalf("Dispatch = %v", got)
	}
	// Digest already in flight: not started again.
	got = Dispatch(cfg, State{Queued: []int{4}, DigestInFlight: true})
	if len(got) != 1 || got[0]["issue"] != "4" {
		t.Fatalf("Dispatch = %v", got)
	}
	// Empty queue with a free slot: only the digest run, taking one slot.
	got = Dispatch(cfg, State{})
	if len(got) != 1 || got[0]["digest"] != "true" {
		t.Fatalf("Dispatch = %v", got)
	}
	// In-flight runs fill every slot: nothing started.
	got = Dispatch(cfg, State{InFlight: 1})
	if len(got) != 0 {
		t.Fatalf("Dispatch = %v", got)
	}
}

func TestStateDigestInFlightJSON(t *testing.T) {
	var st State
	if err := json.Unmarshal([]byte(`{"digest_in_flight": true, "in_flight": 1}`), &st); err != nil {
		t.Fatal(err)
	}
	if !st.DigestInFlight || st.InFlight != 1 {
		t.Fatalf("State = %+v", st)
	}
}

func TestDecideDigest(t *testing.T) {
	d := Decide(Event{Inputs: map[string]string{"digest": "true"}})
	if !d.Run || d.Mode != "digest" || d.Number != 0 {
		t.Fatalf("Decide = %+v", d)
	}
	// The digest input must be exactly "true".
	for _, v := range []string{"false", "TRUE", "1", "", "yes"} {
		if d := Decide(Event{Inputs: map[string]string{"digest": v}}); d.Run && d.Mode == "digest" {
			t.Fatalf("digest=%q ran: %+v", v, d)
		}
	}
	// Empty digest behaves as before: needs a numeric issue or review.
	if d := Decide(Event{Inputs: map[string]string{"digest": ""}}); d.Run {
		t.Fatalf("empty digest ran: %+v", d)
	}
	// digest with issue or review is refused.
	if d := Decide(Event{Inputs: map[string]string{"digest": "true", "issue": "7"}}); d.Run {
		t.Fatalf("digest+issue ran: %+v", d)
	}
	if d := Decide(Event{Inputs: map[string]string{"digest": "true", "review": "8"}}); d.Run {
		t.Fatalf("digest+review ran: %+v", d)
	}
}

func TestExecuteDigest(t *testing.T) {
	got, err := Execute(Decision{Run: true, Mode: "digest"})
	if err != nil || got != "wrote digest" {
		t.Fatalf("Execute = %q, %v", got, err)
	}
}

func TestDecideCommandDigestHasNoNumber(t *testing.T) {
	dir := t.TempDir()
	ev := dir + "/event.json"
	if err := os.WriteFile(ev, []byte(`{"inputs": {"digest": "true"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := run([]string{"decide", ev}, &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "mode=digest\n") || strings.Contains(out, "number=") {
		t.Fatalf("decide output = %q", out)
	}
}
