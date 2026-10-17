package main

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

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

func TestDigestDefaults(t *testing.T) {
	// Unset, and explicit 0, both default to 2.
	for _, cfg := range []Config{{}, {Digest: Digest{MinBacklog: 0}}} {
		got, err := withDefaults(cfg)
		if err != nil {
			t.Fatalf("withDefaults(%+v) error: %v", cfg, err)
		}
		if got.Digest.MinBacklog != 2 {
			t.Fatalf("MinBacklog = %d, want 2", got.Digest.MinBacklog)
		}
	}
	// A negative value is an error, like the existing settings.
	cfg, err := withDefaults(Config{Digest: Digest{MinBacklog: -1}})
	if err == nil {
		t.Fatalf("withDefaults accepted a negative min_backlog: %+v", cfg)
	}
}

func TestDispatchDigestRun(t *testing.T) {
	cfg, _ := withDefaults(Config{}) // min_backlog 2, max_parallel 2
	// A backlog below the threshold and a free slot: one digest run, and only
	// one, after the queued issues.
	got := Dispatch(cfg, State{Queued: []int{4}})
	if len(got) != 2 || got[0]["issue"] != "4" || got[1]["digest"] != "true" {
		t.Fatalf("Dispatch = %v", got)
	}
	// Backlog at the threshold: no digest.
	if got := Dispatch(cfg, State{Queued: []int{4, 5}}); len(got) != 2 || got[0]["digest"] != "" || got[1]["digest"] != "" {
		t.Fatalf("Dispatch at threshold = %v", got)
	}
	// No slot left after the queued issues: no digest. With max_parallel 2 and
	// one run in flight, the queued issue takes the only free slot.
	if got := Dispatch(cfg, State{Queued: []int{4}, InFlight: 1}); len(got) != 1 || got[0]["digest"] != "" {
		t.Fatalf("Dispatch with no free slot = %v", got)
	}
	// A backlog the slots cannot drain leaves no spare capacity either, even
	// though only one issue is started.
	if got := Dispatch(cfg, State{Queued: []int{4, 5, 6}}); len(got) != 2 {
		t.Fatalf("Dispatch with an undrained backlog = %v", got)
	}
	// One issue on a two-slot queue with nothing in flight: the issue and a
	// digest, in that order.
	if got := Dispatch(cfg, State{Queued: []int{4}}); len(got) != 2 || got[0]["issue"] != "4" || got[1]["digest"] != "true" {
		t.Fatalf("Dispatch with a spare slot = %v", got)
	}
	// A digest already in progress: no second digest.
	if got := Dispatch(cfg, State{DigestInFlight: true}); len(got) != 0 {
		t.Fatalf("Dispatch with a digest in flight = %v", got)
	}
	// That the flag arrives from JSON under its documented name.
	var st State
	if err := json.Unmarshal([]byte(`{"digest_in_flight":true}`), &st); err != nil || !st.DigestInFlight {
		t.Fatalf("State.DigestInFlight from JSON = %v, err %v", st.DigestInFlight, err)
	}
}

func TestConfigDigestJSON(t *testing.T) {
	var cfg Config
	if err := json.Unmarshal([]byte(`{"digest":{"min_backlog":3}}`), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.Digest.MinBacklog != 3 {
		t.Fatalf("Config.Digest.MinBacklog = %d, want 3", cfg.Digest.MinBacklog)
	}
}

func TestDecideDigest(t *testing.T) {
	d := Decide(Event{Inputs: map[string]string{"digest": "true"}})
	if !d.Run || d.Mode != "digest" || d.Number != 0 {
		t.Fatalf("Decide = %+v", d)
	}
	// The digest input must be exactly "true".
	for _, v := range []string{"false", "1", "TRUE", "yes"} {
		if d := Decide(Event{Inputs: map[string]string{"digest": v}}); d.Run {
			t.Fatalf("digest=%q ran: %+v", v, d)
		}
	}
	// digest together with issue or review is refused.
	for _, in := range []map[string]string{
		{"digest": "true", "issue": "7"},
		{"digest": "true", "review": "7"},
	} {
		if d := Decide(Event{Inputs: in}); d.Run {
			t.Fatalf("Decide(%v) ran: %+v", in, d)
		}
	}
	// Issue and review runs are unchanged.
	if d := Decide(Event{Inputs: map[string]string{"review": "9"}}); !d.Run || d.Mode != "review" || d.Number != 9 {
		t.Fatalf("review Decide = %+v", d)
	}
}

func TestExecuteDigest(t *testing.T) {
	got, err := Execute(Decision{Run: true, Mode: "digest"})
	if err != nil || got != "wrote digest" {
		t.Fatalf("Execute = %q, %v", got, err)
	}
}

func TestDecideOutputDigestHasNoNumber(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir+"/event.json", `{"inputs":{"digest":"true"}}`)
	var buf bytes.Buffer
	if err := run([]string{"decide", dir + "/event.json"}, &buf); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got, want := buf.String(), "run=true\nreason=digest dispatch\nmode=digest\n"; got != want {
		t.Fatalf("decide output = %q, want %q", got, want)
	}
}

func TestDispatchOutputDigestFromFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir+"/config.json", `{"queue":{"max_parallel":2}}`)
	writeFile(t, dir+"/state.json", `{"queued":[],"in_flight":0,"digest_in_flight":false}`)
	var buf bytes.Buffer
	if err := run([]string{"dispatch", dir + "/config.json", dir + "/state.json"}, &buf); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got, want := buf.String(), "{\"digest\":\"true\"}\n"; got != want {
		t.Fatalf("dispatch output = %q, want %q", got, want)
	}
}
