package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// digestInputs is the one input a digest run may carry.
func isDigest(in map[string]string) bool {
	return len(in) == 1 && in["digest"] == "true"
}

// TestDispatchStartsDigestWhenBacklogLowAndSlotFree covers a low backlog with a
// slot left over: one digest run, after the issue inputs.
func TestDispatchStartsDigestWhenBacklogLowAndSlotFree(t *testing.T) {
	cfg, err := withDefaults(Config{Queue: Queue{MaxParallel: 3}, Digest: Digest{MinBacklog: 2}})
	if err != nil {
		t.Fatalf("withDefaults: %v", err)
	}
	got := Dispatch(cfg, State{Queued: []int{4}})
	if len(got) != 2 {
		t.Fatalf("Dispatch = %v, want issue then digest", got)
	}
	if got[0]["issue"] != "4" || len(got[0]) != 1 {
		t.Fatalf("first input = %v, want only the issue", got[0])
	}
	if !isDigest(got[1]) {
		t.Fatalf("second input = %v, want exactly {\"digest\":\"true\"}", got[1])
	}
	// An empty queue is below the minimum too: just the digest.
	got = Dispatch(cfg, State{})
	if len(got) != 1 || !isDigest(got[0]) {
		t.Fatalf("Dispatch(empty) = %v, want one digest", got)
	}
	// At most one digest per tick, even with several slots left free.
	got = Dispatch(cfg, State{})
	if len(got) != 1 {
		t.Fatalf("Dispatch = %v, want at most one digest", got)
	}
}

// TestDispatchNoDigestWhenQueuedAtMinBacklog covers queued count >= MinBacklog,
// while slots are still free.
func TestDispatchNoDigestWhenQueuedAtMinBacklog(t *testing.T) {
	cfg, err := withDefaults(Config{Queue: Queue{MaxParallel: 4}, Digest: Digest{MinBacklog: 2}})
	if err != nil {
		t.Fatalf("withDefaults: %v", err)
	}
	for _, queued := range [][]int{{1, 2}, {1, 2, 3}} {
		for _, in := range Dispatch(cfg, State{Queued: queued}) {
			if _, ok := in["digest"]; ok {
				t.Fatalf("digest started with queued %v: %v", queued, in)
			}
		}
	}
}

// TestDispatchNoDigestWhenNoFreeSlotAfterIssues covers a free slot that the
// queued issues consume.
func TestDispatchNoDigestWhenNoFreeSlotAfterIssues(t *testing.T) {
	cfg, err := withDefaults(Config{Queue: Queue{MaxParallel: 1}, Digest: Digest{MinBacklog: 2}})
	if err != nil {
		t.Fatalf("withDefaults: %v", err)
	}
	got := Dispatch(cfg, State{Queued: []int{9}})
	if len(got) != 1 || got[0]["issue"] != "9" {
		t.Fatalf("Dispatch = %v, want only the issue", got)
	}
}

// TestDispatchNoDigestWhenInFlight covers State.DigestInFlight.
func TestDispatchNoDigestWhenInFlight(t *testing.T) {
	cfg, err := withDefaults(Config{Queue: Queue{MaxParallel: 3}, Digest: Digest{MinBacklog: 2}})
	if err != nil {
		t.Fatalf("withDefaults: %v", err)
	}
	for _, got := range [][]map[string]string{
		Dispatch(cfg, State{DigestInFlight: true}),
		Dispatch(cfg, State{Queued: []int{4}, DigestInFlight: true}),
	} {
		for _, in := range got {
			if _, ok := in["digest"]; ok {
				t.Fatalf("digest started while one is in flight: %v", got)
			}
		}
	}
}

// TestDispatchNoDigestWhenExactlyFull covers a tick with no free slot at all.
func TestDispatchNoDigestWhenExactlyFull(t *testing.T) {
	cfg, err := withDefaults(Config{Queue: Queue{MaxParallel: 2}, Digest: Digest{MinBacklog: 2}})
	if err != nil {
		t.Fatalf("withDefaults: %v", err)
	}
	if got := Dispatch(cfg, State{InFlight: 2}); len(got) != 0 {
		t.Fatalf("Dispatch = %v, want nothing", got)
	}
	if got := Dispatch(cfg, State{Queued: []int{1, 2}, InFlight: 0}); len(got) != 2 {
		t.Fatalf("Dispatch = %v, want two issues and no digest", got)
	}
}

// TestDispatchTestStateDigestInFlightTag proves the digest_in_flight JSON tag.
func TestDispatchTestStateDigestInFlightTag(t *testing.T) {
	var st State
	if err := json.Unmarshal([]byte(`{"digest_in_flight":true}`), &st); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !st.DigestInFlight {
		t.Fatalf("State = %+v, want DigestInFlight true", st)
	}
	b, err := json.Marshal(State{DigestInFlight: true})
	if err != nil || !strings.Contains(string(b), `"digest_in_flight":true`) {
		t.Fatalf("Marshal = %s, %v", b, err)
	}
}
