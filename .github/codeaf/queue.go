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
// `gh run list --workflow=codeaf.yml --json status,databaseId,displayTitle,createdAt`
// array the queue reads. DatabaseId and DisplayTitle are untrusted data: the
// title is parsed strictly, and anything that does not match the queue's own
// shape counts as lane 0 and no known issue. `gh run list --json` prints
// camelCase keys, and the tags below must match them: a created_at tag would
// silently drop the stamp, and the planner would read every plan run as
// inside its cooldown.
type runStatus struct {
	Status       string `json:"status"`
	DatabaseId   int    `json:"databaseId"`
	DisplayTitle string `json:"displayTitle"`
	// CreatedAt is the run's creation stamp (the JSON key `createdAt`), from
	// `gh run list --json createdAt`: when the run started. The planner reads
	// it for its cooldown; a missing or unparseable stamp makes a plan run
	// count as inside the cooldown (fail closed).
	CreatedAt string `json:"createdAt"`
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
	// PRFiles holds the changed-file paths of the open pull requests
	// in-flight runs are working on, by PR number (issue #132): a PR run's
	// title names the PR, and a PR item carries no area of its own, so the
	// queue reads what the PR touched. The paths are raw filenames; runAreas
	// maps each through pathArea. runNext fills the map from the API, and a
	// fetch failure leaves the entry absent; Next itself only reads it.
	PRFiles map[int][]string
	// NoAreaHeld, when non-nil, is called once per candidate held because it
	// names no area (reason "no area label", issue #132). Like Logf, it is
	// the caller's side effect; Next's pick never depends on it.
	NoAreaHeld func(number int)
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
		if len(areas) == 0 {
			// An issue that names no area is held, not started, and blocks
			// no one (issue #132): it is skipped like any other hold, and
			// the next candidate is still considered.
			logf("holding #%d: no area label", c.Number)
			hold("no area label")
			if o.NoAreaHeld != nil {
				o.NoAreaHeld(c.Number)
			}
			continue
		}
		if busy, r, conflict := areaBusy(areas, inflight, byNumber, o.PRFiles); conflict {
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

// runPlanRe matches the display title a plan dispatch writes (issue #61):
// "CodeAF plan" or "CodeAF plan (lane N)". Like runTitleRe it is anchored
// and strict: run titles are untrusted data, and anything else parses to no
// plan run.
var runPlanRe = regexp.MustCompile(`^CodeAF plan(?: \(lane ([0-9]+)\))?$`)

// runPlanFields reads the lane out of a plan run's display title. ok is
// false for every title that is not a plan title, issue titles included —
// the two shapes are disjoint, so a plan run is never matched by runTitleRe
// and parses to issue 0 there.
func runPlanFields(title string) (lane int, ok bool) {
	m := runPlanRe.FindStringSubmatch(title)
	if m == nil {
		return 0, false
	}
	// m[1] is empty when the suffix is absent; that is lane 0 too.
	lane, _ = strconv.Atoi(m[1])
	return lane, true
}

// isPlanRun reports whether a run was dispatched in plan mode (issue #61).
func isPlanRun(r runStatus) bool {
	_, ok := runPlanFields(r.DisplayTitle)
	return ok
}

// runLane is the lane a run was dispatched on, read out of its title. Plan
// runs and issue runs share the lane numbering: a plan run occupies its
// lane like any other run, which is what keeps MaxParallelRuns honest.
func runLane(r runStatus) int {
	if lane, ok := runPlanFields(r.DisplayTitle); ok {
		return lane
	}
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
	labelAreas(it, add)
	for _, p := range bodyPaths(cappedBody(it.Body)) {
		add(p)
	}
	slices.Sort(areas)
	return areas
}

// labelAreas adds an item's area labels — area:harness and area:app/<name> —
// to add. Shared by issueAreas and runAreas, which reads a PR's own labels.
func labelAreas(it item, add func(string)) {
	for _, l := range it.Labels {
		switch {
		case l.Name == "area:"+areaHarness:
			add(areaHarness)
		case strings.HasPrefix(l.Name, "area:app/"):
			add(strings.TrimPrefix(l.Name, "area:"))
		}
	}
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
//
// A run on a pull request (a review, a continuation or a fix run, titled
// `CodeAF #<pr>`; issue #132) takes its areas from three sources, united:
// the issue the PR closes (`Closes #N`, looked up in byNumber), the PR's
// changed files from prFiles, and the PR's own area: labels, if any. A PR
// item itself names no area, and without the union every review run would
// conflict with everything. An empty set after all three sources still
// conflicts with everything, as above.
func runAreas(r runStatus, byNumber map[int]item, prFiles map[int][]string) []string {
	n, _ := runTitleFields(r.DisplayTitle)
	if n <= 0 {
		return nil
	}
	it, ok := byNumber[n]
	if !ok {
		return nil
	}
	if len(it.PullRequest) == 0 {
		return issueAreas(it)
	}
	var areas []string
	seen := map[string]bool{}
	add := func(a string) {
		if a != "" && !seen[a] {
			seen[a] = true
			areas = append(areas, a)
		}
	}
	if m := linkedIssue(it.Body); m > 0 {
		if linked, ok := byNumber[m]; ok {
			for _, a := range issueAreas(linked) {
				add(a)
			}
		}
	}
	for _, p := range prFiles[n] {
		add(pathArea(p))
	}
	labelAreas(it, add)
	slices.Sort(areas)
	return areas
}

// areaBusy finds the first in-flight run whose areas collide with the
// candidate's, and the area to name in the log. An empty set on either side
// conflicts with everything — except a plan run (issue #61): a plan run
// occupies its lane but works on no area, so it never conflicts with an
// issue run, and two plan runs are kept apart by the planner's cooldown,
// not by areas. When the collision is an intersection, the first (sorted)
// shared area is named.
func areaBusy(areas []string, inflight []runStatus, byNumber map[int]item, prFiles map[int][]string) (busy string, r runStatus, conflict bool) {
	for _, run := range inflight {
		if isPlanRun(run) {
			continue
		}
		other := runAreas(run, byNumber, prFiles)
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
	// A PR run's areas come from its changed files (issue #132), fetched
	// here so Next stays pure. With no token the map stays empty and the
	// runs' area sets fall back to the issues the PRs close.
	gh := newGitHub()
	prFiles := fetchPRFiles(gh, issues, runs, log.Printf)
	var noArea []int
	number, lane, reason := Next(issues, runs, queueOptions{
		Label:           envOr("CODEAF_LABEL", "codeaf"),
		QueuedLabel:     envOr("CODEAF_QUEUED_LABEL", "codeaf-queued"),
		PriorityLabel:   envOr("CODEAF_PRIORITY_LABEL", "codeaf-priority"),
		WeekCap:         cfg.Limits.WeeklyCreditUSD,
		WeekSpent:       spent,
		MaxParallelRuns: cfg.Queue.MaxParallelRuns,
		PRFiles:         prFiles,
		NoAreaHeld:      func(n int) { noArea = append(noArea, n) },
		Logf:            log.Printf,
	})
	// One comment per held issue, posted once: the marker check keeps a
	// repeat tick from stacking copies (issue #132). Best effort: a failed
	// comment never fails the pick.
	if gh.token != "" {
		for _, n := range noArea {
			if err := ensureHoldComment(gh, n); err != nil {
				log.Printf("hold comment on #%d: %v", n, err)
			}
		}
	}
	if number == 0 {
		log.Print(reason)
		return nil
	}
	fmt.Printf("number=%d\nlane=%d\n", number, lane)
	return nil
}

// maxPRFiles caps how many changed paths are read per pull request: one
// page of 100, matching the API's own page size.
const maxPRFiles = 100

// fetchPRFiles fetches the changed-file paths of every open pull request an
// in-flight run is working on (issue #132): a PR item carries no area of its
// own, so the queue reads what the PR touched. A number whose item is not an
// open PR in the issues list is skipped, and a fetch failure leaves the
// entry absent — which makes that run's area set empty and conflict with
// everything, fail closed.
func fetchPRFiles(g github, issues []item, runs []runStatus, logf func(string, ...any)) map[int][]string {
	prFiles := map[int][]string{}
	if g.token == "" {
		return prFiles
	}
	byNumber := map[int]item{}
	for _, it := range issues {
		if it.State == "open" {
			byNumber[it.Number] = it
		}
	}
	seen := map[int]bool{}
	for _, r := range runs {
		if r.Status != "queued" && r.Status != "in_progress" {
			continue
		}
		n, _ := runTitleFields(r.DisplayTitle)
		if n <= 0 || seen[n] {
			continue
		}
		it, ok := byNumber[n]
		if !ok || len(it.PullRequest) == 0 {
			continue
		}
		seen[n] = true
		var files []prFile
		if err := g.get(fmt.Sprintf("/pulls/%d/files?per_page=%d", n, maxPRFiles), &files); err != nil {
			logf("pr files for #%d: %v", n, err)
			continue
		}
		if len(files) > maxPRFiles {
			files = files[:maxPRFiles]
		}
		paths := make([]string, 0, len(files))
		for _, f := range files {
			paths = append(paths, f.Filename)
		}
		prFiles[n] = paths
	}
	return prFiles
}

// queueHoldMarker marks the comment the queue posts on an issue it holds for
// want of an area label (issue #132); the marker keeps repeat ticks from
// posting the comment twice.
const queueHoldMarker = "<!-- codeaf-queue-hold -->"

// holdCommentBody is the comment's fixed text, built from the issue number
// alone: no issue text — untrusted data — goes into it.
func holdCommentBody(n int) string {
	return fmt.Sprintf("%s\nCodeAF's queue is holding this issue (#%d): it carries no `area:harness` or `area:app/<name>` label and its body names no path under one. Add an area label to queue it (see [running several tickets at once](https://github.com/RichardoC/factory-repo#running-several-tickets-at-once-issue-60)).", queueHoldMarker, n)
}

// ensureHoldComment posts the hold comment on issue n unless a comment
// carrying the marker is already there.
func ensureHoldComment(g github, n int) error {
	var comments []struct {
		Body string `json:"body"`
	}
	if err := g.get(fmt.Sprintf("/issues/%d/comments?per_page=100", n), &comments); err != nil {
		return err
	}
	for _, c := range comments {
		if strings.Contains(c.Body, queueHoldMarker) {
			return nil
		}
	}
	return g.post(fmt.Sprintf("/issues/%d/comments", n), map[string]string{"body": holdCommentBody(n)}, nil)
}
