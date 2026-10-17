package main

import (
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
//     carries a plan title whose created_at is an RFC3339 stamp within
//     MinHours. A plan-title run with a missing or unparseable stamp counts
//     as inside the cooldown: an unknown age must not wave a plan run
//     through (fail closed).
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
	// (d) Cooldown: one plan run per MinHours, read off the created_at
	// stamp every run carries. An unparseable or missing stamp on a
	// plan-title run is treated as inside the cooldown, never outside it.
	horizon := time.Duration(o.MinHours) * time.Hour
	for _, r := range runs {
		if !isPlanRun(r) {
			continue
		}
		t, err := time.Parse(time.RFC3339, r.CreatedAt)
		if err != nil {
			return refuse(fmt.Sprintf("plan cooldown active (run %d: created_at %q unparseable)", r.DatabaseId, r.CreatedAt))
		}
		if age := time.Since(t); age < horizon {
			return refuse(fmt.Sprintf("plan cooldown active (run %d started %.1fh ago, cooldown %dh)", r.DatabaseId, age.Hours(), o.MinHours))
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
	lane, ok, reason := PlanCheck(issues, runs, planOptions{
		QueuedLabel:     envOr("CODEAF_QUEUED_LABEL", "codeaf-queued"),
		Label:           envOr("CODEAF_LABEL", "codeaf"),
		WeekCap:         cfg.Limits.WeeklyCreditUSD,
		WeekSpent:       spent,
		MaxParallelRuns: cfg.Queue.MaxParallelRuns,
		MinBacklog:      cfg.Planner.MinBacklog,
		MinHours:        cfg.Planner.MinHours,
		Logf:            log.Printf,
	})
	if !ok {
		log.Print(reason)
		return nil
	}
	fmt.Printf("plan=1\nlane=%d\n", lane)
	return nil
}
