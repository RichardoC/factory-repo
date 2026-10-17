package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- config -----------------------------------------------------------------

func TestWithDefaultsDigestMinBacklogDefaultsToTwo(t *testing.T) {
	for _, in := range []int{0} {
		cfg, err := withDefaults(Config{Digest: Digest{MinBacklog: in}})
		if err != nil {
			t.Fatalf("withDefaults(%d) error: %v", in, err)
		}
		if cfg.Digest.MinBacklog != 2 {
			t.Fatalf("MinBacklog(%d) = %d, want 2", in, cfg.Digest.MinBacklog)
		}
	}
	// Unset: no digest section at all.
	cfg, err := withDefaults(Config{})
	if err != nil || cfg.Digest.MinBacklog != 2 {
		t.Fatalf("unset MinBacklog = %d, %v; want 2, nil", cfg.Digest.MinBacklog, err)
	}
}

func TestWithDefaultsDigestMinBacklogNegativeIsError(t *testing.T) {
	if _, err := withDefaults(Config{Digest: Digest{MinBacklog: -1}}); err == nil {
		t.Fatal("negative digest.min_backlog accepted")
	}
}

func TestLoadConfigDigest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jobq.json")

	// The JSON key is "digest" and, inside it, "min_backlog".
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write(`{"digest": {"min_backlog": 5}}`)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig error: %v", err)
	}
	if cfg.Digest.MinBacklog != 5 {
		t.Fatalf("MinBacklog = %d, want 5", cfg.Digest.MinBacklog)
	}
	if cfg.MaxRetries != 3 || cfg.Queue.MaxParallel != 2 {
		t.Fatalf("other defaults lost: %+v", cfg)
	}

	write(`{"digest": {"min_backlog": 0}}`)
	if cfg, err = loadConfig(path); err != nil || cfg.Digest.MinBacklog != 2 {
		t.Fatalf("explicit 0 = %+v, %v; want default 2", cfg.Digest, err)
	}

	write(`{"digest": {"min_backlog": -3}}`)
	if _, err = loadConfig(path); err == nil {
		t.Fatal("loadConfig accepted a negative min_backlog")
	}
}

// --- dispatch ---------------------------------------------------------------

func digestRuns(got []map[string]string) int {
	n := 0
	for _, in := range got {
		if _, ok := in["digest"]; ok {
			n++
		}
	}
	return n
}

// All of: a slot free after the queued issues, backlog below MinBacklog, and
// no digest already in flight.
func TestDispatchStartsDigestWhenSlotFreeAndBacklogLow(t *testing.T) {
	cfg, _ := withDefaults(Config{Queue: Queue{MaxParallel: 2}, Digest: Digest{MinBacklog: 2}})
	got := Dispatch(cfg, State{Queued: []int{4}, InFlight: 0})
	if len(got) != 2 {
		t.Fatalf("Dispatch = %v, want the issue then one digest", got)
	}
	if got[0]["issue"] != "4" {
		t.Fatalf("issue run first: %v", got)
	}
	if got[1]["digest"] != "true" {
		t.Fatalf("digest inputs = %v, want exactly {digest:true}", got[1])
	}
}

func TestDispatchDigestInputsExactlyDigestTrue(t *testing.T) {
	cfg, _ := withDefaults(Config{})
	got := Dispatch(cfg, State{})
	if len(got) != 1 || len(got[0]) != 1 || got[0]["digest"] != "true" {
		t.Fatalf("digest dispatch = %v, want [{digest:true}]", got)
	}
}

// len(Queued) < MinBacklog: at the threshold, and above it, no digest.
func TestDispatchNoDigestWhenBacklogAtOrAboveMinBacklog(t *testing.T) {
	cfg, _ := withDefaults(Config{Queue: Queue{MaxParallel: 3}, Digest: Digest{MinBacklog: 2}})
	for _, queued := range [][]int{{4, 5}, {4, 5, 6}} {
		got := Dispatch(cfg, State{Queued: queued})
		if digestRuns(got) != 0 {
			t.Fatalf("backlog %v started a digest: %v", queued, got)
		}
	}
	// Below the threshold it does start one.
	if got := Dispatch(cfg, State{Queued: []int{4}}); digestRuns(got) != 1 {
		t.Fatalf("backlog [4] started %d digests: %v", digestRuns(got), got)
	}
}

func TestDispatchNoDigestWhenDigestInFlight(t *testing.T) {
	cfg, _ := withDefaults(Config{})
	got := Dispatch(cfg, State{Queued: []int{4}, DigestInFlight: true})
	if len(got) != 1 || got[0]["issue"] != "4" {
		t.Fatalf("DigestInFlight did not block the digest: %v", got)
	}
	if digestRuns(Dispatch(cfg, State{DigestInFlight: true})) != 0 {
		t.Fatal("digest started while a digest was in flight")
	}
}

// No slot free after the queued issues: no digest, and the issue-only
// behaviour is exactly as before.
func TestDispatchNoDigestWhenNoSlotFree(t *testing.T) {
	cfg, _ := withDefaults(Config{Queue: Queue{MaxParallel: 1}, Digest: Digest{MinBacklog: 2}})
	got := Dispatch(cfg, State{Queued: []int{4}, InFlight: 0})
	if len(got) != 1 || got[0]["issue"] != "4" {
		t.Fatalf("expected only the issue run: %v", got)
	}
	// Every slot taken by in-flight runs.
	got = Dispatch(cfg, State{Queued: []int{4}, InFlight: 1})
	if len(got) != 0 {
		t.Fatalf("no free slot but Dispatch = %v", got)
	}
}

func TestDispatchIssueOnlyRegression(t *testing.T) {
	cfg, _ := withDefaults(Config{})
	got := Dispatch(cfg, State{Queued: []int{4, 5, 6}, InFlight: 1})
	if len(got) != 1 || got[0]["issue"] != "4" {
		t.Fatalf("Dispatch = %v", got)
	}
	// A full queue with every slot free still fills only the free slots.
	got = Dispatch(cfg, State{Queued: []int{4, 5, 6}})
	if len(got) != 2 || got[0]["issue"] != "4" || got[1]["issue"] != "5" {
		t.Fatalf("Dispatch = %v", got)
	}
}

// --- decide -----------------------------------------------------------------

func TestDecideDigestTrue(t *testing.T) {
	d := Decide(Event{Inputs: map[string]string{"digest": "true"}})
	if !d.Run || d.Mode != "digest" || d.Number != 0 {
		t.Fatalf("Decide = %+v, want a digest run with no number", d)
	}
}

func TestDecideDigestFalse(t *testing.T) {
	for _, bad := range []string{"false", "TRUE", "True", "1", "yes", " true"} {
		d := Decide(Event{Inputs: map[string]string{"digest": bad}})
		if d.Run {
			t.Fatalf("digest=%q ran: %+v", bad, d)
		}
	}
}

func TestDecideDigestMixedIsRefused(t *testing.T) {
	for _, inputs := range []map[string]string{
		{"digest": "true", "issue": "7"},
		{"digest": "true", "review": "8"},
		{"digest": "true", "issue": "7", "review": "8"},
		{"digest": "false", "issue": "7"},
	} {
		if d := Decide(Event{Inputs: inputs}); d.Run {
			t.Fatalf("inputs %v ran: %+v", inputs, d)
		}
	}
}

func TestDecideIssueAndReviewRegression(t *testing.T) {
	d := Decide(Event{Inputs: map[string]string{"issue": "7"}})
	if !d.Run || d.Mode != "issue" || d.Number != 7 {
		t.Fatalf("Decide issue = %+v", d)
	}
	d = Decide(Event{Inputs: map[string]string{"review": "8"}})
	if !d.Run || d.Mode != "review" || d.Number != 8 {
		t.Fatalf("Decide review = %+v", d)
	}
	if d := Decide(Event{Inputs: map[string]string{"issue": "7", "review": "8"}}); d.Run {
		t.Fatalf("both inputs ran: %+v", d)
	}
	if d := Decide(Event{Inputs: map[string]string{"issue": "0"}}); d.Run {
		t.Fatalf("issue 0 ran: %+v", d)
	}
	if d := Decide(Event{Inputs: map[string]string{}}); d.Run {
		t.Fatalf("empty inputs ran: %+v", d)
	}
}

// --- execute ----------------------------------------------------------------

func TestExecuteDigest(t *testing.T) {
	got, err := Execute(Decide(Event{Inputs: map[string]string{"digest": "true"}}))
	if err != nil || got != "wrote digest" {
		t.Fatalf("Execute digest = %q, %v; want \"wrote digest\"", got, err)
	}
}

func TestExecuteRegression(t *testing.T) {
	if got, err := Execute(Decision{Run: true, Mode: "review", Number: 3}); err != nil || got != "reviewed PR #3" {
		t.Fatalf("Execute = %q, %v", got, err)
	}
	if got, err := Execute(Decision{Run: true, Mode: "issue", Number: 3}); err != nil || got != "worked issue #3" {
		t.Fatalf("Execute = %q, %v", got, err)
	}
	if _, err := Execute(Decision{Reason: "nope"}); err == nil {
		t.Fatal("Execute ran a skip")
	}
}

// --- CLI --------------------------------------------------------------------

func TestRunDecideDigestPrintsNoNumber(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "event.json")
	if err := os.WriteFile(path, []byte(`{"inputs": {"digest": "true"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := run([]string{"decide", path}, &out); err != nil {
		t.Fatalf("run decide: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "run=true\n") || !strings.Contains(got, "mode=digest\n") {
		t.Fatalf("decide output = %q", got)
	}
	if strings.Contains(got, "number=") {
		t.Fatalf("digest output has a number= line: %q", got)
	}
}

func TestRunDecideIssueStillPrintsNumber(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "event.json")
	if err := os.WriteFile(path, []byte(`{"inputs": {"issue": "7"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := run([]string{"decide", path}, &out); err != nil {
		t.Fatalf("run decide: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "mode=issue\n") || !strings.Contains(got, "number=7\n") {
		t.Fatalf("decide output = %q", got)
	}
}

// The dispatcher reads State from the state JSON file, so the state's wire
// keys matter: digest_in_flight must set DigestInFlight.
func TestStateJSONKeys(t *testing.T) {
	var st State
	if err := json.Unmarshal([]byte(`{"queued": [4, 5], "in_flight": 1, "digest_in_flight": true}`), &st); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if !st.DigestInFlight {
		t.Fatalf("digest_in_flight JSON key did not set DigestInFlight: got %v", st.DigestInFlight)
	}
	if st.InFlight != 1 || len(st.Queued) != 2 || st.Queued[0] != 4 {
		t.Fatalf("other state keys lost: %+v", st)
	}

	// A state file with that key suppresses the digest at dispatch time.
	dir := t.TempDir()
	stPath := filepath.Join(dir, "state.json")
	cfgPath := filepath.Join(dir, "jobq.json")
	if err := os.WriteFile(stPath, []byte(`{"queued": [4], "digest_in_flight": true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte(`{"queue": {"max_parallel": 2}, "digest": {"min_backlog": 5}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if err := readJSON(stPath, &st); err != nil {
		t.Fatalf("readJSON state: %v", err)
	}
	if got := Dispatch(cfg, st); digestRuns(got) != 0 {
		t.Fatalf("digest_in_flight from file did not block the digest: %v", got)
	}
}
