package main

import (
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

// Digest config.

func TestDigestMinBacklogDefault(t *testing.T) {
	cfg, err := withDefaults(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Digest.MinBacklog != 2 {
		t.Fatalf("default MinBacklog = %d, want 2", cfg.Digest.MinBacklog)
	}
	// An explicit zero also becomes the default.
	cfg, err = loadConfig(write(t, `{"digest": {"min_backlog": 0}}`))
	if err != nil || cfg.Digest.MinBacklog != 2 {
		t.Fatalf("explicit zero: cfg=%+v err=%v", cfg.Digest, err)
	}
}

func TestDigestMinBacklogKept(t *testing.T) {
	cfg, err := loadConfig(write(t, `{"digest": {"min_backlog": 5}}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Digest.MinBacklog != 5 {
		t.Fatalf("MinBacklog = %d, want 5", cfg.Digest.MinBacklog)
	}
}

func TestDigestMinBacklogNegativeIsError(t *testing.T) {
	if _, err := withDefaults(Config{Digest: Digest{MinBacklog: -1}}); err == nil {
		t.Fatal("withDefaults accepted a negative min_backlog")
	}
	if _, err := loadConfig(write(t, `{"digest": {"min_backlog": -3}}`)); err == nil {
		t.Fatal("loadConfig accepted a negative min_backlog")
	}
}

// Digest dispatching.

func TestDispatchDigestInSpareSlot(t *testing.T) {
	cfg, _ := withDefaults(Config{}) // MaxParallel 2, MinBacklog 2
	got := Dispatch(cfg, State{})
	if len(got) != 1 || got[0]["digest"] != "true" {
		t.Fatalf("Dispatch = %v, want one digest run", got)
	}
}

func TestDispatchDigestOnlyWithFreeSlot(t *testing.T) {
	cfg, _ := withDefaults(Config{})
	got := Dispatch(cfg, State{Queued: []int{4, 5}, InFlight: 1}) // the single free slot goes to issue 4
	if len(got) != 1 || got[0]["digest"] != "" {
		t.Fatalf("Dispatch = %v, want no digest run once slots are full", got)
	}
}

func TestDispatchNeedsLowBacklog(t *testing.T) {
	cfg, _ := withDefaults(Config{}) // MinBacklog 2
	got := Dispatch(cfg, State{Queued: []int{4, 5}})
	if len(got) != 2 || got[0]["issue"] != "4" || got[1]["issue"] != "5" {
		t.Fatalf("Dispatch = %v, want only issues when backlog is at the minimum", got)
	}
}

func TestDispatchAtMostOneDigest(t *testing.T) {
	cfg, _ := withDefaults(Config{Queue: Queue{MaxParallel: 4}, Digest: Digest{MinBacklog: 4}})
	// too many free slots must not produce two digest runs
	got := Dispatch(cfg, State{})
	if len(got) != 1 || got[0]["digest"] != "true" {
		t.Fatalf("Dispatch = %v, want exactly one digest run", got)
	}
}

func TestDispatchNoDigestWhenOneInFlight(t *testing.T) {
	cfg, _ := withDefaults(Config{})
	got := Dispatch(cfg, State{DigestInFlight: true})
	if len(got) != 0 {
		t.Fatalf("Dispatch = %v, want no digest run while one is in flight", got)
	}
}

func TestDispatchIssuesStartedBeforeDigest(t *testing.T) {
	cfg, _ := withDefaults(Config{Queue: Queue{MaxParallel: 3}, Digest: Digest{MinBacklog: 2}})
	got := Dispatch(cfg, State{Queued: []int{4}})
	if len(got) != 2 || got[0]["issue"] != "4" || got[1]["digest"] != "true" {
		t.Fatalf("Dispatch = %v, want the issue first then one digest", got)
	}
}

func TestDispatchDigestFromJSONState(t *testing.T) {
	// The state flag round-trips through its JSON name.
	cfg, err := loadConfig(write(t, `{"digest": {"min_backlog": 2}}`))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte(`{"queued": [], "in_flight": 0, "digest_in_flight": true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var st State
	if err := readJSON(path, &st); err != nil {
		t.Fatal(err)
	}
	if !st.DigestInFlight {
		t.Fatal("digest_in_flight did not unmarshal into State.DigestInFlight")
	}
	if got := Dispatch(cfg, st); len(got) != 0 {
		t.Fatalf("Dispatch = %v, want nothing with a digest in flight", got)
	}
}

// Digest decisions.

func TestDecideDigest(t *testing.T) {
	d := Decide(Event{Inputs: map[string]string{"digest": "true"}})
	if !d.Run || d.Mode != "digest" || d.Number != 0 {
		t.Fatalf("Decide = %+v, want a digest run with no number", d)
	}
	if got, err := Execute(d); err != nil || got != "wrote digest" {
		t.Fatalf("Execute = %q, %v, want %q", got, err, "wrote digest")
	}
}

func TestDecideDigestMustBeTrue(t *testing.T) {
	for _, v := range []string{"false", "TRUE", "yes", "1"} {
		if d := Decide(Event{Inputs: map[string]string{"digest": v}}); d.Run {
			t.Fatalf("digest=%q ran: %+v", v, d)
		}
	}
}

func TestDecideDigestExclusiveWithIssueAndReview(t *testing.T) {
	for _, inputs := range []map[string]string{
		{"digest": "true", "issue": "7"},
		{"digest": "true", "review": "8"},
		{"digest": "true", "issue": "7", "review": "8"},
	} {
		if d := Decide(Event{Inputs: inputs}); d.Run {
			t.Fatalf("Decide ran a mixed dispatch: %+v", d)
		}
	}
}

func TestDecideIssueAndReviewUnchanged(t *testing.T) {
	d := Decide(Event{Inputs: map[string]string{"issue": "9"}})
	if !d.Run || d.Mode != "issue" || d.Number != 9 {
		t.Fatalf("Decide issue = %+v", d)
	}
	d = Decide(Event{Inputs: map[string]string{"review": "10"}})
	if !d.Run || d.Mode != "review" || d.Number != 10 {
		t.Fatalf("Decide review = %+v", d)
	}
	if got, err := Execute(Decision{Run: true, Mode: "issue", Number: 9}); err != nil || got != "worked issue #9" {
		t.Fatalf("Execute issue = %q, %v", got, err)
	}
}

func TestDecidePrintsDigestWithoutNumber(t *testing.T) {
	out := &strings.Builder{}
	if err := run([]string{"decide", write(t, `{"inputs": {"digest": "true"}}`)}, out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "mode=digest") || strings.Contains(got, "number=") {
		t.Fatalf("decide output = %q, want mode=digest and no number line", got)
	}

	out = &strings.Builder{}
	if err := run([]string{"decide", write(t, `{"inputs": {"issue": "42"}}`)}, out); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "mode=issue") || !strings.Contains(got, "number=42") {
		t.Fatalf("decide output = %q, want mode=issue and number=42", got)
	}
}

func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "in.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
