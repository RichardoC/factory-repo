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
}

// TestDispatchInvalid: an unknown kind and a non-positive number on the
// three numeric kinds are errors, like the gate refusing the dispatch.
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

// gateInputs are the workflow_dispatch input names the gate reads.
var gateInputs = map[string]bool{
	"issue": true, "pr": true, "review": true, "lane": true, "plan": true, "profile": true,
}

// dispatchFlag extracts the name from one `-f name=` or `-f "name=` flag.
var dispatchFlag = regexp.MustCompile(`-f "?([A-Za-z][A-Za-z0-9_-]*)=`)

// checkWorkflowsSendWhatTheGateReads walks both workflow files and refuses
// any dispatch flag the gate does not read, and any run-name line that has
// drifted from the title shapes title() mirrors. The harness is
// stdlib-only, so the YAML is read as text.
func checkWorkflowsSendWhatTheGateReads(queuePath, codeafPath string) error {
	checkDispatch := func(path string) error {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(data), "\n") {
			if !strings.Contains(line, "gh workflow run codeaf.yml") {
				continue
			}
			for _, m := range dispatchFlag.FindAllStringSubmatch(line, -1) {
				if !gateInputs[m[1]] {
					return fmt.Errorf("%s:%d: dispatch sends -f %s=, which the gate does not read", path, i+1, m[1])
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
}
