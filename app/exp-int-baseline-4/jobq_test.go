package main

import "testing"

func TestDispatchFillsFreeSlots(t *testing.T) {
	cfg, _ := withDefaults(Config{})
	got := Dispatch(cfg, State{Queued: []int{4, 5, 6}, InFlight: 1})
	if len(got) != 1 || got[0]["issue"] != "4" {
		t.Fatalf("Dispatch = %v", got)
	}
}

func TestDispatchDigestWhenBacklogBelowMin(t *testing.T) {
	cfg, _ := withDefaults(Config{}) // MinBacklog defaults to 2
	got := Dispatch(cfg, State{Queued: []int{4}, InFlight: 0})
	if len(got) != 2 || got[0]["issue"] != "4" || got[1]["digest"] != "true" {
		t.Fatalf("Dispatch = %v", got)
	}
}

func TestDispatchNoDigestWhenBacklogAtMin(t *testing.T) {
	cfg, _ := withDefaults(Config{})
	got := Dispatch(cfg, State{Queued: []int{4, 5}, InFlight: 0})
	if len(got) != 2 || got[1]["issue"] != "5" {
		t.Fatalf("Dispatch = %v", got)
	}
	for _, inputs := range got {
		if inputs["digest"] == "true" {
			t.Fatalf("digest started with backlog at MinBacklog: %v", got)
		}
	}
}

func TestDispatchNoDigestWhenNoSlotFree(t *testing.T) {
	cfg, _ := withDefaults(Config{})
	got := Dispatch(cfg, State{Queued: []int{4}, InFlight: 1})
	if len(got) != 1 || got[0]["issue"] != "4" {
		t.Fatalf("Dispatch = %v", got)
	}
	if got := Dispatch(cfg, State{Queued: nil, InFlight: 2}); len(got) != 0 {
		t.Fatalf("digest started with no free slot: %v", got)
	}
}

func TestDispatchNoDigestWhenDigestInFlight(t *testing.T) {
	cfg, _ := withDefaults(Config{})
	got := Dispatch(cfg, State{Queued: []int{4}, InFlight: 1, DigestInFlight: true})
	if len(got) != 1 || got[0]["issue"] != "4" {
		t.Fatalf("Dispatch = %v", got)
	}
}

func TestDispatchMinBacklogFromConfig(t *testing.T) {
	cfg, _ := withDefaults(Config{Digest: DigestConfig{MinBacklog: 3}, Queue: Queue{MaxParallel: 3}})
	got := Dispatch(cfg, State{Queued: []int{4, 5}, InFlight: 0})
	if len(got) != 3 || got[2]["digest"] != "true" {
		t.Fatalf("Dispatch = %v", got)
	}
}

func TestDispatchAtMostOneDigestPerTick(t *testing.T) {
	cfg, _ := withDefaults(Config{Queue: Queue{MaxParallel: 4}})
	got := Dispatch(cfg, State{Queued: []int{4}, InFlight: 0})
	digests := 0
	for _, inputs := range got {
		if inputs["digest"] == "true" {
			digests++
		}
	}
	if digests != 1 {
		t.Fatalf("Dispatch = %v, digests = %d", got, digests)
	}
}

func TestWithDefaultsDigest(t *testing.T) {
	cfg, err := withDefaults(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Digest.MinBacklog != 2 {
		t.Fatalf("MinBacklog = %d, want default 2", cfg.Digest.MinBacklog)
	}
	cfg, err = withDefaults(Config{Digest: DigestConfig{MinBacklog: 5}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Digest.MinBacklog != 5 {
		t.Fatalf("MinBacklog = %d, want 5", cfg.Digest.MinBacklog)
	}
	if _, err = withDefaults(Config{Digest: DigestConfig{MinBacklog: -1}}); err == nil {
		t.Fatal("negative MinBacklog accepted")
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
		t.Fatalf("Decide = %+v", d)
	}
	if d := Decide(Event{Inputs: map[string]string{"digest": "yes"}}); d.Run {
		t.Fatalf("digest=%q ran: %+v", "yes", d)
	}
	if d := Decide(Event{Inputs: map[string]string{"digest": "True"}}); d.Run {
		t.Fatalf("digest=%q ran: %+v", "True", d)
	}
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
