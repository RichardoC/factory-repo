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

func TestDispatchDigest(t *testing.T) {
	cfg, _ := withDefaults(Config{}) // MinBacklog defaults to 2
	// Free slot and a low backlog: one digest run with exactly {"digest": "true"}.
	got := Dispatch(cfg, State{Queued: []int{4}, InFlight: 0})
	if len(got) != 2 || got[0]["issue"] != "4" || len(got[1]) != 1 || got[1]["digest"] != "true" {
		t.Fatalf("Dispatch = %v", got)
	}
	// Backlog at MinBacklog: no digest run.
	if got := Dispatch(cfg, State{Queued: []int{4, 5}, InFlight: 0}); len(got) != 2 {
		t.Fatalf("Dispatch with full backlog = %v, want issues only", got)
	}
	// No free slot after the queued issues: no digest run.
	if got := Dispatch(cfg, State{Queued: []int{4, 5}, InFlight: 1}); len(got) != 1 {
		t.Fatalf("Dispatch with no free slot = %v, want issues only", got)
	}
	// A digest run already in progress: no second one.
	if got := Dispatch(cfg, State{Queued: []int{4}, InFlight: 1, DigestInFlight: true}); len(got) != 1 {
		t.Fatalf("Dispatch with digest in flight = %v, want issues only", got)
	}
	// A digest run takes a slot like any other run.
	if got := Dispatch(cfg, State{Queued: nil, InFlight: 1}); len(got) != 1 {
		t.Fatalf("Dispatch with one free slot = %v, want one digest run", got)
	}
	if got := Dispatch(cfg, State{Queued: nil, InFlight: 2}); len(got) != 0 {
		t.Fatalf("Dispatch with no free slot = %v, want nothing", got)
	}
	// At most one digest run per tick.
	cfg.Queue.MaxParallel = 4
	if got := Dispatch(cfg, State{Queued: nil, InFlight: 0}); len(got) != 1 {
		t.Fatalf("Dispatch with many free slots = %v, want one digest run", got)
	}
}

func TestDispatchDigestMinBacklog(t *testing.T) {
	cfg, _ := withDefaults(Config{Digest: Digest{MinBacklog: 1}})
	// One queued issue is not fewer than MinBacklog 1: no digest run.
	if got := Dispatch(cfg, State{Queued: []int{4}, InFlight: 0}); len(got) != 1 {
		t.Fatalf("Dispatch = %v, want issues only", got)
	}
	if got := Dispatch(cfg, State{Queued: nil, InFlight: 0}); len(got) != 1 || got[0]["digest"] != "true" {
		t.Fatalf("Dispatch = %v, want a digest run", got)
	}
}

func TestConfigDigestDefaults(t *testing.T) {
	cfg, err := withDefaults(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Digest.MinBacklog != 2 {
		t.Fatalf("MinBacklog = %d, want default 2", cfg.Digest.MinBacklog)
	}
	if cfg, _ := withDefaults(Config{Digest: Digest{MinBacklog: 5}}); cfg.Digest.MinBacklog != 5 {
		t.Fatal("explicit MinBacklog overwritten")
	}
	if _, err := withDefaults(Config{Digest: Digest{MinBacklog: -1}}); err == nil {
		t.Fatal("negative min_backlog accepted")
	}
}

func TestLoadConfigDigestNegative(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jobq.json")
	if err := os.WriteFile(path, []byte(`{"digest":{"min_backlog":-3}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err == nil || !strings.Contains(err.Error(), "min_backlog") {
		t.Fatalf("loadConfig = %v, want a min_backlog error", err)
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

func TestDecideReview(t *testing.T) {
	d := Decide(Event{Inputs: map[string]string{"review": "9"}})
	if !d.Run || d.Mode != "review" || d.Number != 9 {
		t.Fatalf("Decide = %+v", d)
	}
}

func TestDecideDigest(t *testing.T) {
	d := Decide(Event{Inputs: map[string]string{"digest": "true"}})
	if !d.Run || d.Mode != "digest" || d.Number != 0 {
		t.Fatalf("Decide = %+v", d)
	}
	// The digest input must be exactly true.
	for _, bad := range []string{"TRUE", "True", "1", "yes", "false"} {
		if d := Decide(Event{Inputs: map[string]string{"digest": bad}}); d.Run {
			t.Fatalf("digest %q ran: %+v", bad, d)
		}
	}
	// digest together with issue or review is refused.
	if d := Decide(Event{Inputs: map[string]string{"digest": "true", "issue": "7"}}); d.Run {
		t.Fatalf("digest with issue ran: %+v", d)
	}
	if d := Decide(Event{Inputs: map[string]string{"digest": "true", "review": "8"}}); d.Run {
		t.Fatalf("digest with review ran: %+v", d)
	}
}

func TestExecute(t *testing.T) {
	if got, err := Execute(Decision{Run: true, Mode: "review", Number: 3}); err != nil || got != "reviewed PR #3" {
		t.Fatalf("Execute = %q, %v", got, err)
	}
	if got, err := Execute(Decision{Run: true, Mode: "digest"}); err != nil || got != "wrote digest" {
		t.Fatalf("Execute = %q, %v", got, err)
	}
}

func TestRunDecideDigestOutput(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "event.json")
	if err := os.WriteFile(path, []byte(`{"inputs":{"digest":"true"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run([]string{"decide", path}, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "run=true\n") || !strings.Contains(got, "mode=digest\n") {
		t.Fatalf("decide output = %q", got)
	}
	if strings.Contains(got, "number=") {
		t.Fatalf("digest output has a number= line: %q", got)
	}
	// An issue dispatch still prints its number.
	if err := os.WriteFile(path, []byte(`{"inputs":{"issue":"7"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := run([]string{"decide", path}, &out); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "mode=issue\n") || !strings.Contains(got, "number=7\n") {
		t.Fatalf("issue output = %q", got)
	}
}

func TestRunDispatchDigestEndToEnd(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "jobq.json")
	stPath := filepath.Join(dir, "state.json")
	if err := os.WriteFile(cfgPath, []byte(`{"digest":{"min_backlog":2}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stPath, []byte(`{"queued":[4],"in_flight":0,"digest_in_flight":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run([]string{"dispatch", cfgPath, stPath}, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, `"digest":"true"`) || !strings.Contains(got, `"issue":"4"`) {
		t.Fatalf("dispatch output = %q", got)
	}
	// digest_in_flight: no digest run.
	if err := os.WriteFile(stPath, []byte(`{"queued":[4],"digest_in_flight":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := run([]string{"dispatch", cfgPath, stPath}, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), `"digest"`) {
		t.Fatalf("dispatch with digest in flight = %q", out.String())
	}
}
