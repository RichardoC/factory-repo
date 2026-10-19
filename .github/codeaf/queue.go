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
	// PRHeads holds, by open pull request number, the pull request's
	// current head SHA, fetched by the caller from /pulls (issue #215). A
	// stranded re-pick is recorded against this SHA, so Next re-picks a PR
	// only when its SHA is here: without it the re-pick could be neither
	// recorded nor bounded, and the PR waits for a later tick. An absent or
	// non-hex entry means unknown — no re-pick.
	PRHeads map[int]string
	// PRMarkers holds, by pull request number, every head SHA a re-pick
	// marker comment on the PR already names (issue #215). The comments are
	// untrusted data: the caller keeps only full 40-hex SHAs. A PR whose
	// current head is in the set has already been re-picked at that commit
	// and is skipped until its head moves.
	PRMarkers map[int][]string
	// IssueRetries holds, by issue number, the stranded issues (issue #240)
	// eligible for their one bounded re-pick: open, non-PR issues labelled
	// codeaf whose latest bot comment is a failed run's report and which
	// carry no retry marker yet. The caller classifies the comments and
	// fills the map; Next itself only reads it. An issue in the map is
	// re-picked like a queued issue — the usual gates (dependencies,
	// readiness, areas) run again, since the repo may have moved on since
	// the first run. Issues not in the map keep dropping out of the pick
	// as before: a second failure is a maintainer hand-off, not a retry.
	IssueRetries map[int]bool
	// NoAreaHeld, when non-nil, is called once per candidate held because it
	// names no area (reason "no area label", issue #132). Like Logf, it is
	// the caller's side effect; Next's pick never depends on it.
	NoAreaHeld func(number int)
	// NotReadyHeld, when non-nil, is called once per candidate held because
	// it is not ready (reason "not ready: ...", issue #136), with the
	// gate's own fixed missing-item names. Like Logf, it is the caller's
	// side effect; Next's pick never depends on it.
	NotReadyHeld func(number int, missing []string)
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

// pick is one run the queue decides to start: what kind of run it is, the
// issue or pull request number it works on, and the lane it runs on. Kind is
// one of the kind* constants below; a zero pick (an empty Kind) means the
// queue is holding.
type pick struct {
	Kind   string
	Number int
	Lane   int
}

// The kinds of run the queue schedules (issue #137): a queued issue, a
// review of a pull request, or a continuation of one.
const (
	kindIssue    = "issue"
	kindReview   = "review"
	kindContinue = "continue"
)

// The labels that put a pull request in the queue (issue #137): publish adds
// codeaf-review-needed or codeaf-continue-needed instead of dispatching the
// workflow itself, and the queue picks the pull request up from the label.
const (
	labelReviewNeeded   = "codeaf-review-needed"
	labelContinueNeeded = "codeaf-continue-needed"
)

// startReason is the log phrase for a started run of each kind.
func startReason(kind string) string {
	switch kind {
	case kindReview:
		return "review requested"
	case kindContinue:
		return "continuation requested"
	}
	return "next queued issue"
}

// Next picks the run CodeAF should start next and the lane it should run on.
// Candidates, in this order per pass (issue #137): open pull requests
// labelled codeaf and codeaf-review-needed (kind review), then open pull
// requests labelled codeaf and codeaf-continue-needed (kind continue), then
// the queued issues as before (kind issue). A pull request stranded by a
// failed publish — open, labelled codeaf, none of the queue labels, no run
// in progress for it — joins the reviews (issue #215), once per head commit:
// the caller fetches its head SHA and the SHA any re-pick marker comment
// names, and a PR whose head still matches its marker is skipped. An open
// issue stranded by a failed run (issue #240) — labelled codeaf, no open
// pull request closing it, no run in progress for it, its latest bot
// comment a failed run's report — joins the queued issues for its single
// bounded re-pick; the caller classifies its comments into IssueRetries,
// and a second failure goes to a maintainer instead. A pull
// request is a candidate only while it is open and no in-flight run already
// has its number as its subject; pull request candidates keep the queue's
// usual sort (oldest first). Next returns a zero pick when nothing should run: fewer than
// MaxParallelRuns CodeAF runs may be queued or in progress (they count
// alike, every kind against the same lanes), the rolling week's credit spend
// must stay under the threshold, and the pick is the first candidate whose
// "Depends on" references (issues only) are all closed and whose areas
// collide with no run in flight. A review holds no area and never conflicts;
// a continuation holds its pull request's areas exactly like the run it
// continues. The queue logs the runs in progress with their lanes, and why
// it held or started each candidate, through Logf — including the final
// reason when nothing is started, which the caller used to print. The pick
// and the lane are deterministic; Next is pure except for Logf.
func Next(issues []item, runs []runStatus, o queueOptions) pick {
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
	// 0) and holds the areas of the issue or pull request it is working on.
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
		logf("spend threshold reached")
		return pick{}
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
	// The candidates of one pass, in the fixed order: reviews, then
	// continuations, then queued issues. A PR stranded by a failed publish
	// (issue #215) joins the reviews: it carries the codeaf label, none of
	// the queue labels, and no run is in progress for it, so the review run
	// decides whether a continuation is needed.
	var reviews, continuations, queued []item
	repicks := map[int]string{} // stranded re-picks, by PR number: its head SHA
	for _, i := range issues {
		switch {
		case i.State != "open":
		case len(i.PullRequest) > 0:
			// A pull request is a candidate only while no in-flight run
			// already has it as its subject (issue #137).
			if !i.labelled(o.Label) || prInFlight(i.Number, inflight) {
				continue
			}
			switch {
			case i.labelled(labelReviewNeeded):
				reviews = append(reviews, i)
			case i.labelled(labelContinueNeeded):
				continuations = append(continuations, i)
			case strandedPR(i, o, inflight):
				// The re-pick is bounded to once per head commit: the
				// marker comment on the PR names the SHA the last re-pick
				// was for, and the PR is skipped while its head still
				// matches. Both sides are untrusted data, and the guard
				// fails closed: an unknown head or an unreadable set of
				// comments means no re-pick this tick — an unrecorded
				// re-pick could repeat on every tick.
				head := strings.ToLower(o.PRHeads[i.Number])
				if !fullSHARx.MatchString(head) || o.PRMarkers == nil {
					continue
				}
				if slices.Contains(o.PRMarkers[i.Number], head) {
					logf("holding #%d: stranded re-pick already recorded at %s", i.Number, head)
					continue
				}
				reviews = append(reviews, i)
				repicks[i.Number] = head
			}
		case i.labelled(o.Label) && !i.labelled(o.QueuedLabel):
			// Stranded (issue #240): the run this label once marked has
			// failed, and without a re-pick the issue would drop out of
			// the factory for good. The caller classified its comments;
			// only a first failure joins the queue here, for its one
			// bounded re-pick, and under the usual gates like any queued
			// issue. A second failure is a maintainer hand-off, never
			// another retry. An issue carrying both labels is not
			// stranded — it falls through to the cases below and stays
			// "already picked up".
			if o.IssueRetries[i.Number] {
				queued = append(queued, i)
			}
		case !i.labelled(o.QueuedLabel):
		case i.labelled(o.Label): // already picked up
		default:
			queued = append(queued, i)
		}
	}
	if len(reviews)+len(continuations)+len(queued) == 0 {
		logf("nothing queued")
		return pick{}
	}
	less := func(a, b item) int { return queueLess(a, b, o.PriorityLabel) }
	slices.SortFunc(reviews, less)
	slices.SortFunc(continuations, less)
	slices.SortFunc(queued, less)
	stuck := ""
	hold := func(reason string) {
		if stuck == "" {
			stuck = reason
		}
	}
	// consider weighs one candidate and, if it starts, returns its pick.
	consider := func(c item, kind string) pick {
		if len(inflight) >= mpr {
			logf("holding #%d: lane capacity full", c.Number)
			hold("lane capacity full")
			return pick{}
		}
		areas := []string(nil)
		switch kind {
		case kindIssue:
			if n := openDep(depsFromBody(cappedBody(c.Body)), open); n > 0 {
				logf("holding #%d: dependencies open for #%d", c.Number, n)
				hold(fmt.Sprintf("dependencies open for #%d", n))
				return pick{}
			}
			// The acceptance gate (issue #132 for the area, #136 for the
			// body), run as one check: an issue missing both the area and
			// the sections is held with a reason - and a hold comment -
			// that names everything it is missing, not just the first
			// thing the gate noticed.
			if missing := issueReady(c); len(missing) > 0 {
				if slices.Equal(missing, []string{"area label"}) {
					// An issue that names no area is held, not started,
					// and blocks no one (issue #132): it is skipped like
					// any other hold, and the next candidate is still
					// considered.
					logf("holding #%d: no area label", c.Number)
					hold("no area label")
					if o.NoAreaHeld != nil {
						o.NoAreaHeld(c.Number)
					}
					return pick{}
				}
				reason := "not ready: " + strings.Join(missing, ", ")
				logf("holding #%d: %s", c.Number, reason)
				hold(reason)
				if o.NotReadyHeld != nil {
					o.NotReadyHeld(c.Number, missing)
				}
				return pick{}
			}
			areas = issueAreas(c)
			if len(areas) == 0 {
				// An issue that names no area is held, not started, and
				// blocks no one (issue #132): it is skipped like any other
				// hold, and the next candidate is still considered.
				logf("holding #%d: no area label", c.Number)
				hold("no area label")
				if o.NoAreaHeld != nil {
					o.NoAreaHeld(c.Number)
				}
				return pick{}
			}
		case kindContinue:
			// A continuation holds its pull request's areas exactly like
			// the run it continues (issue #137).
			areas = prAreas(c.Number, byNumber, o.PRFiles)
		}
		// A review holds no area and never conflicts; for the other kinds
		// an empty set on either side conflicts with everything.
		if kind != kindReview {
			if busy, r, conflict := areaBusy(areas, inflight, byNumber, o.PRFiles); conflict {
				reason := fmt.Sprintf("area %s busy (run %d)", busy, r.DatabaseId)
				if len(areas) == 0 {
					reason = fmt.Sprintf("no area; conflicts with run %d", r.DatabaseId)
				}
				logf("holding #%d: %s", c.Number, reason)
				hold(reason)
				return pick{}
			}
		}
		// The capacity check above leaves a lane free: fewer than mpr runs
		// are in flight, each occupying one lane value, so the scan always
		// finds one. Lanes outside [0, mpr) are occupied but never assigned.
		lane := 0
		for ; lane < mpr && lanes[lane]; lane++ {
		}
		if sha, ok := repicks[c.Number]; ok {
			// Issue #215: the re-pick logs its own line, naming the commit
			// it was for.
			logf("re-picking stranded PR #%d at %s", c.Number, sha)
		}
		logf("starting #%d (lane %d): kind=%s, %s", c.Number, lane, kind, startReason(kind))
		return pick{Kind: kind, Number: c.Number, Lane: lane}
	}
	for _, c := range reviews {
		if p := consider(c, kindReview); p.Kind != "" {
			return p
		}
	}
	for _, c := range continuations {
		if p := consider(c, kindContinue); p.Kind != "" {
			return p
		}
	}
	for _, c := range queued {
		if p := consider(c, kindIssue); p.Kind != "" {
			return p
		}
	}
	logf("%s", stuck)
	return pick{}
}

// prInFlight reports whether an in-flight run already has pull request n as
// its subject (issue #137): a `CodeAF #n` run — a continuation or a manual
// pull request run — or a `CodeAF review #n` run. A review title parses to
// issue 0 in runTitleFields, so the review shape is matched on its own.
func prInFlight(n int, inflight []runStatus) bool {
	for _, r := range inflight {
		if m, _ := runTitleFields(r.DisplayTitle); m == n {
			return true
		}
		if m, _, ok := runReviewFields(r.DisplayTitle); ok && m == n {
			return true
		}
	}
	return false
}

// strandedPR reports whether an open pull request is a stranded re-pick
// candidate (issue #215): it carries the codeaf label, none of the queue
// labels — review-needed, continue-needed, review-passed — and no CodeAF run
// is in progress for it. A run that failed at publish leaves a PR exactly
// there: the queue removed continue-needed when it dispatched, and nothing
// put a label back, so without this the PR would drop out of the factory.
func strandedPR(i item, o queueOptions, inflight []runStatus) bool {
	if i.State != "open" || len(i.PullRequest) == 0 {
		return false
	}
	if !i.labelled(o.Label) || prInFlight(i.Number, inflight) {
		return false
	}
	return !i.labelled(labelReviewNeeded) && !i.labelled(labelContinueNeeded) &&
		!i.labelled(reviewPassedLabel)
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

// runReviewRe matches the display title a review dispatch writes (issue
// #137): "CodeAF review #5" or "CodeAF review #5 (lane 1)". Run titles are
// untrusted data, so the match is anchored and strict, like runTitleRe:
// anything else — another prefix, a lane with letters in it, trailing prose
// — parses to no review run.
var runReviewRe = regexp.MustCompile(`^CodeAF review #([0-9]+)(?: \(lane ([0-9]+)\))?$`)

// runReviewFields reads the PR number and lane out of a review run's display
// title. ok is false for every title that is not a review title, issue
// titles included — the shapes are disjoint, so a review run is never
// matched by runTitleRe and parses to issue 0 there.
func runReviewFields(title string) (number, lane int, ok bool) {
	m := runReviewRe.FindStringSubmatch(title)
	if m == nil {
		return 0, 0, false
	}
	number, _ = strconv.Atoi(m[1])
	// m[2] is empty when the suffix is absent; that is lane 0 too.
	lane, _ = strconv.Atoi(m[2])
	return number, lane, true
}

// isReviewRun reports whether a run was dispatched in review mode (issue
// #137). Like a plan run, a review run occupies its lane but works on no
// area, so it never conflicts by area.
func isReviewRun(r runStatus) bool {
	_, _, ok := runReviewFields(r.DisplayTitle)
	return ok
}

// runLane is the lane a run was dispatched on, read out of its title. Plan
// runs, review runs and issue runs share the lane numbering: each occupies
// its lane like any other run, which is what keeps MaxParallelRuns honest.
func runLane(r runStatus) int {
	if lane, ok := runPlanFields(r.DisplayTitle); ok {
		return lane
	}
	if _, lane, ok := runReviewFields(r.DisplayTitle); ok {
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

// issueReady reports what an issue still needs before the queue may start it
// (issue #136): an area label or a body path naming one, a "## Done when"
// section with at least one "- " bullet under it, and a "## End-to-end
// check" section with some real text. The names are the gate's own fixed
// strings — never issue text — so they are safe to log and to post as a
// comment. Headings match case-insensitively at levels 1 to 3, and the body
// is scanned only up to maxBodyScan.
func issueReady(it item) (missing []string) {
	if len(issueAreas(it)) == 0 {
		missing = append(missing, "area label")
	}
	return append(missing, issueBodyReady(it)...)
}

// issueBodyReady is the body half of the acceptance gate (issue #136): the
// two section checks of issueReady without the area check, so Next can name
// the area and the sections in one hold. Issues whose bodies are generated
// — ref updates and model experiments — are exempt from it, not from the
// area check: their body is written by the harness, and refIssueBody
// renders the same acceptance sections into it for the people who read the
// issue. The queue never reads those sections on a generated body.
func issueBodyReady(it item) (missing []string) {
	if it.labelled(refUpdateLabel) || it.labelled(experimentLabel) {
		return nil // generated body: the body checks do not apply
	}
	body := cappedBody(it.Body)
	if done, ok := issueSection(body, issueDoneWhenHeading); !ok || !issueHasBullet(done) {
		missing = append(missing, "## Done when bullets")
	}
	e2e, ok := issueSection(body, issueEndToEndHeading)
	if !ok || e2eUnfilled(e2e) {
		missing = append(missing, "## End-to-end check")
	}
	return missing
}

// e2eUnfilled reports whether the text of an "## End-to-end check" section
// is not a filled-in check (issue #136): empty once HTML comments are
// stripped, or still carrying part of the unfilled placeholder. A check
// hidden in a comment — the template's instructions, a reviewer's note —
// is not a check (issue #136 review): the comment is stripped before the
// text is judged. An unclosed "<!--" comments out the rest of the section,
// so everything from it on is comment too. The placeholder marks are read
// on the visible text, so a filled-in check that quotes the placeholder
// inside a comment still passes.
func e2eUnfilled(e2e string) bool {
	visible := stripHTMLComments(e2e)
	if strings.TrimSpace(visible) == "" {
		return true
	}
	return holdsE2EPlaceholder(visible)
}

// stripHTMLComments removes whole HTML comments — "<!--" through the first
// "-->" — and, when a comment is never closed, everything from the "<!--"
// to the end, which is how markdown reads it. What is left is the text a
// reader sees.
func stripHTMLComments(s string) string {
	i := strings.Index(s, "<!--")
	if i < 0 {
		return s
	}
	j := strings.Index(s[i:], "-->")
	if j < 0 {
		return s[:i] // unclosed: the rest of the section is comment
	}
	return stripHTMLComments(s[:i] + s[i+j+len("-->"):])
}

// e2ePlaceholderMarks are the fixed phrases that mark an "## End-to-end
// check" section as not filled in (issue #136): the placeholder that
// ProposalBody and issue-template.md render begins with the first phrase
// and ends with the second, so a truncated or half-edited copy of it still
// carries one of them. A section that carries any of them holds the issue,
// so a promoted proposal or a fresh template issue is never queued
// unfilled. They are the gate's own fixed strings — never issue text — so
// they are safe to match against an arbitrary body.
var e2ePlaceholderMarks = []string{"fill in before", "the seams work)"}

// holdsE2EPlaceholder reports whether the text of an "## End-to-end check"
// section still carries part of the unfilled placeholder.
func holdsE2EPlaceholder(e2e string) bool {
	for _, mark := range e2ePlaceholderMarks {
		if strings.Contains(e2e, mark) {
			return true
		}
	}
	return false
}

// issueAnyHeading is any markdown heading line, of any level: it ends the
// section before it.
var issueAnyHeading = regexp.MustCompile(`^#{1,6}(\s|$)`)

// issueDoneWhenHeading and issueEndToEndHeading match the two acceptance
// headings (issue #136), case-insensitive, at levels 1 to 3 ("#", "##",
// "###"), with the exact heading text and nothing else on the line.
var (
	issueDoneWhenHeading = regexp.MustCompile(`(?i)^#{1,3}[ \t]+done[ \t]+when[ \t]*$`)
	issueEndToEndHeading = regexp.MustCompile(`(?i)^#{1,3}[ \t]+end-to-end[ \t]+check[ \t]*$`)
)

// issueSection returns the text of the first section whose heading matches
// head — the lines from the heading to the next heading of any level — and
// whether the heading was there at all.
func issueSection(body string, head *regexp.Regexp) (text string, found bool) {
	var lines []string
	in := false
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if in {
			if issueAnyHeading.MatchString(line) {
				break
			}
			lines = append(lines, line)
			continue
		}
		if head.MatchString(line) {
			in = true
		}
	}
	return strings.Join(lines, "\n"), in
}

// issueHasBullet reports whether any line of the section is a "- " bullet.
func issueHasBullet(section string) bool {
	for _, line := range strings.Split(section, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "- ") {
			return true
		}
	}
	return false
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
	return prAreas(n, byNumber, prFiles)
}

// prAreas is the area set a pull request holds (issue #132, #137): the issue
// it closes (`Closes #N`, looked up in byNumber), united with the areas of
// its changed files from prFiles and its own area: labels, if any. The queue
// reads it for an in-flight pull request run through runAreas, and for a
// continuation candidate — which holds its pull request's areas before the
// run starts — straight from the candidate. An empty set after all three
// sources conflicts with everything, fail closed.
func prAreas(n int, byNumber map[int]item, prFiles map[int][]string) []string {
	it, ok := byNumber[n]
	if !ok {
		return nil
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
// conflicts with everything — except a plan run (issue #61) or a review run
// (issue #137): each occupies its lane but works on no area, so neither
// conflicts with an issue run. (Two plan runs are kept apart by the
// planner's cooldown, not by areas.) When the collision is an intersection,
// the first (sorted) shared area is named.
func areaBusy(areas []string, inflight []runStatus, byNumber map[int]item, prFiles map[int][]string) (busy string, r runStatus, conflict bool) {
	for _, run := range inflight {
		if isPlanRun(run) || isReviewRun(run) {
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
// issue, review or continuation to run and prints the pick on stdout, which
// the queue job writes straight into GITHUB_OUTPUT. On a start that is
// exactly three lines — "kind=<kind>", then "number=<n>", then
// "lane=<digits>" — and nothing else (issue #137); when the queue holds,
// stdout stays empty and the command still exits 0. stderr carries the human
// log: the runs in progress with their lanes, and why each candidate was
// held or started, including the final reason when nothing is. The queue job
// labels the pick to start the run, which keeps the trust gate: only the
// workflow's own token labels.
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
	// With a token (the queue job sets GH_TOKEN), make sure every app has
	// its area label before the pick runs (issue #139): a new app's first
	// issues then queue instead of stalling. Best effort, never fatal.
	if gh.token != "" {
		if err := ensureAreaLabels(gh, log.Printf); err != nil {
			log.Printf("area labels: %v", err)
		}
	}
	prFiles := fetchPRFiles(gh, issues, runs, log.Printf)
	var noArea []int
	notReady := map[int][]string{}
	opts := queueOptions{
		Label:           envOr("CODEAF_LABEL", "codeaf"),
		QueuedLabel:     envOr("CODEAF_QUEUED_LABEL", "codeaf-queued"),
		PriorityLabel:   envOr("CODEAF_PRIORITY_LABEL", "codeaf-priority"),
		WeekCap:         cfg.Limits.WeeklyCreditUSD,
		WeekSpent:       spent,
		MaxParallelRuns: cfg.Queue.MaxParallelRuns,
		PRFiles:         prFiles,
		NoAreaHeld:      func(n int) { noArea = append(noArea, n) },
		NotReadyHeld:    func(n int, missing []string) { notReady[n] = missing },
		Logf:            log.Printf,
	}
	// The stranded re-pick (issue #215) reads each candidate's head SHA and
	// marker comments here, so Next stays pure. With no token the maps stay
	// empty and no stranded PR is re-picked.
	if gh.token != "" {
		opts.PRHeads, opts.PRMarkers = fetchStrandedPRs(gh, issues, runs, opts.Label, log.Printf)
	}
	// The stranded-issue re-pick (issue #240) reads each candidate's
	// comments here, so Next stays pure. With no token the map stays empty
	// and no stranded issue is re-picked or escalated.
	var issueEscalations []int
	if gh.token != "" {
		opts.IssueRetries, issueEscalations = fetchStrandedIssues(gh, issues, runs, opts.Label, opts.QueuedLabel, log.Printf)
	}
	p := Next(issues, runs, opts)
	// One comment per held issue, posted once: the marker check keeps a
	// repeat tick from stacking copies (issue #132, #136). Best effort: a
	// failed comment never fails the pick.
	if gh.token != "" {
		for _, n := range noArea {
			if err := ensureHoldCommentBody(gh, n, holdCommentBody(n)); err != nil {
				log.Printf("hold comment on #%d: %v", n, err)
			}
		}
		for n, missing := range notReady {
			if err := ensureHoldCommentBody(gh, n, notReadyHoldCommentBody(n, missing)); err != nil {
				log.Printf("hold comment on #%d: %v", n, err)
			}
		}
	}
	// The maintainer hand-off (issue #240): label first, then the fixed
	// comment — the comment failing then still leaves the label on, and
	// the next tick retries the comment because the marker is missing.
	// Best effort: a failed escalation never fails the pick.
	for _, n := range issueEscalations {
		if err := ensureMaintainerEscalation(gh, n); err != nil {
			log.Printf("maintainer hand-off on #%d: %v", n, err)
		}
	}
	// The re-pick marker (issue #215): record the head SHA the re-pick was
	// for, so the next tick skips the PR until its head moves. Best effort:
	// a failed comment never fails the pick.
	if gh.token != "" && p.Kind == kindReview {
		if sha, ok := opts.PRHeads[p.Number]; ok {
			if err := ensureRepickMarker(gh, p.Number, sha); err != nil {
				log.Printf("re-pick marker on #%d: %v", p.Number, err)
			}
		}
	}
	// The stranded-issue re-pick marker (issue #240): record that the one
	// retry is spent, so the next failed run hands the issue to a
	// maintainer instead of looping. Best effort: a failed comment never
	// fails the pick.
	if gh.token != "" && p.Kind == kindIssue && opts.IssueRetries[p.Number] {
		if err := ensureIssueRetryMarker(gh, p.Number); err != nil {
			log.Printf("re-pick marker on #%d: %v", p.Number, err)
		}
	}
	if p.Kind == "" {
		return nil
	}
	fmt.Printf("kind=%s\nnumber=%d\nlane=%d\n", p.Kind, p.Number, p.Lane)
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
// everything, fail closed. Review titles (`CodeAF review #N`, issue #137) do
// not parse through runTitleFields and are skipped here: a review holds no
// area, so it needs no files.
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

// appDirNameRe matches the app directory names the area labels are built
// from (issue #139): anything else in the listing is ignored, so an entry
// from the API — untrusted data — can never become a label name.
var appDirNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// maxAreaApps caps how many directories the label sweep reads from the
// listing, so an API reply that names thousands of entries cannot make the
// queue tick post for all of them.
const maxAreaApps = 50

// ensureAreaLabels makes sure a label `area:app/<name>` exists for every
// app directory (issue #139), so a new app's first issues do not stall for
// want of a hand-made label. The queue job checks out only .github/codeaf,
// so the app names come from the API, not the filesystem. Best effort by
// design: an unreadable listing, a too-long listing, or a label post that
// fails is logged and never fails the pick; a 422 "already_exists" reply
// means the label is there and counts as success.
func ensureAreaLabels(g github, logf func(string, ...any)) error {
	if g.token == "" {
		return nil
	}
	var entries []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	if err := g.get("/contents/app", &entries); err != nil {
		logf("area labels: reading app/ contents: %v", err)
		return nil
	}
	if len(entries) == 0 {
		logf("area labels: nothing under app/ to label")
		return nil
	}
	for i, e := range entries {
		if i >= maxAreaApps {
			logf("area labels: stopped at the %d-entry cap", maxAreaApps)
			break
		}
		if e.Type != "dir" || !appDirNameRe.MatchString(e.Name) {
			continue
		}
		body := map[string]string{
			"name":        "area:app/" + e.Name,
			"color":       "cccccc",
			"description": "CodeAF area: app/" + e.Name,
		}
		if err := g.post("/labels", body, nil); err != nil {
			// The label already exists: fine, that is the steady state on
			// every tick after the first. The error text carries the status
			// (POST /labels: 422 ...).
			text := err.Error()
			if strings.Contains(text, "422") || strings.Contains(text, "already_exists") {
				continue
			}
			logf("area labels: %v", err)
		}
	}
	return nil
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

// notReadyHoldCommentBody is the comment the queue posts on an issue held
// because it is not ready (issue #136). The missing names are the gate's
// own fixed strings — issue #132's rule that no issue text goes into these
// comments holds here too.
func notReadyHoldCommentBody(n int, missing []string) string {
	return fmt.Sprintf("%s\nCodeAF's queue is holding this issue (#%d): it is not ready to run. Missing: %s. Add what is missing — a `## Done when` section with bullets, a `## End-to-end check` section, an `area:` label (see the issue template) — and the next queue tick will start it.", queueHoldMarker, n, strings.Join(missing, ", "))
}

// queueRepickMarker begins the hidden comment the queue posts when it
// re-picks a stranded pull request (issue #215): the comment names the head
// SHA the re-pick was for, and keeps the re-pick to once per commit.
const queueRepickMarker = "<!-- codeaf-queue-repick "

// repickSHARe finds the head SHA inside a re-pick marker comment. Comments
// are untrusted data: only a full 40-hex SHA is accepted, and a comment
// whose marker carries anything else — truncated, upper prose, someone
// quoting the marker — is ignored as if it were absent.
var repickSHARe = regexp.MustCompile(`codeaf-queue-repick[ \t]+([0-9a-fA-F]{40})`)

// fullSHARx matches a full 40-character hex git SHA. Both sides of the
// re-pick guard are untrusted data — the marker comment and the API's own
// head field — so each must prove its shape before it is compared.
var fullSHARx = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

// bodyComments is what /issues/N/comments decodes into when only the comment
// bodies are read.
type bodyComments []struct {
	Body string `json:"body"`
	User struct {
		Type string `json:"type"`
	} `json:"user"`
}

// strandedByFailedRun reports whether a PR's comments end in a failed CodeAF
// run: its latest bot comment, the re-pick markers aside, is a run report
// whose outcome is "publishing failed" or "run job failed". That is the one
// state the re-pick exists for (issue #215). Every other way a PR ends with no
// queue label is a deliberate hand-off to a person -- a spent fix budget, an
// unrecognised severity, an incomplete review -- and re-reviewing those would
// keep asking until a review passed. Only bot comments count, so a comment
// anyone can post on a public repository cannot fake a failed run.
func strandedByFailedRun(comments bodyComments) bool {
	for i := len(comments) - 1; i >= 0; i-- {
		c := comments[i]
		if c.User.Type != "Bot" || strings.Contains(c.Body, queueRepickMarker) {
			continue
		}
		if !strings.HasPrefix(c.Body, reportMarker+"\n") {
			return false
		}
		return strings.HasPrefix(c.Body, reportMarker+"\n### CodeAF run — publishing failed\n") ||
			strings.HasPrefix(c.Body, reportMarker+"\n### CodeAF run — run job failed\n")
	}
	return false
}

// repickMarkers collects the head SHAs the re-pick marker comments name, in
// comment order, lowercased. A malformed marker is ignored as if absent
// (issue #215).
func repickMarkers(comments bodyComments) []string {
	var shas []string
	for _, c := range comments {
		if m := repickSHARe.FindStringSubmatch(c.Body); m != nil {
			shas = append(shas, strings.ToLower(m[1]))
		}
	}
	return shas
}

// repickMarkerBody is the re-pick marker comment's fixed text (issue #215):
// the marker with the SHA, then text built from the PR number alone — no PR
// or issue text, untrusted data, goes into these comments.
func repickMarkerBody(n int, sha string) string {
	return fmt.Sprintf("%s%s -->\nCodeAF's queue re-picked this stranded pull request (#%d) for review at commit %s: it carried no queue label and no CodeAF run was in progress for it. The re-pick runs once per commit; push new commits and the queue picks the pull request again.", queueRepickMarker, sha, n, sha)
}

// ensureRepickMarker posts the re-pick marker comment on pull request n
// unless a marker naming sha is already there (issue #215); the marker keeps
// repeat ticks from posting the comment twice for the same commit.
func ensureRepickMarker(g github, n int, sha string) error {
	var comments bodyComments
	if err := g.get(fmt.Sprintf("/issues/%d/comments?per_page=100", n), &comments); err != nil {
		return err
	}
	if slices.Contains(repickMarkers(comments), sha) {
		return nil
	}
	return g.post(fmt.Sprintf("/issues/%d/comments", n), map[string]string{"body": repickMarkerBody(n, sha)}, nil)
}

// fetchStrandedPRs fetches what the stranded re-pick (issue #215) needs for
// every candidate the issues list names: the pull request's current head SHA
// from /pulls, and the head SHAs any marker comment on it already names. The
// maps feed Next through queueOptions, so the pick stays pure. A candidate
// whose head cannot be read, or whose comments cannot be, gets no entries
// and is not re-picked this tick: the guard fails closed, since a re-pick
// that cannot be recorded could repeat on every tick.
func fetchStrandedPRs(g github, issues []item, runs []runStatus, lbl string, logf func(string, ...any)) (heads map[int]string, markers map[int][]string) {
	heads = map[int]string{}
	markers = map[int][]string{}
	var inflight []runStatus
	for _, r := range runs {
		if r.Status == "queued" || r.Status == "in_progress" {
			inflight = append(inflight, r)
		}
	}
	for _, i := range issues {
		if !strandedPR(i, queueOptions{Label: lbl}, inflight) {
			continue
		}
		var pr item
		if err := g.get(fmt.Sprintf("/pulls/%d", i.Number), &pr); err != nil {
			logf("stranded PR #%d head: %v", i.Number, err)
			continue
		}
		head := strings.ToLower(pr.Head.SHA)
		if !fullSHARx.MatchString(head) {
			logf("stranded PR #%d: head %q is not a 40-hex SHA", i.Number, pr.Head.SHA)
			continue
		}
		var comments bodyComments
		if err := g.get(fmt.Sprintf("/issues/%d/comments?per_page=100", i.Number), &comments); err != nil {
			logf("stranded PR #%d comments: %v", i.Number, err)
			continue
		}
		if !strandedByFailedRun(comments) {
			// No label because a person was handed the PR, not because a
			// run failed: leave it to them.
			continue
		}
		heads[i.Number] = head
		markers[i.Number] = repickMarkers(comments)
	}
	return heads, markers
}

// ensureHoldComment posts the area hold comment on issue n unless a comment
// carrying the marker is already there.
func ensureHoldComment(g github, n int) error {
	return ensureHoldCommentBody(g, n, holdCommentBody(n))
}

// ensureHoldCommentBody posts body as the hold comment on issue n unless a
// comment carrying the marker is already there; the marker keeps repeat
// ticks from posting the comment twice.
func ensureHoldCommentBody(g github, n int, body string) error {
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
	return g.post(fmt.Sprintf("/issues/%d/comments", n), map[string]string{"body": body}, nil)
}

// ---- Stranded issues (issue #240) -------------------------------------
//
// A failed run strands its issue: the queue swaps codeaf-queued for codeaf
// when the run starts, and a codeaf label means "already picked up", so the
// issue drops out of the factory for good. The re-pick below gives every
// failure one bounded retry and then hands the issue to a maintainer — the
// same shape as the stranded pull request re-pick (issue #215), with one
// difference the PR path does not need: the retry is once per issue, not
// once per commit, so its marker is remembered across the whole scan.

// queueIssueRetryMarker begins the hidden comment the queue posts when it
// re-picks a stranded issue: the comment records that the one retry is
// spent, and keeps the retry to once.
const queueIssueRetryMarker = "<!-- codeaf-queue-issue-retry -->"

// queueIssueMaintainerMarker begins the hidden comment the queue posts when
// it hands a stranded issue to a maintainer: the marker keeps repeat ticks
// from adding the label or posting the comment twice.
const queueIssueMaintainerMarker = "<!-- codeaf-queue-issue-maintainer -->"

// maintainerLabel goes on an issue the queue stopped retrying: a person has
// to look at it now.
const maintainerLabel = "codeaf-maintainer"

// workflowPolicyRefusal is the phrase a publish refusal carries when the
// change breaks the workflow policy. A run refused on policy grounds fails
// identically on every retry, so it is never retried at all.
const workflowPolicyRefusal = "breaks the workflow policy"

// What the queue does with a stranded issue, as strandedIssueState reports
// it: strandNone leaves the issue where it is, strandRetry re-picks it
// once, strandMaintainer hands it to a maintainer.
const (
	strandNone       = ""
	strandRetry      = "retry"
	strandMaintainer = "maintainer"
)

// strandedIssueState reads what happened to a stranded issue out of its
// comments: its latest bot comment, the queue's own marker comments aside,
// is a run report, and the report's outcome decides. A failure report on a
// first failure is a retry; a failure after the retry, or a publish refusal
// that names the workflow policy — retrying would repeat the same refusal —
// goes to a maintainer. Everything else leaves the issue alone: a run that
// ended well ("done", "changes pushed", "no changes"), a latest bot comment
// that is no run report, or no bot comments at all.
//
// Comments are untrusted data: only bot-authored comments count, so a
// comment anyone can post on a public repository cannot fake a failed run
// or spend the retry, and the report shape is matched as strictly as
// strandedByFailedRun's. The marker comments are skipped in the scan but
// not forgotten — a retry marker anywhere in the history is what makes the
// next failure report a maintainer case, whichever order the comments came
// in.
func strandedIssueState(comments bodyComments) string {
	retried, escalated := false, false
	for _, c := range comments {
		if c.User.Type != "Bot" {
			continue
		}
		if strings.Contains(c.Body, queueIssueRetryMarker) {
			retried = true
		}
		if strings.Contains(c.Body, queueIssueMaintainerMarker) {
			escalated = true
		}
	}
	for i := len(comments) - 1; i >= 0; i-- {
		c := comments[i]
		if c.User.Type != "Bot" || strings.Contains(c.Body, queueIssueRetryMarker) ||
			strings.Contains(c.Body, queueIssueMaintainerMarker) || strings.Contains(c.Body, queueRepickMarker) {
			continue
		}
		if !strings.HasPrefix(c.Body, reportMarker+"\n") {
			return strandNone
		}
		failed := strings.HasPrefix(c.Body, reportMarker+"\n### CodeAF run — publishing failed\n") ||
			strings.HasPrefix(c.Body, reportMarker+"\n### CodeAF run — run job failed\n")
		if !failed {
			return strandNone
		}
		if escalated || retried || strings.Contains(c.Body, workflowPolicyRefusal) {
			return strandMaintainer
		}
		return strandRetry
	}
	return strandNone
}

// issueRetryBody is the re-pick comment's fixed text (issue #240): the
// marker, then text built from the issue number alone — no issue text,
// untrusted data, goes into these comments.
func issueRetryBody(n int) string {
	return fmt.Sprintf("%s\nCodeAF's queue re-picked this stranded issue (#%d) once: its last CodeAF run failed and no run is in progress for it. If the run fails again, the queue hands the issue to a maintainer instead of retrying it.", queueIssueRetryMarker, n)
}

// issueMaintainerBody is the maintainer hand-off's one fixed comment (issue
// #240), the same text for every hand-off — the run failed again after its
// retry, or publishing refused the change under the workflow policy. Built
// from the issue number alone.
func issueMaintainerBody(n int) string {
	return fmt.Sprintf("%s\nCodeAF's queue is handing this issue (#%d) to a maintainer: its latest CodeAF run failed in a way retrying will not fix — the run failed again after its one retry, or publishing refused the change under the workflow policy. The issue carries the `codeaf-maintainer` label and needs a person to look at it.", queueIssueMaintainerMarker, n)
}

// fetchStrandedIssues fetches what the stranded-issue re-pick (issue #240)
// needs for every candidate the issues list names: the comments of each
// open, non-PR issue that carries the codeaf label, none of the queue
// labels, has no run in progress for it, and has no open pull request
// closing it — that pull request's review or continuation owns the work
// now. The classification feeds Next through queueOptions, so the pick
// stays pure. A candidate whose comments cannot be read is skipped: the
// guard fails closed, since a retry that cannot be recorded could repeat
// on every tick. Issues already handed to a maintainer stay in the
// escalations list, so the label and its comment stay ensured;
// ensureMaintainerEscalation keeps both to once.
func fetchStrandedIssues(g github, issues []item, runs []runStatus, lbl, queuedLbl string, logf func(string, ...any)) (retries map[int]bool, escalations []int) {
	retries = map[int]bool{}
	var inflight []runStatus
	for _, r := range runs {
		if r.Status == "queued" || r.Status == "in_progress" {
			inflight = append(inflight, r)
		}
	}
	// Open pull requests by the issue they close — the `Closes #N` line the
	// publish step writes first in every PR body. Bodies are untrusted data
	// of unbounded size, so they are read through cappedBody like every
	// other body scan.
	prFor := map[int]bool{}
	for _, it := range issues {
		if it.State == "open" && len(it.PullRequest) > 0 {
			if n := linkedIssue(cappedBody(it.Body)); n > 0 {
				prFor[n] = true
			}
		}
	}
	for _, i := range issues {
		if i.State != "open" || len(i.PullRequest) > 0 {
			continue
		}
		if !i.labelled(lbl) || i.labelled(queuedLbl) || prInFlight(i.Number, inflight) || prFor[i.Number] {
			continue
		}
		var comments bodyComments
		if err := g.get(fmt.Sprintf("/issues/%d/comments?per_page=100", i.Number), &comments); err != nil {
			logf("stranded issue #%d comments: %v", i.Number, err)
			continue
		}
		switch strandedIssueState(comments) {
		case strandRetry:
			retries[i.Number] = true
		case strandMaintainer:
			escalations = append(escalations, i.Number)
		}
	}
	return retries, escalations
}

// ensureIssueRetryMarker posts the re-pick marker comment on issue n unless
// a comment carrying the marker is already there (issue #240); the marker
// keeps the retry to once, whatever later ticks observe.
func ensureIssueRetryMarker(g github, n int) error {
	var comments []struct {
		Body string `json:"body"`
	}
	if err := g.get(fmt.Sprintf("/issues/%d/comments?per_page=100", n), &comments); err != nil {
		return err
	}
	for _, c := range comments {
		if strings.Contains(c.Body, queueIssueRetryMarker) {
			return nil
		}
	}
	return g.post(fmt.Sprintf("/issues/%d/comments", n), map[string]string{"body": issueRetryBody(n)}, nil)
}

// ensureMaintainerEscalation hands stranded issue n to a maintainer (issue
// #240): the codeaf-maintainer label first — the comment failing then still
// leaves the label on, and the next tick re-posts the comment because the
// marker is missing — then the one fixed comment, keyed on its marker. A
// repeat tick with the marker served posts nothing more.
func ensureMaintainerEscalation(g github, n int) error {
	var comments []struct {
		Body string `json:"body"`
	}
	if err := g.get(fmt.Sprintf("/issues/%d/comments?per_page=100", n), &comments); err != nil {
		return err
	}
	for _, c := range comments {
		if strings.Contains(c.Body, queueIssueMaintainerMarker) {
			return nil
		}
	}
	if err := g.post(fmt.Sprintf("/issues/%d/labels", n), map[string][]string{"labels": {maintainerLabel}}, nil); err != nil {
		return err
	}
	return g.post(fmt.Sprintf("/issues/%d/comments", n), map[string]string{"body": issueMaintainerBody(n)}, nil)
}
