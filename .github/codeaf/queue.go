package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"slices"
	"time"
)

// ---------------------------------------------------------------- queue

// runStatus is one entry of the `gh run list --workflow=codeaf.yml --json
// status` array the queue reads.
type runStatus struct {
	Status string `json:"status"`
}

// queueOptions is the environment the next command decides in.
type queueOptions struct {
	Label         string  // active label: an issue carrying it is already running
	QueuedLabel   string  // backlog label: only issues carrying it are candidates
	PriorityLabel string  // jumps the queue
	DayCap        float64 // limits.day_usd from models.json
	DaySpent      float64 // today's spend, from the shared state
}

// queueSpendFraction is the largest share of the day cap at which the queue
// still starts a run: at 75% of day_usd spent, the rest of the day's budget
// is left for the run it is about to start.
const queueSpendFraction = 0.75 // 75% of day_usd

// maxBodyScan caps how much of a candidate's body is scanned for "Depends on"
// lines. Issue bodies are untrusted text of unbounded size, so only a bounded
// prefix is read, like the harness's other reads of untrusted files.
const maxBodyScan = 1 << 16 // 64KiB

// Next picks the issue CodeAF should work next. It returns the issue's number
// and a reason, or 0 and a reason when nothing should run: no CodeAF run may
// be in progress or waiting, today's spend must be under the threshold, and
// the pick is the oldest queued issue (priority-labelled ones first) whose
// "Depends on" references are all closed. The reason is for humans, on
// stderr; stdout carries only the number.
func Next(issues []item, runs []runStatus, o queueOptions) (int, string) {
	for _, r := range runs {
		if r.Status == "queued" || r.Status == "in_progress" {
			return 0, "a CodeAF run is in progress or waiting"
		}
	}
	if o.DaySpent >= queueSpendFraction*o.DayCap {
		return 0, "spend threshold reached"
	}
	// open holds every open entry by number, PRs included: a "Depends on"
	// an open PR blocks just like one on an open issue.
	open := map[int]bool{}
	for _, i := range issues {
		if i.State == "open" {
			open[i.Number] = true
		}
	}
	var candidates []item
	for _, i := range issues {
		switch {
		case i.State != "open":
		case len(i.PullRequest) > 0: // PRs are never candidates
		case !i.labelled(o.QueuedLabel):
		case i.labelled(o.Label): // already picked up
		default:
			candidates = append(candidates, i)
		}
	}
	if len(candidates) == 0 {
		return 0, "nothing queued"
	}
	slices.SortFunc(candidates, func(a, b item) int { return queueLess(a, b, o.PriorityLabel) })
	stuck := ""
	for _, c := range candidates {
		if n := openDep(depsFromBody(cappedBody(c.Body)), open); n > 0 {
			if stuck == "" {
				stuck = fmt.Sprintf("dependencies open for #%d", n)
			}
			continue
		}
		return c.Number, "next queued issue"
	}
	return 0, stuck
}

// queueLess orders the queue: priority-labelled issues first, then the oldest
// created_at first (RFC3339; an unparseable stamp sorts last), the lower
// issue number breaking remaining ties so the pick never depends on the
// order the issues arrived in.
func queueLess(a, b item, priorityLabel string) int {
	if ap, bp := a.labelled(priorityLabel), b.labelled(priorityLabel); ap != bp {
		if ap {
			return -1
		}
		return 1
	}
	da, aok := stamp(a.CreatedAt)
	db, bok := stamp(b.CreatedAt)
	switch {
	case aok && bok && !da.Equal(db):
		if da.Before(db) {
			return -1
		}
		return 1
	case aok != bok:
		if aok {
			return -1
		}
		return 1
	}
	switch {
	case a.Number < b.Number:
		return -1
	case a.Number > b.Number:
		return 1
	}
	return 0
}

// stamp parses an RFC3339 timestamp; ok is false when the stamp is missing or
// malformed, which sorts last.
func stamp(s string) (t time.Time, ok bool) {
	t, err := time.Parse(time.RFC3339, s)
	return t, err == nil
}

// openDep returns the first of deps that names an open entry, or 0. A
// self-reference is open by construction — the candidate is in open — so a
// body that "Depends on" its own number blocks it too.
func openDep(deps []int, open map[int]bool) int {
	for _, n := range deps {
		if open[n] {
			return n
		}
	}
	return 0
}

// cappedBody cuts a body to its first maxBodyScan bytes.
func cappedBody(s string) string {
	if len(s) > maxBodyScan {
		return s[:maxBodyScan]
	}
	return s
}

// readItems reads a JSON array of issues, as
// `gh api "repos/$REPO/issues?state=open&per_page=100"` prints it.
func readItems(path string) ([]item, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var items []item
	if err := json.Unmarshal(data, &items); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return items, nil
}

// readRuns reads a JSON array of workflow runs, as
// `gh run list --workflow=codeaf.yml --json status` prints it.
func readRuns(path string) ([]runStatus, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var runs []runStatus
	if err := json.Unmarshal(data, &runs); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return runs, nil
}

// runNext implements `harness next ISSUES_JSON RUNS_JSON`: it prints the
// number of the queued issue CodeAF should work next, or nothing, and the
// reason either way on stderr. The queue job labels that issue to start the
// run, which keeps the trust gate: only the workflow's own token labels.
func runNext() error {
	if len(os.Args) != 4 {
		return errors.New("usage: harness next ISSUES_JSON RUNS_JSON")
	}
	issues, err := readItems(os.Args[2])
	if err != nil {
		return err
	}
	runs, err := readRuns(os.Args[3])
	if err != nil {
		return err
	}
	// CODEAF_DAY_SPENT_USD is the day_usd figure `harness state load`
	// printed. It is required: treating a missing total as $0 would start
	// runs with the day's spend unknown.
	if os.Getenv("CODEAF_DAY_SPENT_USD") == "" {
		return errors.New("CODEAF_DAY_SPENT_USD is empty; run `harness state load` first")
	}
	spent, err := spentToday("CODEAF_DAY_SPENT_USD")
	if err != nil {
		return err
	}
	cfg, err := loadConfig(configPath())
	if err != nil {
		return err
	}
	number, reason := Next(issues, runs, queueOptions{
		Label:         envOr("CODEAF_LABEL", "codeaf"),
		QueuedLabel:   envOr("CODEAF_QUEUED_LABEL", "codeaf-queued"),
		PriorityLabel: envOr("CODEAF_PRIORITY_LABEL", "codeaf-priority"),
		DayCap:        cfg.Limits.DayUSD,
		DaySpent:      spent,
	})
	if number == 0 {
		log.Print(reason)
		return nil
	}
	log.Printf("starting #%d: %s", number, reason)
	fmt.Println(number)
	return nil
}
