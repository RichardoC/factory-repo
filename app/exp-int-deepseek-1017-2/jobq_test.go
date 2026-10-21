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

func TestDigestDefaults(t *testing.T) {
	cfg, err := withDefaults(Config{})
	if err != nil {
		t.Fatalf("withDefaults: %v", err)
	}
	if cfg.Digest.MinBacklog != 2 {
		t.Fatalf("Digest.MinBacklog = %d, want 2", cfg.Digest.MinBacklog)
	}
	// An explicit value is kept.
	cfg, err = withDefaults(Config{Digest: Digest{MinBacklog: 5}})
	if err != nil || cfg.Digest.MinBacklog != 5 {
		t.Fatalf("withDefaults = %+v, %v", cfg, err)
	}
}

func TestDigestNegativeIsError(t *testing.T) {
	if _, err := withDefaults(Config{Digest: Digest{MinBacklog: -1}}); err == nil {
		t.Fatal("negative digest.min_backlog accepted")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "jobq.json")
	if err := os.WriteFile(path, []byte(`{"digest":{"min_backlog":-3}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err == nil {
		t.Fatal("loadConfig accepted a negative digest.min_backlog")
	}
}

func TestLoadConfigDigestJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jobq.json")
	if err := os.WriteFile(path, []byte(`{"digest":{"min_backlog":4}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Digest.MinBacklog != 4 {
		t.Fatalf("Digest.MinBacklog = %d, want 4", cfg.Digest.MinBacklog)
	}
}

func TestDispatchDigest(t *testing.T) {
	cfg, _ := withDefaults(Config{})
	// Backlog below the default minimum (2): no issue runs, one digest run.
	got := Dispatch(cfg, State{Queued: []int{9}})
	if len(got) != 2 || got[0]["issue"] != "9" || got[1]["digest"] != "true" || len(got[1]) != 1 {
		t.Fatalf("Dispatch = %v", got)
	}
	// Empty queue: one digest run.
	got = Dispatch(cfg, State{})
	if len(got) != 1 || got[0]["digest"] != "true" {
		t.Fatalf("Dispatch (empty) = %v", got)
	}
	// Backlog at or above the minimum: no digest.
	for _, queued := range [][]int{{1, 2}, {1, 2, 3}} {
		for _, in := range Dispatch(cfg, State{Queued: queued}) {
			if _, ok := in["digest"]; ok {
				t.Fatalf("digest started with backlog %v: %v", queued, in)
			}
		}
	}
	// No free slot after the queued issues are started: no digest.
	cfgOne, _ := withDefaults(Config{Queue: Queue{MaxParallel: 1}})
	got = Dispatch(cfgOne, State{Queued: []int{9}})
	if len(got) != 1 || got[0]["issue"] != "9" {
		t.Fatalf("Dispatch (full) = %v", got)
	}
	// No free slot at all: no digest.
	got = Dispatch(cfg, State{InFlight: 2})
	if len(got) != 0 {
		t.Fatalf("Dispatch (in flight full) = %v", got)
	}
	// A digest already in progress: none started.
	got = Dispatch(cfg, State{DigestInFlight: true})
	if len(got) != 0 {
		t.Fatalf("Dispatch (digest in flight) = %v", got)
	}
	// At most one digest per tick, even with several slots free.
	got = Dispatch(cfg, State{Queued: []int{9}})
	digests := 0
	for _, in := range got {
		if in["digest"] != "" {
			digests++
		}
	}
	if digests != 1 {
		t.Fatalf("digests this tick = %d, want 1 (%v)", digests, got)
	}
}

func TestStateDigestInFlightJSON(t *testing.T) {
	var st State
	if err := readJSON(writeTempJSON(t, `{"queued":[1],"digest_in_flight":true}`), &st); err != nil {
		t.Fatalf("readJSON: %v", err)
	}
	if !st.DigestInFlight || len(st.Queued) != 1 {
		t.Fatalf("State = %+v", st)
	}
}

func TestDecideDigest(t *testing.T) {
	d := Decide(Event{Inputs: map[string]string{"digest": "true"}})
	if !d.Run || d.Mode != "digest" || d.Number != 0 {
		t.Fatalf("Decide = %+v", d)
	}
	for _, bad := range []map[string]string{
		{"digest": "false"},
		{"digest": "1"},
		{"digest": "TRUE"},
		{"digest": ""},
		{"digest": "true", "issue": "7"},
		{"digest": "true", "review": "8"},
		{"digest": "true", "issue": "7", "review": "8"},
	} {
		if d := Decide(Event{Inputs: bad}); d.Run {
			t.Fatalf("Decide(%v) ran: %+v", bad, d)
		}
	}
}

func TestExecuteDigest(t *testing.T) {
	d := Decide(Event{Inputs: map[string]string{"digest": "true"}})
	got, err := Execute(d)
	if err != nil || got != "wrote digest" {
		t.Fatalf("Execute = %q, %v", got, err)
	}
}

func TestDecideCommandPrintsDigestWithoutNumber(t *testing.T) {
	path := writeTempJSON(t, `{"inputs":{"digest":"true"}}`)
	var out bytes.Buffer
	if err := run([]string{"decide", path}, &out); err != nil {
		t.Fatalf("run decide: %v", err)
	}
	text := out.String()
	if !strings.Contains(text, "mode=digest\n") {
		t.Fatalf("decide output = %q, want mode=digest", text)
	}
	if strings.Contains(text, "number=") {
		t.Fatalf("decide output = %q, want no number line", text)
	}
	// Issue runs still print their number.
	path = writeTempJSON(t, `{"inputs":{"issue":"7"}}`)
	out.Reset()
	if err := run([]string{"decide", path}, &out); err != nil {
		t.Fatalf("run decide: %v", err)
	}
	if !strings.Contains(out.String(), "mode=issue\nnumber=7\n") {
		t.Fatalf("decide output = %q, want mode=issue and number=7", out.String())
	}
}

func TestDispatchCommandStartsDigest(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "jobq.json")
	if err := os.WriteFile(cfgPath, []byte(`{"queue":{"max_parallel":3},"digest":{"min_backlog":2}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stPath := filepath.Join(dir, "state.json")
	if err := os.WriteFile(stPath, []byte(`{"queued":[4],"in_flight":0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run([]string{"dispatch", cfgPath, stPath}, &out); err != nil {
		t.Fatalf("run dispatch: %v", err)
	}
	want := `{"issue":"4"}` + "\n" + `{"digest":"true"}` + "\n"
	if out.String() != want {
		t.Fatalf("dispatch output = %q, want %q", out.String(), want)
	}
}

func writeTempJSON(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "x.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestDecideIssueReviewUnchanged pins the issue and review decisions, so a
// digest addition that changed them would fail here.
func TestDecideIssueReviewUnchanged(t *testing.T) {
	for _, tc := range []struct {
		inputs map[string]string
		mode   string
		number int
	}{
		{map[string]string{"issue": "7"}, "issue", 7},
		{map[string]string{"review": "8"}, "review", 8},
	} {
		d := Decide(Event{Inputs: tc.inputs})
		if !d.Run || d.Mode != tc.mode || d.Number != tc.number {
			t.Fatalf("Decide(%v) = %+v, want mode %s number %d", tc.inputs, d, tc.mode, tc.number)
		}
	}
	for _, bad := range []map[string]string{
		{"issue": "0"},
		{"issue": "-1"},
		{"issue": "x"},
		{"review": "0"},
		{"review": "8a"},
	} {
		if d := Decide(Event{Inputs: bad}); d.Run {
			t.Fatalf("Decide(%v) ran: %+v", bad, d)
		}
	}
}
