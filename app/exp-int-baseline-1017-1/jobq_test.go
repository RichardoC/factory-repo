package main

import (
	"os"
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

func TestDigestMinBacklogDefault(t *testing.T) {
	cfg, err := withDefaults(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Digest.MinBacklog != 2 {
		t.Fatalf("MinBacklog default = %d, want 2", cfg.Digest.MinBacklog)
	}
}

func TestDigestMinBacklogNegative(t *testing.T) {
	if _, err := withDefaults(Config{Digest: Digest{MinBacklog: -1}}); err == nil {
		t.Fatal("negative min_backlog accepted")
	}
	f := t.TempDir() + "/jobq.json"
	os.WriteFile(f, []byte(`{"digest":{"min_backlog":-3}}`), 0o600)
	if _, err := loadConfig(f); err == nil {
		t.Fatal("loadConfig accepted negative min_backlog")
	}
}

func TestDispatchStartsDigestWhenIdleAndLowBacklog(t *testing.T) {
	cfg, _ := withDefaults(Config{})
	got := Dispatch(cfg, State{})
	if len(got) != 1 || got[0]["digest"] != "true" {
		t.Fatalf("Dispatch = %v, want one {digest:true}", got)
	}
}

func TestDispatchDigestOnlyInSpareSlot(t *testing.T) {
	cfg, _ := withDefaults(Config{})
	// Backlog lower than the digest threshold, but no slot left after
	// the queued issues start: no digest run.
	got := Dispatch(cfg, State{Queued: []int{1}, InFlight: 1})
	if len(got) != 1 || got[0]["digest"] != "" {
		t.Fatalf("Dispatch = %v, want only the queued issue", got)
	}
}

func TestDispatchNoDigestWhenBacklogAtThreshold(t *testing.T) {
	cfg, _ := withDefaults(Config{Digest: Digest{MinBacklog: 2}})
	got := Dispatch(cfg, State{Queued: []int{1, 2}})
	for _, in := range got {
		if in["digest"] != "" {
			t.Fatalf("digest started with full backlog: %v", got)
		}
	}
}

func TestDispatchNoDigestWhenBacklogHigh(t *testing.T) {
	cfg, _ := withDefaults(Config{})
	got := Dispatch(cfg, State{Queued: []int{1, 2}})
	for _, in := range got {
		if in["digest"] != "" {
			t.Fatalf("digest started with backlog >= MinBacklog: %v", got)
		}
	}
}

func TestDispatchNoDigestWhenDigestInFlight(t *testing.T) {
	cfg, _ := withDefaults(Config{})
	got := Dispatch(cfg, State{DigestInFlight: true})
	if len(got) != 0 {
		t.Fatalf("Dispatch = %v, want no digest while one is in flight", got)
	}
}

func TestDispatchDigestInputExactlyTrue(t *testing.T) {
	cfg, _ := withDefaults(Config{})
	got := Dispatch(cfg, State{})
	if len(got) != 1 || len(got[0]) != 1 || got[0]["digest"] != "true" {
		t.Fatalf("digest inputs = %v, want exactly {digest:true}", got)
	}
}

func TestDecideDigest(t *testing.T) {
	d := Decide(Event{Inputs: map[string]string{"digest": "true"}})
	if !d.Run || d.Mode != "digest" || d.Number != 0 {
		t.Fatalf("Decide = %+v", d)
	}
}

func TestDecideDigestRefusals(t *testing.T) {
	cases := []struct {
		name   string
		inputs map[string]string
	}{
		{"with issue", map[string]string{"digest": "true", "issue": "7"}},
		{"with review", map[string]string{"digest": "true", "review": "8"}},
		{"not exactly true", map[string]string{"digest": "1"}},
		{"yes", map[string]string{"digest": "yes"}},
	}
	for _, c := range cases {
		if d := Decide(Event{Inputs: c.inputs}); d.Run {
			t.Fatalf("%s: ran: %+v", c.name, d)
		}
	}
}

func TestDecideIssueAndReviewUnchanged(t *testing.T) {
	if d := Decide(Event{Inputs: map[string]string{"review": "8"}}); !d.Run || d.Mode != "review" || d.Number != 8 {
		t.Fatalf("Decide = %+v", d)
	}
	if d := Decide(Event{Inputs: map[string]string{}}); d.Run {
		t.Fatalf("empty event ran: %+v", d)
	}
}

func TestExecuteDigest(t *testing.T) {
	if got, err := Execute(Decision{Run: true, Mode: "digest"}); err != nil || got != "wrote digest" {
		t.Fatalf("Execute = %q, %v", got, err)
	}
}
