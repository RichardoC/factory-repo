package main

// TestDecideRoutine pins the gate's routine branch (issue #117): a
// routine=<name> workflow_dispatch runs in mode routine only for a name
// that matches routineNameRe and is present in the committed routines.json,
// never beside another target input, and never when routines.json cannot be
// loaded (fail closed). The dispatch side of the same contract — that
// newDispatch cannot build a combination the gate refuses, and that title()
// and inputs() round trip — is pinned in dispatch_test.go and, through the
// binary, in contract_test.go.

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestDecideRoutine(t *testing.T) {
	t.Setenv("GITHUB_REPOSITORY", "RichardoC/factory-repo")
	t.Setenv("GITHUB_RUN_ID", "42")
	lookup := func(n int) (item, error) {
		t.Errorf("routine dispatch looked up PR %d", n)
		return item{}, nil
	}
	lookupIssue := func(n int) (item, error) {
		t.Errorf("routine dispatch looked up issue %d", n)
		return item{}, nil
	}
	decide := func(t *testing.T, inputs map[string]string) Decision {
		t.Helper()
		ev := event{Inputs: inputs}
		ev.Repository.DefaultBranch = "main"
		got, err := Decide("workflow_dispatch", ev, "codeaf", lookup, lookupIssue)
		if err != nil {
			t.Fatalf("Decide(%v): %v", inputs, err)
		}
		return got
	}

	// The name comes from the committed routines.json, the same file the
	// gate validates against: model-prices ships with issue #117.
	routines, err := LoadRoutines()
	if err != nil {
		t.Fatalf("LoadRoutines: %v", err)
	}
	name := "model-prices"
	known := false
	for _, r := range routines {
		if r.Name == name {
			known = true
		}
	}
	if !known {
		t.Fatalf("committed routines.json has no %q routine; update this test and dispatch-routine.json", name)
	}

	t.Run("known name runs", func(t *testing.T) {
		// The inputs are exactly what dispatch.inputs() emits, built with
		// newDispatch: the round trip the contract test pins through the
		// binary.
		const lane = 2
		d, err := newDispatch("routine", 0, lane, "", name)
		if err != nil {
			t.Fatalf("newDispatch(routine): %v", err)
		}
		got := decide(t, d.inputs())
		if !got.Run || got.Mode != "routine" || got.Reason != "routine dispatch" {
			t.Fatalf("Decide(%v) = %+v; want a routine dispatch", d.inputs(), got)
		}
		// No number, no branch, no profile: the run is read-only and acts
		// on no issue or PR, and every workflow step reading one must see
		// it empty.
		if got.Number != 0 || got.Branch != "" || got.Profile != "" {
			t.Fatalf("routine decision carries number=%d branch=%q profile=%q; want all empty", got.Number, got.Branch, got.Profile)
		}
		if got.Base != "main" || got.Lane != lane {
			t.Fatalf("routine decision base=%q lane=%d; want main, %d", got.Base, got.Lane, lane)
		}
		// The title parses back through the queue's own routine parser,
		// with the lane suffix always present.
		routine, titleLane, ok := runRoutineFields(d.title())
		if !ok || routine != name || titleLane != lane {
			t.Fatalf("runRoutineFields(%q) = (%q, %d, %v); want (%q, %d)", d.title(), routine, titleLane, ok, name, lane)
		}
	})

	t.Run("unknown name skipped", func(t *testing.T) {
		got := decide(t, map[string]string{"lane": "0", "routine": "no-such-routine"})
		if got.Run {
			t.Fatalf("Decide ran an unknown routine: %+v", got)
		}
		if !strings.Contains(got.Reason, "no-such-routine") || !strings.Contains(got.Reason, "routines.json") {
			t.Fatalf("skip reason = %q; want it to name the routine and routines.json", got.Reason)
		}
	})

	t.Run("bad name skipped", func(t *testing.T) {
		for _, bad := range []string{"Model_Prices", "has spaces", "../../etc"} {
			got := decide(t, map[string]string{"lane": "0", "routine": bad})
			if got.Run {
				t.Fatalf("Decide ran routine input %q: %+v", bad, got)
			}
			if !strings.Contains(got.Reason, "does not match") {
				t.Fatalf("routine %q skip reason = %q; want the routineNameRe refusal", bad, got.Reason)
			}
		}
	})

	// Every target input beside routine=<name> is refused, named: the run
	// is never silently demoted to another mode (the #61 failure mode).
	t.Run("combined with a target skipped", func(t *testing.T) {
		for _, other := range []string{"issue", "pr", "review", "plan", "profile"} {
			value := "3"
			if other == "plan" {
				value = "true"
			}
			inputs := map[string]string{"lane": "0", "routine": name, other: value}
			got := decide(t, inputs)
			if got.Run {
				t.Fatalf("Decide(%v) ran: %+v", inputs, got)
			}
			if !strings.Contains(got.Reason, "a routine dispatch takes no") {
				t.Fatalf("Decide(%v) reason = %q; want the routine-input refusal", inputs, got.Reason)
			}
		}
	})

	// Fail closed: without routines.json no routine run is decided, and
	// the reason says so. The override follows PROXY_ROUTINES
	// (routines.go), like PROXY_MODELS.
	t.Run("routines.json unreadable skipped", func(t *testing.T) {
		t.Setenv("PROXY_ROUTINES", filepath.Join(t.TempDir(), "absent-routines.json"))
		got := decide(t, map[string]string{"lane": "0", "routine": name})
		if got.Run {
			t.Fatalf("Decide ran a routine with no routines.json: %+v", got)
		}
		if !strings.Contains(got.Reason, "routines.json cannot be loaded") {
			t.Fatalf("skip reason = %q; want the fail-closed routines.json refusal", got.Reason)
		}
	})
}
