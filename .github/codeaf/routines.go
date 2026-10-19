// routines.go implements factory routines (issue #117): recurring factory
// activities run on a schedule, configured in one trusted file. The pieces
// it replaces were one-offs: the weekly ref check (codeaf-ref-check.yml),
// the planner trigger (plan-check) and other hand-run checks become
// routines. routines.json sits next to models.json, is trusted config on
// the default branch, and is validated on load the way models.json is:
// unknown keys and bad values fail with a message naming the field.
//
// Two kinds of routine exist:
//
//   - check: a deterministic harness subcommand. No model runs, no secret
//     beyond GITHUB_TOKEN; it reads the network only for public data. It
//     handles anything computable: diffs, counts, test reruns.
//   - agent: a read-only CodeAF run with a brief from a template under
//     .github/codeaf/routines/, dispatched like a plan run. Its output
//     goes through the planner's proposal path (codeaf-proposed issues,
//     never codeaf-queued, never a patch).
//
// The queue workflow no longer decides when routines fire (the maintainer
// wiring lives in a dedicated read-only routines workflow): `harness
// routines-due` is called there, on a schedule and on every queue tick,
// and prints the dispatch inputs of every routine whose cooldown has
// passed. Standard library only.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"
)

// The two kinds of routine (issue #117).
const (
	routineKindCheck = "check"
	routineKindAgent = "agent"
)

// routinesPath is routines.json, read from the harness directory like
// models.json. The env override follows PROXY_MODELS so a test or a job can
// point at another copy without moving the harness.
func routinesPath() string {
	return envOr("PROXY_ROUTINES", filepath.Join(harnessDir(), "routines.json"))
}

// routine is one entry of routines.json. Every field is validated in
// validateRoutine; a command or brief that does not belong to the entry's
// kind is a bad value, not an ignored one.
type routine struct {
	Name       string  `json:"name"`
	EveryHours int     `json:"every_hours"`
	Kind       string  `json:"kind"`
	Command    string  `json:"command,omitempty"` // check only: the check command's name
	Brief      string  `json:"brief,omitempty"`   // agent only: template path under the harness dir
	MaxUSD     float64 `json:"max_usd,omitempty"` // agent only: per-run credit cap, enforced by the proxy like run_credit_usd
}

// routinesDoc is the file's shape: {"routines": [...]}.
type routinesDoc struct {
	// Comment carries the file's own explanation, like models.json's; it
	// is data, never read by the code.
	Comment  string    `json:"_comment,omitempty"`
	Routines []routine `json:"routines"`
}

// routineNameRe bounds a routine's name: lowercase, digits and dashes, at
// most 32 characters. It is the name the run title, the dispatch input and
// the proposal label carry, so anything outside it would not round-trip
// through the gate.
var routineNameRe = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// routineBriefRe bounds an agent routine's template path: a relative file
// name under the harness directory's routines/ directory, ending in .md. A
// path that escapes the directory (.., a leading /) is refused, so a
// template name can never point outside the trusted config.
var routineBriefRe = regexp.MustCompile(`^[a-z0-9][a-z0-9/_-]*\.md$`)

// LoadRoutines reads and validates routines.json. The file is trusted
// config on the default branch, but it is still validated on every load:
// an unknown key or a bad value must fail loudly here, not behave oddly in
// a scheduled job nobody watches.
func LoadRoutines() ([]routine, error) {
	data, err := os.ReadFile(routinesPath())
	if err != nil {
		return nil, err
	}
	return parseRoutines(data)
}

// parseRoutines validates routines config. DisallowUnknownFields rejects an
// unknown key at either level with the field's own name in the message; the
// per-entry checks name the field and its index, so a maintainer can find
// the line.
func parseRoutines(data []byte) ([]routine, error) {
	var doc routinesDoc
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("routines.json: %w", err)
	}
	seen := map[string]bool{}
	for i, r := range doc.Routines {
		if err := validateRoutine(r, i); err != nil {
			return nil, err
		}
		if seen[r.Name] {
			return nil, fmt.Errorf("routines.json: routines[%d].name: %q is a duplicate", i, r.Name)
		}
		seen[r.Name] = true
	}
	return doc.Routines, nil
}

// validateRoutine checks one entry. Every message names the field: the
// committed file is edited by hand, and an unnamed failure in a scheduled
// job is a debugging session, not a validation error.
func validateRoutine(r routine, i int) error {
	if !routineNameRe.MatchString(r.Name) {
		return fmt.Errorf("routines.json: routines[%d].name: %q does not match %s", i, r.Name, routineNameRe)
	}
	if r.EveryHours < 1 {
		return fmt.Errorf("routines.json: routines[%d].every_hours: %d is less than 1", i, r.EveryHours)
	}
	switch r.Kind {
	case routineKindCheck:
		switch {
		case r.Command == "":
			return fmt.Errorf("routines.json: routines[%d].command: a check routine needs a command", i)
		case r.Brief != "":
			return fmt.Errorf("routines.json: routines[%d].brief: a check routine takes no brief", i)
		case r.MaxUSD != 0:
			return fmt.Errorf("routines.json: routines[%d].max_usd: a check routine runs no model and takes no max_usd", i)
		}
	case routineKindAgent:
		switch {
		case !routineBriefRe.MatchString(r.Brief):
			return fmt.Errorf("routines.json: routines[%d].brief: %q is not a template path under routines/", i, r.Brief)
		case filepath.Clean(filepath.Join("routines", r.Brief)) != filepath.Join("routines", r.Brief):
			return fmt.Errorf("routines.json: routines[%d].brief: %q escapes routines/", i, r.Brief)
		case r.Command != "":
			return fmt.Errorf("routines.json: routines[%d].command: an agent routine takes no command", i)
		case r.MaxUSD <= 0:
			return fmt.Errorf("routines.json: routines[%d].max_usd: an agent routine needs a positive max_usd", i)
		}
	default:
		return fmt.Errorf("routines.json: routines[%d].kind: %q is not one of check, agent", i, r.Kind)
	}
	return nil
}

// runRoutineRe matches the display title a routine dispatch writes (issue
// #117): "CodeAF routine <name>" or "CodeAF routine <name> (lane N)". Like
// runTitleRe and runPlanRe it is anchored and strict: run titles are
// untrusted data, and anything else — another prefix, a name outside
// routineNameRe, trailing prose — parses to no routine run.
var runRoutineRe = regexp.MustCompile(`^CodeAF routine ([a-z0-9-]{1,32})(?: \(lane ([0-9]+)\))?$`)

// runRoutineFields reads the routine name and lane out of a routine run's
// display title. ok is false for every title that is not a routine title;
// the shapes of issue, plan, review and routine titles are disjoint, so one
// run is never matched by two of them.
func runRoutineFields(title string) (name string, lane int, ok bool) {
	m := runRoutineRe.FindStringSubmatch(title)
	if m == nil {
		return "", 0, false
	}
	// m[2] is empty when the suffix is absent; that is lane 0 too. The
	// regex already guarantees digits, so the conversion cannot fail.
	if m[2] != "" {
		lane, err := strconv.Atoi(m[2])
		if err != nil {
			return m[1], 0, true // unreachable; the regex allows only digits
		}
		return m[1], lane, true
	}
	return m[1], 0, true
}

// isRoutineRun reports whether a run was dispatched in routine mode (issue
// #117). A routine run occupies its lane like any other run, so the queue's
// lane count stays honest.
func isRoutineRun(r runStatus) bool {
	_, _, ok := runRoutineFields(r.DisplayTitle)
	return ok
}

// routineOptions is the environment RoutinesDue decides in: the same
// weekly-credit figures and lane capacity the queue and the planner read,
// so a routine competes for the same budget and the same seats.
type routineOptions struct {
	WeekCap         float64 // limits.weekly_credit_usd from models.json
	WeekSpent       float64 // the rolling week's credit spend, from the shared state
	MaxParallelRuns int     // queue.max_parallel_runs
	// Now is the clock the cooldowns are measured against; runRoutinesDue
	// passes time.Now. Tests pass a fixed time.
	Now time.Time
	// Logf is where routines-due explains itself; nil discards.
	Logf func(string, ...any)
}

// dueRoutine is one routine whose cooldown has passed, with the free lane
// it may be dispatched on.
type dueRoutine struct {
	routine
	Lane int
}

// RoutinesDue decides which routines are due (issue #117). A routine is due
// when no run titled "CodeAF routine <name>" started within its
// every_hours, and the same guards the planner runs hold: the spend floor
// (queueSpendFraction of the weekly credit, the queue's own threshold — a
// routine run competes for the same weekly credit) and a free lane. The
// checks run in a fixed order and each refusal is explained through Logf:
//
//   - the spend guard: past queueSpendFraction of the weekly credit,
//     nothing is due;
//   - a free lane: fewer than MaxParallelRuns runs may be queued or in
//     progress; each due routine takes the next free lane, and the rest
//     wait for the next tick;
//   - the cooldown: no run with the routine's title — queued, in progress
//     or completed — carries a created_at RFC3339 stamp within
//     every_hours. A run of the routine still queued or in progress holds
//     it whatever its stamp, and a routine-title run with a missing or
//     unparseable stamp holds it too: an unknown age must not wave a
//     routine through (fail closed).
//
// RoutinesDue is deterministic and pure except for Logf: the only clock it
// reads is o.Now against the created_at stamps.
func RoutinesDue(routines []routine, runs []runStatus, o routineOptions) []dueRoutine {
	logf := func(string, ...any) {}
	if o.Logf != nil {
		logf = o.Logf
	}
	// (a) Spend guard: the queue's own threshold. A routine run spends
	// credit like any other run.
	if o.WeekSpent >= queueSpendFraction*o.WeekCap {
		logf("routines-due: spend threshold reached")
		return nil
	}
	// (b) Free lanes: a routine run is dispatched like any other run and
	// takes a lane no in-flight run occupies. Due routines are assigned in
	// file order; when the lanes run out, the rest wait for the next tick.
	mpr := o.MaxParallelRuns
	if mpr < 1 {
		mpr = 1
	}
	taken := map[int]bool{}
	ninflight := 0
	for _, r := range runs {
		if r.Status == "queued" || r.Status == "in_progress" {
			ninflight++
			taken[runLane(r)] = true
		}
	}
	logf("routines-due: runs in progress: %d/%d", ninflight, mpr)
	nextLane := func() (int, bool) {
		for lane := 0; lane < mpr; lane++ {
			if !taken[lane] {
				taken[lane] = true
				return lane, true
			}
		}
		return 0, false
	}
	// (c) The cooldown, per routine, read off the created_at stamp every
	// run carries.
	var due []dueRoutine
	for _, rt := range routines {
		held := ""
		for _, r := range runs {
			name, _, ok := runRoutineFields(r.DisplayTitle)
			if !ok || name != rt.Name {
				continue
			}
			if r.Status == "queued" || r.Status == "in_progress" {
				held = fmt.Sprintf("run %d is %s", r.DatabaseId, r.Status)
				break
			}
			t, err := time.Parse(time.RFC3339, r.CreatedAt)
			if err != nil {
				held = fmt.Sprintf("run %d: created_at %q unparseable", r.DatabaseId, r.CreatedAt)
				break
			}
			if age := o.Now.Sub(t); age < time.Duration(rt.EveryHours)*time.Hour {
				held = fmt.Sprintf("run %d started %.1fh ago, cooldown %dh", r.DatabaseId, age.Hours(), rt.EveryHours)
				break
			}
		}
		if held != "" {
			logf("routines-due: %s held: %s", rt.Name, held)
			continue
		}
		lane, ok := nextLane()
		if !ok {
			logf("routines-due: %s held: lane capacity full", rt.Name)
			continue
		}
		logf("routines-due: %s due on lane %d", rt.Name, lane)
		due = append(due, dueRoutine{routine: rt, Lane: lane})
	}
	return due
}

// runRoutinesDue implements `harness routines-due ISSUES_JSON RUNS_JSON`
// (issue #117), called by the routines workflow on its schedule and on
// every queue tick. For every due routine it prints one line,
// "routine=<name> lane=<digits>" — the dispatch inputs the job feeds `gh
// workflow run codeaf.yml -f routine=<name> -f lane=<n>` — which Decide
// reads back as a routine run. When nothing is due, stdout stays empty and
// the command still exits 0, like `harness next` and `plan-check`; stderr
// carries the human log.
//
// The issues file is read and validated like plan-check's, for the same
// reason: the call site hands both lists over, and a malformed list is an
// installation error, not a silent zero. The backlog condition itself is
// not a routine guard — a routine runs on its cooldown, not on the queue's
// backlog — but the argument stays so the call sites of plan-check and
// routines-due look alike and a future `when` guard has its input already.
func runRoutinesDue() error {
	if len(os.Args) != 4 {
		return errors.New("usage: harness routines-due ISSUES_JSON RUNS_JSON")
	}
	if _, err := readItems(os.Args[2]); err != nil {
		return err
	}
	runs, err := readRuns(os.Args[3])
	if err != nil {
		return err
	}
	// CODEAF_WEEK_SPENT_USD is the week_usd figure `harness state load`
	// printed. It is required, as for `harness next` and `plan-check`:
	// treating a missing total as $0 would start a routine with the week's
	// spend unknown.
	if os.Getenv("CODEAF_WEEK_SPENT_USD") == "" {
		return errors.New("CODEAF_WEEK_SPENT_USD is empty; run `harness state load` first")
	}
	spent, err := spentWeek("CODEAF_WEEK_SPENT_USD")
	if err != nil {
		return err
	}
	cfg, err := loadConfig(configPath())
	if err != nil {
		return err
	}
	rts, err := LoadRoutines()
	if err != nil {
		return err
	}
	for _, d := range RoutinesDue(rts, runs, routineOptions{
		WeekCap:         cfg.Limits.WeeklyCreditUSD,
		WeekSpent:       spent,
		MaxParallelRuns: cfg.Queue.MaxParallelRuns,
		Now:             time.Now(),
		Logf:            log.Printf,
	}) {
		fmt.Printf("routine=%s lane=%d\n", d.Name, d.Lane)
	}
	return nil
}

// checkFunc is one check routine: a deterministic harness function that
// runs no model and answers the report text the calling workflow posts on
// the routine's tracking issue. The text is harness-rendered from
// validated data, never model text — that is what makes a check routine
// allowed to write on a tracking issue.
type checkFunc func() (string, error)

// routineChecks is the registry of check commands, filled by each check's
// own init through registerCheck. A name in routines.json is only valid
// when it is registered here (TestRoutineCatalog pins that) or names an
// agent brief template on disk.
var routineChecks = map[string]checkFunc{}

// registerCheck adds one check command. A duplicate is a programming error
// in this module, not a runtime condition: it panics.
func registerCheck(name string, fn checkFunc) {
	if _, dup := routineChecks[name]; dup {
		panic(fmt.Sprintf("registerCheck: %q is already registered", name))
	}
	routineChecks[name] = fn
}

// runRoutineCheck implements `harness routine-check NAME` (issue #117): it
// runs the named check command and prints its report text, which the
// calling workflow posts on the routine's tracking issue. A check runs no
// agent and needs no secret beyond GITHUB_TOKEN; the report is
// harness-rendered text built from validated data.
func runRoutineCheck() error {
	if len(os.Args) != 3 {
		return errors.New("usage: harness routine-check NAME")
	}
	fn, ok := routineChecks[os.Args[2]]
	if !ok {
		return fmt.Errorf("routine-check: unknown check command %q", os.Args[2])
	}
	text, err := fn()
	if err != nil {
		return err
	}
	fmt.Print(text)
	return nil
}
