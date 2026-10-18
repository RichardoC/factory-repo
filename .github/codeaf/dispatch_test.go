package main

// The dispatch tests pin the three sides of a workflow_dispatch together:
// the inputs the workflows send (checked against the workflow files as
// text), the inputs the gate reads in Decide, and the title the run-name
// expression renders and the queue parses back (issue #133).

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// dispatchCases are one dispatch per kind, plus the shapes that carry a
// lane or a profile: the queue always sends a lane, and a profile goes
// only with an issue labelled codeaf-experiment.
var dispatchCases = []struct {
	name    string
	kind    string
	number  int
	lane    int
	profile string
}{
	{"issue", "issue", 3, 0, ""},
	{"issue lane", "issue", 133, 2, ""},
	{"pr", "pr", 7, 1, ""},
	{"review", "review", 7, 3, ""},
	{"plan", "plan", 0, 0, ""},
	{"plan lane", "plan", 0, 2, ""},
	{"issue profile", "issue", 5, 1, "baseline"},
}

// newDispatchError checks newDispatch accepts what it should and returns
// the dispatch.
func dispatchOf(t *testing.T, kind string, number, lane int, profile string) dispatch {
	t.Helper()
	d, err := newDispatch(kind, number, lane, profile)
	if err != nil {
		t.Fatalf("newDispatch(%s, %d, %d, %q): %v", kind, number, lane, profile, err)
	}
	return d
}

// TestDispatchRoundTrip: every dispatch's inputs, fed back through Decide
// as a workflow_dispatch event, decide a run in the dispatch's own mode
// with the same number and lane. The lookups return open, labelled,
// same-repo items, so the only thing under test is the input round trip.
func TestDispatchRoundTrip(t *testing.T) {
	t.Setenv("GITHUB_REPOSITORY", "RichardoC/factory-repo")
	t.Setenv("GITHUB_RUN_ID", "42")
	labelled := []label{{Name: "codeaf"}}
	lookup := func(n int) (item, error) {
		pr := item{Number: n, State: "open", Labels: labelled}
		pr.Head.Ref = "codeaf/issue-3"
		pr.Head.Repo = &struct {
			FullName string `json:"full_name"`
		}{FullName: "RichardoC/factory-repo"}
		pr.Base.Ref = "main"
		return pr, nil
	}
	lookupIssue := func(n int) (item, error) {
		it := item{Number: n, State: "open", Labels: labelled}
		return it, nil
	}

	for _, c := range dispatchCases {
		t.Run(c.name, func(t *testing.T) {
			// A profile is only read on an issue labelled codeaf-experiment,
			// and experimentProfile reads experiments.json from HARNESS_DIR
			// (main.go), so the case points it at the package directory.
			issues := func(n int) (item, error) {
				it, _ := lookupIssue(n)
				if c.profile != "" {
					it.Labels = append(it.Labels, label{Name: experimentLabel})
				}
				return it, nil
			}
			if c.profile != "" {
				t.Setenv("HARNESS_DIR", ".")
			}
			d := dispatchOf(t, c.kind, c.number, c.lane, c.profile)
			ev := event{Inputs: d.inputs()}
			ev.Repository.DefaultBranch = "main"
			got, err := Decide("workflow_dispatch", ev, "codeaf", lookup, issues)
			if err != nil {
				t.Fatalf("Decide(%v): %v", d.inputs(), err)
			}
			if !got.Run {
				t.Fatalf("Decide(%v) refused the run: %+v", d.inputs(), got)
			}
			if got.Mode != c.kind {
				t.Fatalf("Decide(%v) mode = %q; want %q", d.inputs(), got.Mode, c.kind)
			}
			if got.Number != c.number {
				t.Fatalf("Decide(%v) number = %d; want %d", d.inputs(), got.Number, c.number)
			}
			if got.Lane != c.lane {
				t.Fatalf("Decide(%v) lane = %d; want %d", d.inputs(), got.Lane, c.lane)
			}
			if got.Profile != c.profile {
				t.Fatalf("Decide(%v) profile = %q; want %q", d.inputs(), got.Profile, c.profile)
			}
		})
	}

	// Every combination the gate refuses beside plan=true decides no run,
	// with the gates own reason spelled out: the inputs such a dispatch
	// would send are refused, not silently demoted to another mode.
	t.Run("refused combinations", func(t *testing.T) {
		for _, c := range refusedDispatchCases {
			t.Run(c.name, func(t *testing.T) {
				ev := event{Inputs: c.inputs}
				ev.Repository.DefaultBranch = "main"
				got, err := Decide("workflow_dispatch", ev, "codeaf", lookup, lookupIssue)
				if err != nil {
					t.Fatalf("Decide(%v): %v", c.inputs, err)
				}
				if got.Run {
					t.Fatalf("Decide(%v) ran a dispatch the gate refuses: %+v", c.inputs, got)
				}
				if !strings.Contains(got.Reason, "a plan dispatch takes no") {
					t.Fatalf("Decide(%v) reason = %q; want the plan-input refusal", c.inputs, got.Reason)
				}
			})
		}
	})
}

// TestDispatchInvalid: an unknown kind, a non-positive number on the three
// numeric kinds, and every input combination the gate refuses beside
// plan=true are errors — newDispatch cannot build a dispatch the gate
// would reject.
func TestDispatchInvalid(t *testing.T) {
	for _, c := range []struct {
		name   string
		kind   string
		number int
	}{
		{"unknown kind", "planner", 3},
		{"issue zero", "issue", 0},
		{"issue negative", "issue", -1},
		{"pr zero", "pr", 0},
		{"review zero", "review", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := newDispatch(c.kind, c.number, 0, ""); err == nil {
				t.Fatalf("newDispatch(%s, %d) accepted", c.kind, c.number)
			}
		})
	}
	for _, c := range refusedDispatchCases {
		t.Run("refused "+c.name, func(t *testing.T) {
			if _, err := newDispatch(c.kind, c.number, c.lane, c.profile); err == nil {
				t.Fatalf("newDispatch(%s, %d, %d, %q) accepted a dispatch the gate refuses", c.kind, c.number, c.lane, c.profile)
			}
		})
	}
	if _, err := newDispatch("plan", 0, 2, ""); err != nil {
		t.Fatalf("newDispatch(plan, 0, 2): %v", err)
	}
}

// TestTitlesRoundTrip: every dispatch's title parses back to the same
// number and lane through the queue's own parsers, runTitleFields and
// runPlanFields — the shapes the run-name expression renders and the queue
// reads out of the runs list.
func TestTitlesRoundTrip(t *testing.T) {
	for _, c := range dispatchCases {
		t.Run(c.name, func(t *testing.T) {
			title := dispatchOf(t, c.kind, c.number, c.lane, c.profile).title()
			if c.kind == "plan" {
				lane, ok := runPlanFields(title)
				if !ok || lane != c.lane {
					t.Fatalf("runPlanFields(%q) = (%d, %v); want lane %d", title, lane, ok, c.lane)
				}
				return
			}
			number, lane := runTitleFields(title)
			if number != c.number || lane != c.lane {
				t.Fatalf("runTitleFields(%q) = (%d, %d); want (%d, %d)", title, number, lane, c.number, c.lane)
			}
		})
	}
}

// gateInputs are the workflow_dispatch input names the gate reads: one per
// dispatch kind (dispatchKinds in dispatch.go), plus the lane and profile
// inputs any kind may carry.
var gateInputs = func() map[string]bool {
	in := map[string]bool{"lane": true, "profile": true}
	for _, k := range dispatchKinds {
		in[k] = true
	}
	return in
}()

// refusedDispatchCases are the input combinations the gate refuses beside
// plan=true: any target or profile input. newDispatch refuses the same
// combinations, so a caller cannot build a dispatch whose inputs() the gate
// would reject — the #61 failure mode. inputs is the raw map such a
// workflow_dispatch would send, for the Decide side of the assertion; for
// the plan-with-target cases the struct analogue is a plan carrying a
// number, which newDispatch refuses with it.
var refusedDispatchCases = []struct {
	name    string
	kind    string
	number  int
	lane    int
	profile string
	inputs  map[string]string
}{
	{"plan profile", "plan", 0, 0, "baseline",
		map[string]string{"lane": "0", "plan": "true", "profile": "baseline"}},
	{"plan issue", "plan", 3, 0, "",
		map[string]string{"lane": "0", "plan": "true", "issue": "3"}},
	{"plan pr", "plan", 7, 0, "",
		map[string]string{"lane": "0", "plan": "true", "pr": "7"}},
	{"plan review", "plan", 7, 0, "",
		map[string]string{"lane": "0", "plan": "true", "review": "7"}},
}

// dispatchFlag extracts the name from one `-f name=` or `-f "name=` flag.
var dispatchFlag = regexp.MustCompile(`-f "?([A-Za-z][A-Za-z0-9_-]*)=`)

// endsWithContinuation reports whether a shell line continues on the next
// physical line: it ends in an odd number of backslashes (an even count is
// escaped backslashes, not a continuation).
func endsWithContinuation(line string) bool {
	t := strings.TrimRight(line, " \t\r")
	n := 0
	for i := len(t) - 1; i >= 0 && t[i] == '\\'; i-- {
		n++
	}
	return n%2 == 1
}

// joinedLine is one logical shell line: the physical lines a trailing
// backslash joined, numbered by the physical line it starts on.
type joinedLine struct {
	start int
	text  string
}

// joinContinuations joins backslash-continued shell lines. A `-f` flag may
// sit on a continuation line under the `gh workflow run` command itself
// (the review dispatch in codeaf.yml does), and only the joined command
// shows every input it sends.
func joinContinuations(lines []string) []joinedLine {
	var out []joinedLine
	open := false
	for i, line := range lines {
		if !open {
			out = append(out, joinedLine{start: i + 1})
		}
		last := &out[len(out)-1]
		if open {
			last.text += " " + strings.TrimSpace(line)
		} else {
			last.text = line
		}
		if open = endsWithContinuation(line); open {
			t := strings.TrimRight(last.text, " \t\r")
			last.text = t[:len(t)-1]
		}
	}
	return out
}

// checkWorkflowsSendWhatTheGateReads walks both workflow files and refuses
// any dispatch flag the gate does not read, and any run-name line that has
// drifted from the title shapes title() mirrors. The harness is
// stdlib-only, so the YAML is read as text, with shell continuations
// joined first so a flag on a continuation line is still seen.
func checkWorkflowsSendWhatTheGateReads(queuePath, codeafPath string) error {
	checkDispatch := func(path string) error {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, line := range joinContinuations(strings.Split(string(data), "\n")) {
			if !strings.Contains(line.text, "gh workflow run codeaf.yml") {
				continue
			}
			for _, m := range dispatchFlag.FindAllStringSubmatch(line.text, -1) {
				if !gateInputs[m[1]] {
					return fmt.Errorf("%s:%d: dispatch sends -f %s=, which the gate does not read", path, line.start, m[1])
				}
			}
		}
		return nil
	}
	if err := checkDispatch(queuePath); err != nil {
		return err
	}
	if err := checkDispatch(codeafPath); err != nil {
		return err
	}
	// The run-name expression renders the titles title() mirrors; the
	// literals are format strings, so they are checked as substrings.
	data, err := os.ReadFile(codeafPath)
	if err != nil {
		return err
	}
	for i, line := range strings.Split(string(data), "\n") {
		if !strings.Contains(line, "run-name:") {
			continue
		}
		for _, want := range []string{"CodeAF plan (lane {0})", "CodeAF #{0}{1}", " (lane {0})"} {
			if !strings.Contains(line, want) {
				return fmt.Errorf("%s:%d: run-name no longer renders %q", codeafPath, i+1, want)
			}
		}
		return nil
	}
	return fmt.Errorf("%s: no run-name line", codeafPath)
}

// TestWorkflowsSendWhatTheGateReads: every `-f name=` the queue and the run
// job send is an input the gate reads, and the run-name line still renders
// the title shapes title() mirrors. A renamed input or a changed title
// shape fails with the workflow line named.
func TestWorkflowsSendWhatTheGateReads(t *testing.T) {
	queuePath := filepath.Join("..", "workflows", "codeaf-queue.yml")
	codeafPath := filepath.Join("..", "workflows", "codeaf.yml")
	if err := checkWorkflowsSendWhatTheGateReads(queuePath, codeafPath); err != nil {
		t.Fatal(err)
	}

	// A dispatch input the gate does not know fails, named. A drift like
	// #61's — the workflow sending what the gate refuses — is what this
	// catches.
	t.Run("unknown input fails", func(t *testing.T) {
		data, err := os.ReadFile(queuePath)
		if err != nil {
			t.Fatal(err)
		}
		renamed := strings.Replace(string(data), "-f plan=true", "-f planner=true", 1)
		if renamed == string(data) {
			t.Fatal("codeaf-queue.yml no longer sends -f plan=true; update this test")
		}
		path := filepath.Join(t.TempDir(), "codeaf-queue.yml")
		if err := os.WriteFile(path, []byte(renamed), 0o644); err != nil {
			t.Fatal(err)
		}
		err = checkWorkflowsSendWhatTheGateReads(path, codeafPath)
		if err == nil || !strings.Contains(err.Error(), "planner") {
			t.Fatalf("renamed input: err = %v; want one naming planner", err)
		}
	})

	// The same drift on a shell-continuation line: the flag sits under the
	// `gh workflow run` command itself, where a check reading only the
	// command line would never see it. Since issue #137 the queue makes
	// every codeaf.yml dispatch, so the drift is planted in its review
	// dispatch, split over two lines.
	t.Run("continuation input fails", func(t *testing.T) {
		data, err := os.ReadFile(queuePath)
		if err != nil {
			t.Fatal(err)
		}
		old := `gh workflow run codeaf.yml --ref "$DEFAULT_BRANCH" -f "review=$N" -f "lane=$LANE"`
		continued := "gh workflow run codeaf.yml --ref \"$DEFAULT_BRANCH\" \\\n" +
			"                  -f \"review=$N\" -f \"lane=$LANE\" -f planner=true"
		renamed := strings.Replace(string(data), old, continued, 1)
		if renamed == string(data) {
			t.Fatal("codeaf-queue.yml no longer dispatches reviews on one line; update this test")
		}
		path := filepath.Join(t.TempDir(), "codeaf-queue.yml")
		if err := os.WriteFile(path, []byte(renamed), 0o644); err != nil {
			t.Fatal(err)
		}
		err = checkWorkflowsSendWhatTheGateReads(path, codeafPath)
		if err == nil || !strings.Contains(err.Error(), "planner") {
			t.Fatalf("continued input: err = %v; want one naming planner", err)
		}
	})
}

// TestDispatchArgsBadLane: dispatch-args refuses a lane or number that is
// not a plain decimal or that overflows an int, with a message naming the
// argument — it must not silently coerce one to 0, or a caller copying the
// printed lines would dispatch `-f lane=0`, which the gate reads as no
// lane at all. runDispatchArgs reads os.Args; the error return is what
// main turns into exit 1.
func TestDispatchArgsBadLane(t *testing.T) {
	bad := []struct {
		name string
		args []string
	}{
		{"non-digit lane", []string{"harness", "dispatch-args", "issue", "3", "zz"}},
		{"empty lane", []string{"harness", "dispatch-args", "issue", "3", ""}},
		{"overflowing lane", []string{"harness", "dispatch-args", "issue", "3", "99999999999999999999"}},
		{"non-digit number", []string{"harness", "dispatch-args", "issue", "x", "2"}},
		{"overflowing number", []string{"harness", "dispatch-args", "issue", "99999999999999999999", "2"}},
	}
	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			old := os.Args
			os.Args = tt.args
			defer func() { os.Args = old }()
			err := runDispatchArgs()
			if err == nil {
				t.Fatalf("runDispatchArgs(%v) accepted a bad argument", tt.args)
			}
			if !strings.Contains(err.Error(), "dispatch-args") {
				t.Fatalf("runDispatchArgs(%v) err = %v; want a message naming the bad argument", tt.args, err)
			}
		})
	}

	t.Run("valid lane still prints", func(t *testing.T) {
		old := os.Args
		os.Args = []string{"harness", "dispatch-args", "plan", "0", "2"}
		defer func() { os.Args = old }()
		out, err := captureStdout(t, runDispatchArgs)
		if err != nil {
			t.Fatalf("runDispatchArgs(plan, 0, 2): %v", err)
		}
		if want := "-f lane=2\n-f plan=true\n"; out != want {
			t.Fatalf("dispatch-args plan 0 2 printed %q; want %q", out, want)
		}
	})
}
