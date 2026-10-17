package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------- queue

// runStatus is one entry of the
// `gh run list --workflow=codeaf.yml --json status,databaseId,displayTitle`
// array the queue reads. DatabaseId and DisplayTitle are untrusted data: the
// title is parsed strictly, and anything that does not match the queue's own
// shape counts as lane 0 and no known issue.
type runStatus struct {
	Status       string `json:"status"`
	DatabaseId   int    `json:"databaseId"`
	DisplayTitle string `json:"displayTitle"`
}

// queueOptions is the environment the next command decides in.
type queueOptions struct {
	Label         string  // active label: an issue carrying it is already running
	QueuedLabel   string  // backlog label: only issues carrying it are candidates
	PriorityLabel string  // jumps the queue
	WeekCap       float64 // limits.weekly_credit_usd from models.json
	WeekSpent     float64 // the rolling week's credit spend, from the shared state

	// MaxParallelRuns is queue.max_parallel_runs: how many CodeAF runs may
	// be queued or in progress at once. Each gets a lane and its own slice
	// of the models' concurrency.
	MaxParallelRuns int
	// Logf is where the queue explains itself; nil discards.
	Logf func(string, ...any)
}

// queueSpendFraction is the largest share of the weekly credit at which the
// queue still starts a run: at 75% of weekly_credit_usd spent in the rolling
// week, the rest of the week's budget is left for the run it is about to
// start.
const queueSpendFraction = 0.75 // 75% of the weekly credit

// maxBodyScan caps how much of a candidate's body is scanned for "Depends on"
// lines. Issue bodies are untrusted text of unbounded size, so only a bounded
// prefix is read, like the harness's other reads of untrusted files.
const maxBodyScan = 1 << 16 // 64KiB

// Next picks the issue CodeAF should work next and the lane it should run
// on. It returns 0 and a reason when nothing should run: fewer than
// MaxParallelRuns CodeAF runs may be queued or in progress (they count
// alike), the rolling week's credit spend must stay under the threshold, and
// the pick is the oldest queued issue (priority-labelled ones first) whose
// "Depends on" references are all closed and whose areas collide with no run
// in flight. The queue logs the runs in progress with their lanes, and why
// it held or started each candidate, through Logf. The pick and the lane are
// deterministic; Next is pure except for Logf.
func Next(issues []item, runs []runStatus, o queueOptions) (int, int, string) {
	quiet := func(string, ...any) {}
	logf := quiet
	if o.Logf != nil {
		logf = o.Logf
	}
	mpr := o.MaxParallelRuns
	if mpr < 1 {
		mpr = 1 // loadConfig defaults and rejects; this guards direct callers
	}
	// Runs in flight: queued counts like in_progress, completed does not
	// count. Each fills a lane (from its title; an unparsable title is lane
	// 0) and holds the areas of the issue it is working on.
	var inflight []runStatus
	lanes := map[int]bool{}
	for _, r := range runs {
		if r.Status == "queued" || r.Status == "in_progress" {
			inflight = append(inflight, r)
			lanes[runLane(r)] = true
		}
	}
	logf("runs in progress: %d/%d, lanes in use: %s", len(inflight), mpr, laneList(inflight))
	if o.WeekSpent >= queueSpendFraction*o.WeekCap {
		return 0, 0, "spend threshold reached"
	}
	// open holds every open entry by number, PRs included: a "Depends on"
	// an open PR blocks just like one on an open issue. byNumber is the
	// same list, and matches in-flight runs to their issue: the open
	// issues include the labelled issues those runs are working on.
	open := map[int]bool{}
	byNumber := map[int]item{}
	for _, i := range issues {
		if i.State == "open" {
			open[i.Number] = true
			byNumber[i.Number] = i
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
		return 0, 0, "nothing queued"
	}
	slices.SortFunc(candidates, func(a, b item) int { return queueLess(a, b, o.PriorityLabel) })
	stuck := ""
	hold := func(reason string) {
		if stuck == "" {
			stuck = reason
		}
	}
	for _, c := range candidates {
		if len(inflight) >= mpr {
			logf("holding #%d: lane capacity full", c.Number)
			hold("lane capacity full")
			continue
		}
		if n := openDep(depsFromBody(cappedBody(c.Body)), open); n > 0 {
			logf("holding #%d: dependencies open for #%d", c.Number, n)
			hold(fmt.Sprintf("dependencies open for #%d", n))
			continue
		}
		areas := issueAreas(c)
		if busy, r, conflict := areaBusy(areas, inflight, byNumber); conflict {
			reason := fmt.Sprintf("area %s busy (run %d)", busy, r.DatabaseId)
			if len(areas) == 0 {
				reason = fmt.Sprintf("no area; conflicts with run %d", r.DatabaseId)
			}
			logf("holding #%d: %s", c.Number, reason)
			hold(reason)
			continue
		}
		// The capacity check above leaves a lane free: fewer than mpr runs
		// are in flight, each occupying one lane value, so the scan always
		// finds one. Lanes outside [0, mpr) are occupied but never assigned.
		lane := 0
		for ; lane < mpr && lanes[lane]; lane++ {
		}
		logf("starting #%d (lane %d): next queued issue", c.Number, lane)
		return c.Number, lane, "next queued issue"
	}
	return 0, 0, stuck
}

// runTitleRe matches the display title a queued dispatch writes: "CodeAF #3"
// or "CodeAF #3 (lane 1)". Run titles are untrusted data, so the match is
// anchored and strict; anything else — another prefix, a lane with letters
// in it, trailing prose — parses to no issue and lane 0.
var runTitleRe = regexp.MustCompile(`^CodeAF #([0-9]+)(?: \(lane ([0-9]+)\))?$`)

// runTitleFields reads the issue number and lane out of a run's display
// title. A title that does not match the queue's own shape is lane 0 and no
// issue; a lane parsed outside [0, max_parallel_runs) still occupies its
// value, and only lanes in range are ever assigned.
func runTitleFields(title string) (issue, lane int) {
	m := runTitleRe.FindStringSubmatch(title)
	if m == nil {
		return 0, 0
	}
	issue, _ = strconv.Atoi(m[1])
	// m[2] is empty when the suffix is absent; that is lane 0 too.
	lane, _ = strconv.Atoi(m[2])
	return issue, lane
}

// runLane is the lane a run was dispatched on, read out of its title.
func runLane(r runStatus) int {
	_, lane := runTitleFields(r.DisplayTitle)
	return lane
}

// laneList renders the lanes the in-flight runs occupy, for the queue log:
// "0 (run 1234), 1 (run 1235)".
func laneList(runs []runStatus) string {
	if len(runs) == 0 {
		return "(none)"
	}
	parts := make([]string, len(runs))
	for i, r := range runs {
		parts[i] = fmt.Sprintf("%d (run %d)", runLane(r), r.DatabaseId)
	}
	return strings.Join(parts, ", ")
}

// areaHarness names the harness area: .github/ and everything the harness
// job itself runs.
const areaHarness = "harness"

// issueAreas is an issue's area set: its area labels first, plus the areas
// of any paths in its body (only the bounded prefix is scanned). A
// "area:harness" label or a path under .github/ names the harness; a
// "area:app/<name>" label or a path under app/<name>/ names the app. The set
// is sorted, so the pick and its log never depend on the order the sources
// arrived in. An empty set — the issue names no area the queue knows —
// conflicts with everything, both sides.
func issueAreas(it item) []string {
	var areas []string
	seen := map[string]bool{}
	add := func(a string) {
		if a != "" && !seen[a] {
			seen[a] = true
			areas = append(areas, a)
		}
	}
	for _, l := range it.Labels {
		switch {
		case l.Name == "area:"+areaHarness:
			add(areaHarness)
		case strings.HasPrefix(l.Name, "area:app/"):
			add(strings.TrimPrefix(l.Name, "area:"))
		}
	}
	for _, p := range bodyPaths(cappedBody(it.Body)) {
		add(p)
	}
	slices.Sort(areas)
	return areas
}

// bodyPathRe finds path-like text in an issue body that names the harness or
// an app. Only a bounded prefix of the body is scanned (cappedBody). The
// match is loose on purpose — "app/" inside a longer word, a dot before
// github in one — because a stray extra area only ever holds a run that
// could have started, while a missed one would let two runs collide.
var bodyPathRe = regexp.MustCompile(`\.github(?:/[A-Za-z0-9_.-]+)*|app/[A-Za-z0-9_-]+(?:/[A-Za-z0-9_.-]+)*`)

// bodyPaths maps the paths found in a body to their areas.
func bodyPaths(body string) []string {
	var areas []string
	seen := map[string]bool{}
	for _, p := range bodyPathRe.FindAllString(body, -1) {
		if a := pathArea(p); a != "" && !seen[a] {
			seen[a] = true
			areas = append(areas, a)
		}
	}
	return areas
}

// pathArea maps one path to its area: the harness for anything under
// .github/, the app for app/<name>/, and nothing for anything else.
func pathArea(p string) string {
	switch {
	case p == ".github" || strings.HasPrefix(p, ".github/"):
		return areaHarness
	case strings.HasPrefix(p, "app/"):
		name := strings.TrimPrefix(p, "app/")
		if i := strings.IndexByte(name, '/'); i >= 0 {
			name = name[:i]
		}
		if name != "" {
			return "app/" + name
		}
	}
	return ""
}

// runAreas is the area set an in-flight run works on: the issue named in its
// display title, looked up in the open issues the queue already fetched. A
// title that does not parse, or an issue that is not open in the list, gives
// an empty set, which conflicts with everything — an unknown run must not
// start a second one on top of itself.
func runAreas(r runStatus, byNumber map[int]item) []string {
	n, _ := runTitleFields(r.DisplayTitle)
	if n <= 0 {
		return nil
	}
	it, ok := byNumber[n]
	if !ok {
		return nil
	}
	return issueAreas(it)
}

// areaBusy finds the first in-flight run whose areas collide with the
// candidate's, and the area to name in the log. An empty set on either side
// conflicts with everything; when the collision is an intersection, the
// first (sorted) shared area is named.
func areaBusy(areas []string, inflight []runStatus, byNumber map[int]item) (busy string, r runStatus, conflict bool) {
	for _, run := range inflight {
		other := runAreas(run, byNumber)
		switch {
		case len(areas) == 0 || len(other) == 0:
			if len(areas) != 0 {
				busy = areas[0]
			}
			return busy, run, true
		}
		if a := firstShared(areas, other); a != "" {
			return a, run, true
		}
	}
	return "", runStatus{}, false
}

// firstShared is the first element of a that also appears in b; both are
// sorted, so the pick is deterministic.
func firstShared(a, b []string) string {
	for _, x := range a {
		if slices.Contains(b, x) {
			return x
		}
	}
	return ""
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

// runNext implements `harness next ISSUES_JSON RUNS_JSON`: it picks the
// queued issue and lane to run and prints them on stdout, which the queue
// job writes straight into GITHUB_OUTPUT. On a start that is exactly
// "number=<n>" then "lane=<digits>" and nothing else; when the queue holds,
// stdout stays empty and the command still exits 0. stderr carries the human
// log: the runs in progress with their lanes, and why each candidate was
// held or started. The queue job labels that issue to start the run, which
// keeps the trust gate: only the workflow's own token labels.
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
	// CODEAF_WEEK_SPENT_USD is the week_usd figure `harness state load`
	// printed. It is required: treating a missing total as $0 would start
	// runs with the week's spend unknown.
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
	number, lane, reason := Next(issues, runs, queueOptions{
		Label:           envOr("CODEAF_LABEL", "codeaf"),
		QueuedLabel:     envOr("CODEAF_QUEUED_LABEL", "codeaf-queued"),
		PriorityLabel:   envOr("CODEAF_PRIORITY_LABEL", "codeaf-priority"),
		WeekCap:         cfg.Limits.WeeklyCreditUSD,
		WeekSpent:       spent,
		MaxParallelRuns: cfg.Queue.MaxParallelRuns,
		Logf:            log.Printf,
	})
	if number == 0 {
		log.Print(reason)
		return nil
	}
	fmt.Printf("number=%d\nlane=%d\n", number, lane)
	return nil
}
