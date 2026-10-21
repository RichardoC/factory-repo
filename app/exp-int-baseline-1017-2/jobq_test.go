package main

import (
	"encoding/json"
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

func TestDispatchDigestWhenBacklogLow(t *testing.T) {
	cfg, _ := withDefaults(Config{})
	got := Dispatch(cfg, State{Queued: []int{4}, InFlight: 0})
	want := []map[string]string{{"issue": "4"}, {"digest": "true"}}
	if len(got) != len(want) {
		t.Fatalf("Dispatch = %v, want %v", got, want)
	}
	for i, m := range want {
		if len(got[i]) != len(m) || got[i]["issue"] != m["issue"] || got[i]["digest"] != m["digest"] {
			t.Fatalf("Dispatch[%d] = %v, want %v", i, got[i], m)
		}
	}
}

func TestDispatchNoDigestWhenSlotsTaken(t *testing.T) {
	cfg, _ := withDefaults(Config{}) // max_parallel 2
	got := Dispatch(cfg, State{Queued: []int{4, 5}, InFlight: 0})
	if len(got) != 2 || got[1]["digest"] != "" {
		t.Fatalf("Dispatch = %v; digest must not steal a queued issue's slot", got)
	}
}

func TestDispatchNoDigestWhenBacklogHigh(t *testing.T) {
	cfg, _ := withDefaults(Config{}) // min_backlog 2
	got := Dispatch(cfg, State{Queued: []int{4, 5}, InFlight: 1})
	if len(got) != 1 || got[0]["digest"] != "" {
		t.Fatalf("Dispatch = %v; backlog of %d meets min_backlog 2", got, len(got[0]))
	}
}

func TestDispatchNoDigestWhenOneInProgress(t *testing.T) {
	cfg, _ := withDefaults(Config{})
	got := Dispatch(cfg, State{Queued: nil, InFlight: 1, DigestInFlight: true})
	if len(got) != 0 {
		t.Fatalf("Dispatch = %v; digest already in flight", got)
	}
}

func TestDigestMinBacklogDefaultsToTwo(t *testing.T) {
	cfg, err := withDefaults(Config{})
	if err != nil || cfg.Digest.MinBacklog != 2 {
		t.Fatalf("withDefaults() = %+v, %v; want min_backlog 2", cfg.Digest, err)
	}
}

func TestDigestNegativeMinBacklogIsError(t *testing.T) {
	if _, err := withDefaults(Config{Digest: Digest{MinBacklog: -1}}); err == nil {
		t.Fatal("withDefaults(negative min_backlog) = nil error")
	}
}

func TestDecideDigest(t *testing.T) {
	d := Decide(Event{Inputs: map[string]string{"digest": "true"}})
	if !d.Run || d.Mode != "digest" || d.Number != 0 {
		t.Fatalf("Decide = %+v; want digest run with no number", d)
	}
	if d := Decide(Event{Inputs: map[string]string{"digest": "yes"}}); d.Run {
		t.Fatalf("digest=%q ran: %+v; digest must be exactly true", "yes", d)
	}
	if d := Decide(Event{Inputs: map[string]string{"digest": "true", "issue": "7"}}); d.Run {
		t.Fatalf("digest with issue ran: %+v", d)
	}
	if d := Decide(Event{Inputs: map[string]string{"digest": "true", "review": "8"}}); d.Run {
		t.Fatalf("digest with review ran: %+v", d)
	}
}

func TestExecuteDigest(t *testing.T) {
	got, err := Execute(Decision{Run: true, Mode: "digest"})
	if err != nil || got != "wrote digest" {
		t.Fatalf("Execute = %q, %v", got, err)
	}
}

func TestStateJSONDigestInFlight(t *testing.T) {
	var st State
	data := []byte(`{"queued":[4],"in_flight":1,"digest_in_flight":true}`)
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !st.DigestInFlight || st.InFlight != 1 || len(st.Queued) != 1 {
		t.Fatalf("State = %+v; digest_in_flight must decode from its JSON name", st)
	}
	cfg, _ := withDefaults(Config{})
	got := Dispatch(cfg, st)
	if len(got) != 1 || got[0]["issue"] != "4" {
		t.Fatalf("Dispatch = %v; want only queued issue 4", got)
	}
}
