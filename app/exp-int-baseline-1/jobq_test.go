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

func TestDigestConfigDefaults(t *testing.T) {
	cfg, err := withDefaults(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Digest.MinBacklog != 2 {
		t.Fatalf("MinBacklog default = %d, want 2", cfg.Digest.MinBacklog)
	}
	cfg, err = withDefaults(Config{Digest: Digest{MinBacklog: 5}})
	if err != nil || cfg.Digest.MinBacklog != 5 {
		t.Fatalf("MinBacklog=5: got %d, %v", cfg.Digest.MinBacklog, err)
	}
}

func TestDigestConfigNegativeIsError(t *testing.T) {
	if _, err := withDefaults(Config{Digest: Digest{MinBacklog: -1}}); err == nil {
		t.Fatal("negative digest.min_backlog was accepted")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "jobq.json")
	if err := os.WriteFile(path, []byte(`{"digest":{"min_backlog":-2}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err == nil {
		t.Fatal("loadConfig accepted a negative digest.min_backlog")
	}
}

func TestDispatchStartsDigestRun(t *testing.T) {
	cfg, _ := withDefaults(Config{})
	got := Dispatch(cfg, State{Queued: []int{4}, InFlight: 0})
	if len(got) != 2 || got[0]["issue"] != "4" {
		t.Fatalf("Dispatch = %v", got)
	}
	if len(got[1]) != 1 || got[1]["digest"] != "true" {
		t.Fatalf("digest inputs = %v, want exactly {\"digest\":\"true\"}", got[1])
	}
	// No backlog at all: only the digest run.
	if got := Dispatch(cfg, State{}); len(got) != 1 || got[0]["digest"] != "true" {
		t.Fatalf("Dispatch = %v", got)
	}
	// A custom MinBacklog is honoured.
	cfg.Digest.MinBacklog = 1
	if got := Dispatch(cfg, State{Queued: []int{4}}); len(got) != 1 || got[0]["issue"] != "4" {
		t.Fatalf("Dispatch with min_backlog=1 = %v, want no digest", got)
	}
}

func TestDispatchDigestNeedsFreeSlot(t *testing.T) {
	cfg, _ := withDefaults(Config{Queue: Queue{MaxParallel: 2}})
	// Both slots busy with queued issues: no digest run.
	got := Dispatch(cfg, State{Queued: []int{4, 5}, InFlight: 0})
	if len(got) != 2 || got[1]["digest"] != "" {
		t.Fatalf("Dispatch = %v, want only queued issues", got)
	}
	// One slot busy, nothing queued: digest takes the free slot.
	if got := Dispatch(cfg, State{InFlight: 1}); len(got) != 1 || got[0]["digest"] != "true" {
		t.Fatalf("Dispatch = %v, want one digest run", got)
	}
	// Every slot busy in flight: no digest run.
	if got := Dispatch(cfg, State{InFlight: 2}); len(got) != 0 {
		t.Fatalf("Dispatch = %v, want nothing", got)
	}
}

func TestDispatchBacklogBlocksDigest(t *testing.T) {
	cfg, _ := withDefaults(Config{}) // MinBacklog 2
	if got := Dispatch(cfg, State{Queued: []int{4, 5}}); len(got) != 2 {
		t.Fatalf("Dispatch = %v, want only queued issues", got)
	}
	if got := Dispatch(cfg, State{Queued: []int{4, 5, 6}, InFlight: 3}); len(got) != 0 {
		t.Fatalf("Dispatch = %v, want nothing", got)
	}
}

func TestDispatchDigestInFlightBlocksDigest(t *testing.T) {
	cfg, _ := withDefaults(Config{})
	if got := Dispatch(cfg, State{DigestInFlight: true}); len(got) != 0 {
		t.Fatalf("Dispatch = %v, want nothing", got)
	}
}

func TestDecideDigest(t *testing.T) {
	d := Decide(Event{Inputs: map[string]string{"digest": "true"}})
	if !d.Run || d.Mode != "digest" || d.Number != 0 {
		t.Fatalf("Decide = %+v", d)
	}
	if d := Decide(Event{Inputs: map[string]string{"digest": "TRUE"}}); d.Run {
		t.Fatalf("digest=TRUE ran: %+v", d)
	}
	if d := Decide(Event{Inputs: map[string]string{"digest": "1"}}); d.Run {
		t.Fatalf("digest=1 ran: %+v", d)
	}
	if d := Decide(Event{Inputs: map[string]string{"digest": "true", "issue": "7"}}); d.Run {
		t.Fatalf("digest with issue ran: %+v", d)
	}
	if d := Decide(Event{Inputs: map[string]string{"digest": "true", "review": "8"}}); d.Run {
		t.Fatalf("digest with review ran: %+v", d)
	}
}

func TestIssueAndReviewUnchanged(t *testing.T) {
	for _, tc := range []struct {
		inputs map[string]string
		mode   string
		number int
	}{
		{map[string]string{"issue": "7"}, "issue", 7},
		{map[string]string{"review": "8"}, "review", 8},
		{map[string]string{"issue": "7", "digest": "false"}, "", 0}, // digest present but not true: no run
	} {
		d := Decide(Event{Inputs: tc.inputs})
		if tc.mode == "" {
			if d.Run {
				t.Fatalf("Decide(%v) = %+v, want a skip", tc.inputs, d)
			}
			continue
		}
		if !d.Run || d.Mode != tc.mode || d.Number != tc.number {
			t.Fatalf("Decide(%v) = %+v", tc.inputs, d)
		}
	}
}

func TestExecuteDigest(t *testing.T) {
	got, err := Execute(Decision{Run: true, Mode: "digest"})
	if err != nil || got != "wrote digest" {
		t.Fatalf("Execute = %q, %v", got, err)
	}
}

func TestDecideDigestOutputHasNoNumberLine(t *testing.T) {
	dir := t.TempDir()
	ev := filepath.Join(dir, "event.json")
	if err := os.WriteFile(ev, []byte(`{"inputs":{"digest":"true"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run([]string{"decide", ev}, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "mode=digest") {
		t.Fatalf("decide output = %q, want mode=digest", got)
	}
	if strings.Contains(got, "number=") {
		t.Fatalf("decide output = %q, want no number= line", got)
	}
	// An issue dispatch still prints its number.
	if err := os.WriteFile(ev, []byte(`{"inputs":{"issue":"7"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := run([]string{"decide", ev}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "number=7") {
		t.Fatalf("decide output = %q, want number=7", out.String())
	}
}

func TestDispatchCommandEmitsDigestRun(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "jobq.json")
	stPath := filepath.Join(dir, "state.json")
	if err := os.WriteFile(cfgPath, []byte(`{"queue":{"max_parallel":2},"digest":{"min_backlog":3}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stPath, []byte(`{"queued":[4],"in_flight":0,"digest_in_flight":false}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run([]string{"dispatch", cfgPath, stPath}, &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"issue"`) || !strings.Contains(lines[1], `"digest":"true"`) {
		t.Fatalf("dispatch output = %q", out.String())
	}
}
