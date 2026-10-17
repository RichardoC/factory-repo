package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"time"
)

// ---------------------------------------------------------------- planner

// planOptions is the environment the planner trigger decides in. It mirrors
// queueOptions where the two agree: the same labels, the same weekly-credit
// figures and the same lane capacity, so the queue job can decide both
// "what runs next" and "does the backlog need a plan run" from one models.json.
type planOptions struct {
	QueuedLabel string  // backlog label: only open issues carrying it are backlog
	Label       string  // active label: an issue carrying it is already running
	WeekCap     float64 // limits.weekly_credit_usd from models.json
	WeekSpent   float64 // the rolling week's credit spend, from the shared state

	// MaxParallelRuns is queue.max_parallel_runs: how many CodeAF runs may
	// be queued or in progress at once. A plan run is one of them.
	MaxParallelRuns int
	// MinBacklog is planner.min_backlog: the trigger fires only when the
	// open queued backlog is smaller than this.
	MinBacklog int
	// MinHours is planner.min_hours: no plan run may have started within
	// this many hours.
	MinHours int
	// RetryMinHours is planner.retry_min_hours: the shorter cooldown that
	// replaces MinHours after a plan run that opened no proposals (issue
	// #119). A run that proposed nothing has nothing to show for its 12
	// hours, so the next attempt waits only this long.
	RetryMinHours int
	// Outcomes maps a completed plan run's databaseId to the number of
	// proposals the publish job opened for it (issue #119), read from the
	// marker the publish job uploads. A run absent from the map has an
	// unknown outcome, which keeps the full MinHours: fail closed.
	Outcomes map[int]int
	// Logf is where the planner explains itself; nil discards.
	Logf func(string, ...any)
}

// PlanCheck decides whether the queue job should dispatch a CodeAF run in
// plan mode (issue #61): the backlog of open, queued, not-yet-running issues
// has fallen under planner.min_backlog and no plan run started within
// planner.min_hours. It returns the first free lane, like Next does. The
// checks run in a fixed order and each refusal is explained through Logf:
//
//   - the same spend guard as Next: past queueSpendFraction of the weekly
//     credit, nothing starts — a plan run spends credit like any other;
//   - a free lane: fewer than MaxParallelRuns CodeAF runs may be queued or
//     in progress, of any title, a plan run included;
//   - the backlog: open, non-PR issues labelled QueuedLabel and NOT labelled
//     Label must number fewer than MinBacklog;
//   - the cooldown: no run in the list — queued, in progress or completed —
//     carries a plan title whose created_at is an RFC3339 stamp within its
//     cooldown. The cooldown is MinHours, except for a plan run whose known
//     outcome (Outcomes, issue #119) is zero proposals opened: for that run
//     it is RetryMinHours — the run proposed nothing, so the full MinHours
//     would only delay the retry. A plan-title run with a missing or
//     unparseable stamp counts as inside the cooldown whatever its outcome:
//     an unknown age must not wave a plan run through (fail closed).
//
// PlanCheck is deterministic and pure except for Logf: the only clock it
// reads is the one the created_at stamps and MinHours are measured against.
func PlanCheck(issues []item, runs []runStatus, o planOptions) (lane int, ok bool, reason string) {
	logf := func(string, ...any) {}
	if o.Logf != nil {
		logf = o.Logf
	}
	refuse := func(r string) (int, bool, string) {
		logf("plan-check: %s", r)
		return 0, false, r
	}
	// (a) Spend guard: the queue's own threshold. A plan run competes for
	// the same weekly credit as the runs it would propose work for.
	if o.WeekSpent >= queueSpendFraction*o.WeekCap {
		return refuse("spend threshold reached")
	}
	// (b) Free lane: the plan run is dispatched like any other run and
	// takes the first lane no in-flight run occupies.
	mpr := o.MaxParallelRuns
	if mpr < 1 {
		mpr = 1 // loadConfig defaults and rejects; this guards direct callers
	}
	lanes := map[int]bool{}
	ninflight := 0
	for _, r := range runs {
		if r.Status == "queued" || r.Status == "in_progress" {
			ninflight++
			lanes[runLane(r)] = true
		}
	}
	logf("plan-check: runs in progress: %d/%d", ninflight, mpr)
	if ninflight >= mpr {
		return refuse("lane capacity full")
	}
	lane = 0
	for ; lane < mpr && lanes[lane]; lane++ {
	}
	// (c) Backlog: the trigger is a small backlog, not an empty one. PRs
	// are never backlog (the queue never picks them either), and an issue
	// already labelled with the active label is running, not waiting.
	backlog := 0
	for _, i := range issues {
		switch {
		case i.State != "open":
		case len(i.PullRequest) > 0:
		case !i.labelled(o.QueuedLabel):
		case i.labelled(o.Label):
		default:
			backlog++
		}
	}
	logf("plan-check: backlog %d queued, need fewer than %d", backlog, o.MinBacklog)
	if backlog >= o.MinBacklog {
		return refuse(fmt.Sprintf("backlog sufficient (%d queued, need fewer than %d)", backlog, o.MinBacklog))
	}
	// (d) Cooldown: one plan run per cooldown, read off the created_at
	// stamp every run carries. A completed plan run known to have opened
	// no proposals (issue #119) cools down for RetryMinHours instead of
	// MinHours; an unknown outcome keeps the full MinHours. An unparseable
	// or missing stamp on a plan-title run is treated as inside the
	// cooldown whatever the outcome, never outside it.
	for _, r := range runs {
		if !isPlanRun(r) {
			continue
		}
		t, err := time.Parse(time.RFC3339, r.CreatedAt)
		if err != nil {
			return refuse(fmt.Sprintf("plan cooldown active (run %d: created_at %q unparseable)", r.DatabaseId, r.CreatedAt))
		}
		cooldown := o.MinHours
		if n, known := o.Outcomes[r.DatabaseId]; known && n == 0 && o.RetryMinHours > 0 {
			cooldown = o.RetryMinHours
		}
		if age := time.Since(t); age < time.Duration(cooldown)*time.Hour {
			return refuse(fmt.Sprintf("plan cooldown active (run %d started %.1fh ago, cooldown %dh)", r.DatabaseId, age.Hours(), cooldown))
		}
	}
	logf("plan-check: dispatching plan run on lane %d", lane)
	return lane, true, "backlog low"
}

// runPlanCheck implements `harness plan-check ISSUES_JSON RUNS_JSON`
// (issue #61), called by the queue job after its start-issues loop. On a
// plan dispatch it prints exactly "plan=1" then "lane=<digits>", which the
// job writes straight into GITHUB_OUTPUT; when the planner holds, stdout
// stays empty and the command still exits 0, like `harness next`. stderr
// carries the human log: the reason for every refusal.
func runPlanCheck() error {
	if len(os.Args) != 4 {
		return errors.New("usage: harness plan-check ISSUES_JSON RUNS_JSON")
	}
	issues, err := readItems(os.Args[2])
	if err != nil {
		return err
	}
	runs, err := readRuns(os.Args[3])
	if err != nil {
		return err
	}
	// CODEAF_WEEK_SPENT_USD is the week_usd figure `harness state load`
	// printed. It is required, as for `harness next`: treating a missing
	// total as $0 would start a plan run with the week's spend unknown.
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
	outcomes, err := readPlanOutcomes(os.Getenv("PLAN_OUTCOMES_PATH"))
	if err != nil {
		// Untrusted data that fails validation: ignore it, and the full
		// cooldown applies (fail closed), like a missing marker.
		log.Printf("plan-check: ignoring the plan-outcome marker: %v", err)
		outcomes = nil
	}
	lane, ok, reason := PlanCheck(issues, runs, planOptions{
		QueuedLabel:     envOr("CODEAF_QUEUED_LABEL", "codeaf-queued"),
		Label:           envOr("CODEAF_LABEL", "codeaf"),
		WeekCap:         cfg.Limits.WeeklyCreditUSD,
		WeekSpent:       spent,
		MaxParallelRuns: cfg.Queue.MaxParallelRuns,
		MinBacklog:      cfg.Planner.MinBacklog,
		MinHours:        cfg.Planner.MinHours,
		RetryMinHours:   cfg.Planner.RetryMinHours,
		Outcomes:        outcomes,
		Logf:            log.Printf,
	})
	if !ok {
		log.Print(reason)
		return nil
	}
	fmt.Printf("plan=1\nlane=%d\n", lane)
	return nil
}

// planOutcome is the marker the publish job of a plan run uploads (issue
// #119): the run's own databaseId and how many proposals its publish step
// opened as issues. plan-check reads it so a run that opened nothing cools
// down for planner.retry_min_hours instead of planner.min_hours.
type planOutcome struct {
	RunID     int `json:"run_id"`
	Proposals int `json:"proposals"`
	Held      int `json:"held"`
}

// readPlanOutcomes reads the marker file path points at and returns its
// run id -> proposals-opened map. The marker comes from the untrusted run
// job, so every field is validated: run_id positive, proposals and held
// not negative. An absent or unset path answers no outcome; a present but
// malformed one is an error, which the caller turns into no outcome — the
// full cooldown, never the short one.
func readPlanOutcomes(path string) (map[int]int, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var o planOutcome
	if err := json.Unmarshal(data, &o); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if o.RunID <= 0 || o.Proposals < 0 || o.Held < 0 {
		return nil, fmt.Errorf("%s: run_id %d, proposals %d, held %d is not a valid plan outcome", path, o.RunID, o.Proposals, o.Held)
	}
	return map[int]int{o.RunID: o.Proposals}, nil
}
