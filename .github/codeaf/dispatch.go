package main

// One source of truth for the workflow_dispatch inputs the queue and the
// run jobs send (codeaf-queue.yml, codeaf.yml) and for the title the
// run-name expression renders (codeaf.yml). The gate reads the inputs back
// in Decide, and the queue reads the titles back with runTitleFields and
// runPlanFields; dispatch_test.go pins the three sides together, so a
// dispatch the gate would refuse or a title the queue cannot parse fails
// CI instead of shipping (issue #61's planner did).

import (
	"fmt"
	"os"
	"sort"
	"strconv"
)

// dispatch is one workflow_dispatch of codeaf.yml. Kind is one of the five
// the gate reads: issue, pr, review, plan or routine. The number is the
// issue or PR the run acts on (0 for plan and routine, which take no
// target); the lane is the worker seat the queue assigned; the profile is
// set only for a model experiment on an issue labelled codeaf-experiment;
// the routine name is set only for a routine dispatch (issue #117), in
// place of the number.
type dispatch struct {
	Kind    string
	Number  int
	Lane    int
	Profile string
	Routine string
}

// dispatchKinds are the kinds the gate's workflow_dispatch branch reads.
var dispatchKinds = []string{"issue", "pr", "review", "plan", "routine"}

// newDispatch validates a dispatch the way Decide reads it: the kind is one
// of the five, and the three numeric kinds carry a positive number. Plan
// takes no target at all: its target is the repository itself, so no number
// and no profile. A routine dispatch (issue #117) takes a routine name
// instead of a number, validated against routineNameRe like routines.json
// on load. The gate refuses any issue, pr, review, plan or profile input
// beside routine=<name>, and any issue, pr, review, routine or profile
// input beside plan=true, and a combination it refuses must be unbuildable
// here, so inputs() can never emit one (the #61 failure mode).
func newDispatch(kind string, number, lane int, profile, routine string) (dispatch, error) {
	switch kind {
	case "issue", "pr", "review":
		switch {
		case number <= 0:
			return dispatch{}, fmt.Errorf("%s dispatch needs a positive number, got %d", kind, number)
		case routine != "":
			return dispatch{}, fmt.Errorf("a %s dispatch takes no routine input, got %q", kind, routine)
		}
	case "plan":
		switch {
		case number != 0:
			return dispatch{}, fmt.Errorf("a plan dispatch takes no number, got %d", number)
		case profile != "":
			return dispatch{}, fmt.Errorf("a plan dispatch takes no profile input, got %q", profile)
		case routine != "":
			return dispatch{}, fmt.Errorf("a plan dispatch takes no routine input, got %q", routine)
		}
	case "routine":
		switch {
		case number != 0:
			return dispatch{}, fmt.Errorf("a routine dispatch takes no number, got %d", number)
		case profile != "":
			return dispatch{}, fmt.Errorf("a routine dispatch takes no profile input, got %q", profile)
		case !routineNameRe.MatchString(routine):
			return dispatch{}, fmt.Errorf("a routine dispatch needs a name matching %s, got %q", routineNameRe, routine)
		}
	default:
		return dispatch{}, fmt.Errorf("dispatch kind %q is not one of issue, pr, review, plan, routine", kind)
	}
	return dispatch{Kind: kind, Number: number, Lane: lane, Profile: profile, Routine: routine}, nil
}

// inputs returns exactly the workflow_dispatch inputs the queue and the run
// jobs send for this dispatch: the number under the kind's own input name
// (plan sends the literal "true" instead, routine the routine name), the
// lane in decimal, and the profile only when one is set. The gate reads
// these same names in Decide.
func (d dispatch) inputs() map[string]string {
	in := map[string]string{"lane": strconv.Itoa(d.Lane)}
	switch d.Kind {
	case "plan":
		in["plan"] = "true"
	case "routine":
		in["routine"] = d.Routine
	default:
		in[d.Kind] = strconv.Itoa(d.Number)
	}
	if d.Profile != "" {
		in["profile"] = d.Profile
	}
	return in
}

// title returns the display title the run-name expression in codeaf.yml
// renders for this dispatch: "CodeAF #N", "CodeAF #N (lane L)" when a lane
// is set, "CodeAF plan (lane L)" and "CodeAF routine <name> (lane L)" — the
// expression falls back to lane 0 for a plan or routine run, so those
// titles always carry the suffix (a routine title must, so runRoutineRe
// parses every routine title, issue #117). runTitleFields, runPlanFields
// and runRoutineFields parse these shapes back; the test pins the round
// trip and the run-name line together.
func (d dispatch) title() string {
	if d.Kind == "plan" {
		return fmt.Sprintf("CodeAF plan (lane %d)", d.Lane)
	}
	if d.Kind == "routine" {
		return fmt.Sprintf("CodeAF routine %s (lane %d)", d.Routine, d.Lane)
	}
	if d.Lane > 0 {
		return fmt.Sprintf("CodeAF #%d (lane %d)", d.Number, d.Lane)
	}
	return fmt.Sprintf("CodeAF #%d", d.Number)
}

// runDispatchArgs implements `harness dispatch-args KIND NUMBER LANE
// [PROFILE]`: one `-f key=value` line per input, in sorted key order, the
// arguments a `gh workflow run codeaf.yml` needs for this dispatch. The
// workflows do not call it yet (issue #133 keeps them as they are); the
// command exists so the next change there can copy its output instead of
// re-deriving the input names by hand.
func runDispatchArgs() error {
	// Like the other commands, the arguments start after the command word.
	if len(os.Args) != 5 && len(os.Args) != 6 {
		return fmt.Errorf("usage: harness dispatch-args KIND NUMBER LANE [PROFILE]")
	}
	kind, numberArg, laneArg := os.Args[2], os.Args[3], os.Args[4]
	// A lane or number that is not a plain decimal, or that overflows an
	// int, is refused rather than silently coerced to 0: a caller copying
	// the printed lines would dispatch `-f lane=0`, which the gate reads as
	// no lane at all.
	parse := func(name, arg string) (int, error) {
		if !digitsOnly(arg) {
			return 0, fmt.Errorf("dispatch-args: %s must be a plain decimal number, got %q", name, arg)
		}
		n, err := strconv.Atoi(arg)
		if err != nil {
			return 0, fmt.Errorf("dispatch-args: %s %q overflows an int", name, arg)
		}
		return n, nil
	}
	lane, err := parse("lane", laneArg)
	if err != nil {
		return err
	}
	number, err := parse("number", numberArg)
	if err != nil {
		return err
	}
	profile := ""
	if len(os.Args) == 6 {
		profile = os.Args[5]
	}
	// dispatch-args does not build routine dispatches: their target is a
	// name, not a number, and the routines workflow prints its own inputs
	// from `routines-due` (issue #117).
	d, err := newDispatch(kind, number, lane, profile, "")
	if err != nil {
		return err
	}
	in := d.inputs()
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("-f %s=%s\n", k, in[k])
	}
	return nil
}
