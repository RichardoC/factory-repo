package main

import "testing"

func TestDispatchFillsFreeSlots(t *testing.T) {
	cfg, _ := withDefaults(Config{})
	got := Dispatch(cfg, State{Queued: []int{4, 5, 6}, InFlight: 1})
	if len(got) != 1 || got[0]["issue"] != "4" {
		t.Fatalf("Dispatch = %v", got)
	}
}

func TestDispatchStartsDigestOnSpareCapacity(t *testing.T) {
	cfg, _ := withDefaults(Config{}) // MaxParallel 2, MinBacklog 2
	got := Dispatch(cfg, State{Queued: []int{4}, InFlight: 0})
	if len(got) != 2 || got[0]["issue"] != "4" || got[1]["digest"] != "true" {
		t.Fatalf("Dispatch = %v, want issue then digest", got)
	}
}

func TestDispatchNoDigestWhenBacklogMeetsMinBacklog(t *testing.T) {
	cfg, _ := withDefaults(Config{}) // MinBacklog 2
	got := Dispatch(cfg, State{Queued: []int{4, 5}})
	for _, inputs := range got {
		if inputs["digest"] == "true" {
			t.Fatalf("digest started with backlog at MinBacklog: %v", got)
		}
	}
}

func TestDispatchNoDigestWhenNoSlotFree(t *testing.T) {
	cfg, _ := withDefaults(Config{}) // MaxParallel 2
	got := Dispatch(cfg, State{Queued: []int{4, 5, 6}, InFlight: 0})
	if len(got) != 2 {
		t.Fatalf("Dispatch = %v, want only the two slot-filling runs", got)
	}
	for _, inputs := range got {
		if inputs["digest"] == "true" {
			t.Fatalf("digest started without a free slot: %v", got)
		}
	}
}

func TestDispatchNoDigestWhenDigestInFlight(t *testing.T) {
	cfg, _ := withDefaults(Config{})
	got := Dispatch(cfg, State{Queued: []int{4}, InFlight: 1, DigestInFlight: true})
	if len(got) != 1 || got[0]["digest"] == "true" {
		t.Fatalf("second digest started: %v", got)
	}
}

func TestDispatchDigestHonoursMinBacklogConfig(t *testing.T) {
	cfg, err := withDefaults(Config{Digest: Digest{MinBacklog: 1}})
	if err != nil {
		t.Fatal(err)
	}
	// MinBacklog 1 means digest only when fewer than 1 issue is queued, so a
	// backlog of 2 gets no digest, even with a slot left over.
	got := Dispatch(cfg, State{Queued: []int{4, 5}, InFlight: 1})
	if len(got) != 1 || got[0]["digest"] == "true" {
		t.Fatalf("Dispatch = %v, want only the queued issue", got)
	}
	// One free slot, backlog 1 >= MinBacklog 1: no digest.
	if got := Dispatch(cfg, State{Queued: []int{4}, InFlight: 1}); len(got) != 1 {
		t.Fatalf("Dispatch = %v, want only the queued issue", got)
	}
	// Backlog 0 < MinBacklog 1: digest starts.
	got = Dispatch(cfg, State{InFlight: 1})
	if len(got) != 1 || got[0]["digest"] != "true" {
		t.Fatalf("Dispatch = %v, want digest", got)
	}
}

func TestAtMostOneDigestPerTick(t *testing.T) {
	cfg, _ := withDefaults(Config{}) // MaxParallel 4, MinBacklog 2
	got := Dispatch(cfg, State{Queued: []int{4}})
	digests := 0
	for _, inputs := range got {
		if inputs["digest"] == "true" {
			digests++
		}
	}
	if digests > 1 {
		t.Fatalf("Dispatch = %v, want at most one digest run", got)
	}
}

func TestDigestDefaultsToMinBacklog2(t *testing.T) {
	cfg, err := withDefaults(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Digest.MinBacklog != 2 {
		t.Fatalf("MinBacklog = %d, want default 2", cfg.Digest.MinBacklog)
	}
	cfg, err = withDefaults(Config{Digest: Digest{MinBacklog: 0}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Digest.MinBacklog != 2 {
		t.Fatalf("MinBacklog = %d, want 2 for explicit 0", cfg.Digest.MinBacklog)
	}
}

func TestNegativeMinBacklogIsError(t *testing.T) {
	if _, err := withDefaults(Config{Digest: Digest{MinBacklog: -1}}); err == nil {
		t.Fatal("negative digest.min_backlog accepted")
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

func TestDecideDigest(t *testing.T) {
	d := Decide(Event{Inputs: map[string]string{"digest": "true"}})
	if !d.Run || d.Mode != "digest" || d.Number != 0 {
		t.Fatalf("Decide = %+v, want a digest run with no number", d)
	}
}

func TestDecideDigestRejectsNonTrue(t *testing.T) {
	for _, v := range []string{"True", "yes", "1", "false", "true "} {
		if d := Decide(Event{Inputs: map[string]string{"digest": v}}); d.Run {
			t.Fatalf("digest=%q ran: %+v", v, d)
		}
	}
}

func TestDecideDigestWithIssueOrReviewRefused(t *testing.T) {
	if d := Decide(Event{Inputs: map[string]string{"digest": "true", "issue": "7"}}); d.Run {
		t.Fatalf("digest+issue ran: %+v", d)
	}
	if d := Decide(Event{Inputs: map[string]string{"digest": "true", "review": "8"}}); d.Run {
		t.Fatalf("digest+review ran: %+v", d)
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
