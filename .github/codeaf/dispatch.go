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

// dispatch is one workflow_dispatch of codeaf.yml. Kind is one of the four
// the gate reads: issue, pr, review or plan. The number is the issue or PR
// the run acts on (0 for plan, which takes no target); the lane is the
// worker seat the queue assigned; the profile is set only for a model
// experiment on an issue labelled codeaf-experiment.
type dispatch struct {
	Kind    string
	Number  int
	Lane    int
	Profile string
}

// dispatchKinds are the kinds the gate's workflow_dispatch branch reads.
var dispatchKinds = []string{"issue", "pr", "review", "plan"}

// newDispatch validates a dispatch the way Decide reads it: the kind is one
// of the four, and the three numeric kinds carry a positive number. Plan
// takes no number: its target is the repository itself.
func newDispatch(kind string, number, lane int, profile string) (dispatch, error) {
	switch kind {
	case "issue", "pr", "review":
		if number <= 0 {
			return dispatch{}, fmt.Errorf("%s dispatch needs a positive number, got %d", kind, number)
		}
	case "plan":
		if number != 0 {
			return dispatch{}, fmt.Errorf("a plan dispatch takes no number, got %d", number)
		}
	default:
		return dispatch{}, fmt.Errorf("dispatch kind %q is not one of issue, pr, review, plan", kind)
	}
	return dispatch{Kind: kind, Number: number, Lane: lane, Profile: profile}, nil
}

// inputs returns exactly the workflow_dispatch inputs the queue and the run
// jobs send for this dispatch: the number under the kind's own input name
// (plan sends the literal "true" instead), the lane in decimal, and the
// profile only when one is set. The gate reads these same names in Decide.
func (d dispatch) inputs() map[string]string {
	in := map[string]string{"lane": strconv.Itoa(d.Lane)}
	switch d.Kind {
	case "plan":
		in["plan"] = "true"
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
// is set, and "CodeAF plan (lane L)" — the expression falls back to lane 0
// for a plan run, so a plan title always carries the suffix. runTitleFields
// and runPlanFields in queue.go parse these shapes back; the test pins the
// round trip and the run-name line together.
func (d dispatch) title() string {
	if d.Kind == "plan" {
		return fmt.Sprintf("CodeAF plan (lane %d)", d.Lane)
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
	lane := 0
	if digitsOnly(laneArg) {
		if n, err := strconv.Atoi(laneArg); err == nil {
			lane = n
		}
	}
	number := 0
	if digitsOnly(numberArg) {
		if n, err := strconv.Atoi(numberArg); err == nil {
			number = n
		}
	}
	profile := ""
	if len(os.Args) == 6 {
		profile = os.Args[5]
	}
	d, err := newDispatch(kind, number, lane, profile)
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
