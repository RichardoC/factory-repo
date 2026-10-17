package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// planOpts is the planner the tests decide in: a $10 weekly credit, the
// three default labels, two parallel runs, and the issue-#61 example
// thresholds (backlog under 3, one plan run per 12 hours).
func planOpts() planOptions {
	return planOptions{
		QueuedLabel:     "codeaf-queued",
		Label:           "codeaf",
		WeekCap:         10,
		MaxParallelRuns: 2,
		MinBacklog:      3,
		MinHours:        12,
	}
}

// planIssue builds an open issue carrying labels, in creation order.
func planIssue(n int, labels ...string) item {
	it := item{Number: n, State: "open"}
	for _, name := range labels {
		it.Labels = append(it.Labels, label{Name: name})
	}
	return it
}

// planRun builds a run with a title, a created_at stamp relative to now and
// a status.
func planRun(status, title, createdAt string, dbid int) runStatus {
	return runStatus{Status: status, DatabaseId: dbid, DisplayTitle: title, CreatedAt: createdAt}
}

// stampAgo is an RFC3339 stamp age hours before the test ran.
func stampAgo(hours float64) string {
	return time.Now().UTC().Add(-time.Duration(hours * float64(time.Hour))).Format(time.RFC3339)
}

// TestPlanCheckBacklog covers the backlog threshold: at planner.min_backlog
// queued issues the planner holds, below it the plan run fires.
func TestPlanCheckBacklog(t *testing.T) {
	three := []item{
		planIssue(1, "codeaf-queued"),
		planIssue(2, "codeaf-queued"),
		planIssue(3, "codeaf-queued"),
	}
	if _, ok, reason := PlanCheck(three, nil, planOpts()); ok || !strings.Contains(reason, "backlog sufficient (3 queued, need fewer than 3)") {
		t.Fatalf("backlog 3: ok=%v reason=%q; want hold", ok, reason)
	}
	if _, ok, _ := PlanCheck(three[:2], nil, planOpts()); !ok {
		t.Fatal("backlog 2: want a plan run")
	}
	// The empty backlog is the whole point of the feature.
	if lane, ok, _ := PlanCheck(nil, nil, planOpts()); !ok || lane != 0 {
		t.Fatalf("empty backlog: lane=%d ok=%v; want lane 0", lane, ok)
	}
}

// TestPlanCheckBacklogFilters covers what is not backlog: PRs, issues
// already labelled codeaf, closed issues, and issues without the queued
// label never count toward the threshold.
func TestPlanCheckBacklogFilters(t *testing.T) {
	pr := planIssue(9, "codeaf-queued")
	pr.PullRequest = []byte(`{"number":9}`)
	running := planIssue(4, "codeaf-queued", "codeaf")
	closed := planIssue(5, "codeaf-queued")
	closed.State = "closed"
	unlabelled := planIssue(6)
	issues := []item{pr, running, closed, unlabelled}
	// None of the four counts: the backlog is empty, so the plan run fires
	// even though five issues carry the queued label in total.
	if _, ok, reason := PlanCheck(issues, nil, planOpts()); !ok {
		t.Fatalf("filtered backlog: ok=%v reason=%q; want a plan run", ok, reason)
	}
	// Two real backlog issues plus all four non-counting entries: still
	// below the threshold of 3.
	two := append(issues, planIssue(1, "codeaf-queued"), planIssue(2, "codeaf-queued"))
	if _, ok, _ := PlanCheck(two, nil, planOpts()); !ok {
		t.Fatal("two real backlog issues: want a plan run")
	}
	// A third real one tips it over.
	three := append(two, planIssue(3, "codeaf-queued"))
	if _, ok, _ := PlanCheck(three, nil, planOpts()); ok {
		t.Fatal("three real backlog issues: want a hold")
	}
}

// TestPlanCheckCooldown covers the cooldown: a plan run that started within
// planner.min_hours holds the next one, one that started before it does
// not, and a plan-title run whose created_at is missing or unparseable
// counts as inside the cooldown (fail closed). Completed runs count too:
// the cooldown is about when the last plan ran, not about what it did since.
func TestPlanCheckCooldown(t *testing.T) {
	issues := []item{planIssue(1, "codeaf-queued"), planIssue(2, "codeaf-queued")} // below the threshold
	planTitle := func(status, createdAt string) []runStatus {
		return []runStatus{planRun(status, "CodeAF plan", createdAt, 100)}
	}
	if _, ok, _ := PlanCheck(issues, planTitle("completed", stampAgo(13)), planOpts()); !ok {
		t.Fatal("13h old plan run: cooldown elapsed, want a plan run")
	}
	if _, ok, reason := PlanCheck(issues, planTitle("completed", stampAgo(11)), planOpts()); ok || !strings.Contains(reason, "plan cooldown active") {
		t.Fatalf("11h old plan run: ok=%v reason=%q; want hold", ok, reason)
	}
	// Exactly at min_hours the cooldown has elapsed.
	if _, ok, _ := PlanCheck(issues, planTitle("completed", stampAgo(12)), planOpts()); !ok {
		t.Fatal("exactly 12h old: want a plan run")
	}
	// A queued or in-progress plan run is inside its own cooldown by
	// construction.
	for _, status := range []string{"queued", "in_progress"} {
		if _, ok, reason := PlanCheck(issues, planTitle(status, stampAgo(0)), planOpts()); ok || !strings.Contains(reason, "plan cooldown active") {
			t.Fatalf("%s plan run: ok=%v reason=%q; want hold", status, ok, reason)
		}
	}
	// Fail closed on a stamp that is missing or not RFC3339.
	for _, bad := range []string{"", "yesterday", "2024-13-45T99:99:99Z"} {
		if _, ok, reason := PlanCheck(issues, planTitle("completed", bad), planOpts()); ok || !strings.Contains(reason, "plan cooldown active") {
			t.Fatalf("created_at %q: ok=%v reason=%q; want hold", bad, ok, reason)
		}
	}
	// An issue run of any age is no plan run: no cooldown.
	issueRun := []runStatus{planRun("completed", "CodeAF #7 (lane 0)", stampAgo(1), 101)}
	if _, ok, _ := PlanCheck(issues, issueRun, planOpts()); !ok {
		t.Fatal("recent issue run: not a plan run, want a plan run")
	}
}

// TestPlanCheckLanes covers the lane side: the plan run is one of the
// MaxParallelRuns in-flight runs, takes the first lane no run occupies, and
// holds when the capacity is full — plan runs and issue runs alike. A plan
// run in flight holds no areas, so an issue run's area never blocks the
// plan dispatch.
func TestPlanCheckLanes(t *testing.T) {
	issues := []item{planIssue(1, "codeaf-queued")}
	// One issue run in flight on lane 0, on the same area the queued issue
	// names: the plan run still dispatches, on the one free lane.
	busy := []runStatus{planRun("in_progress", "CodeAF #7 (lane 0)", stampAgo(1), 101)}
	if lane, ok, _ := PlanCheck(issues, busy, planOpts()); !ok || lane != 1 {
		t.Fatalf("one issue run on lane 0: lane=%d ok=%v; want lane 1", lane, ok)
	}
	// One plan run in flight, started an hour ago: it is inside the
	// cooldown — two plan runs are kept apart by the cooldown, not by
	// areas — so the dispatch holds even with a lane free.
	aPlan := []runStatus{planRun("in_progress", "CodeAF plan", stampAgo(1), 102)}
	if _, ok, reason := PlanCheck(issues, aPlan, planOpts()); ok || !strings.Contains(reason, "plan cooldown active") {
		t.Fatalf("fresh plan run in flight: ok=%v reason=%q; want hold", ok, reason)
	}
	// A long-running plan run (started past the cooldown) still occupies
	// its lane: with lane 0 taken, the plan run takes lane 1.
	aPlan = []runStatus{planRun("in_progress", "CodeAF plan", stampAgo(20), 102)}
	if lane, ok, _ := PlanCheck(issues, aPlan, planOpts()); !ok || lane != 1 {
		t.Fatalf("old plan run on lane 0: lane=%d ok=%v; want lane 1", lane, ok)
	}
	// The same without a lane suffix: "CodeAF plan" parses to lane 0.
	aPlan0 := []runStatus{planRun("queued", "CodeAF plan", stampAgo(20), 103)}
	if lane, ok, _ := PlanCheck(issues, aPlan0, planOpts()); !ok || lane != 1 {
		t.Fatalf("plan run lane 0 (no suffix): lane=%d ok=%v; want lane 1", lane, ok)
	}
	// Two in flight, of any mix of titles: at the capacity, held.
	full := []runStatus{
		planRun("queued", "CodeAF plan (lane 1)", stampAgo(20), 104),
		planRun("in_progress", "CodeAF #7 (lane 0)", stampAgo(1), 101),
	}
	if _, ok, reason := PlanCheck(issues, full, planOpts()); ok || reason != "lane capacity full" {
		t.Fatalf("two in flight: ok=%v reason=%q; want lane capacity full", ok, reason)
	}
	// Completed runs do not fill a lane.
	done := []runStatus{
		planRun("completed", "CodeAF plan (lane 0)", stampAgo(20), 104),
		planRun("completed", "CodeAF #7 (lane 1)", stampAgo(20), 101),
	}
	if lane, ok, _ := PlanCheck(issues, done, planOpts()); !ok || lane != 0 {
		t.Fatalf("completed runs: lane=%d ok=%v; want lane 0", lane, ok)
	}
	// An unparsable title is lane 0: it occupies lane 0 without ever being
	// assigned one, so the plan run starts on lane 1.
	mystery := []runStatus{planRun("in_progress", "someone else's run", stampAgo(1), 105)}
	if lane, ok, _ := PlanCheck(issues, mystery, planOpts()); !ok || lane != 1 {
		t.Fatalf("unparsable title: lane=%d ok=%v; want lane 1", lane, ok)
	}
}

// TestPlanCheckSpendThreshold covers the spend guard: the same
// queueSpendFraction as Next, and the same refusal reason.
func TestPlanCheckSpendThreshold(t *testing.T) {
	issues := []item{planIssue(1, "codeaf-queued")}
	o := planOpts()   // weekly credit $10
	o.WeekSpent = 7.5 // exactly 0.75 of the credit
	if _, ok, reason := PlanCheck(issues, nil, o); ok || reason != "spend threshold reached" {
		t.Fatalf("at the threshold: ok=%v reason=%q", ok, reason)
	}
	o.WeekSpent = 7.4999
	if _, ok, _ := PlanCheck(issues, nil, o); !ok {
		t.Fatal("just under the threshold: want a plan run")
	}
}

// TestPlanCheckOrder covers the fixed order of the checks: the spend guard
// first, then the lane, then the backlog, then the cooldown. Each refusal
// names the check that stopped the dispatch.
func TestPlanCheckOrder(t *testing.T) {
	issues := []item{planIssue(1, "codeaf-queued")}
	o := planOpts()
	o.WeekSpent = 9
	full := []runStatus{
		planRun("queued", "CodeAF plan (lane 1)", stampAgo(0), 104),
		planRun("in_progress", "CodeAF #7 (lane 0)", stampAgo(1), 101),
	}
	// Spend first even with everything else refusing too.
	if _, ok, reason := PlanCheck(issues, full, o); ok || reason != "spend threshold reached" {
		t.Fatalf("spend wins: reason=%q", reason)
	}
	// Lane second, even with a full backlog and a fresh plan run.
	o.WeekSpent = 0
	freshBacklog := []item{
		planIssue(1, "codeaf-queued"),
		planIssue(2, "codeaf-queued"),
		planIssue(3, "codeaf-queued"),
	}
	if _, ok, reason := PlanCheck(freshBacklog, full, o); ok || reason != "lane capacity full" {
		t.Fatalf("lane wins: reason=%q", reason)
	}
	// Backlog third, before the cooldown.
	o.MaxParallelRuns = 4 // lane free now
	if _, ok, reason := PlanCheck(freshBacklog, full, o); ok || !strings.Contains(reason, "backlog sufficient") {
		t.Fatalf("backlog wins: reason=%q", reason)
	}
}

// TestPlanCheckDeterministic runs the same decision twice and wants the
// same lane and reason: the pick never depends on the order the runs
// arrived in.
func TestPlanCheckDeterministic(t *testing.T) {
	issues := []item{planIssue(1, "codeaf-queued")}
	runs := []runStatus{
		planRun("in_progress", "CodeAF plan (lane 1)", stampAgo(20), 104),
		planRun("in_progress", "CodeAF #7 (lane 0)", stampAgo(1), 101),
	}
	o := planOpts()
	o.MaxParallelRuns = 3 // lanes 0 and 1 are taken; the plan run takes 2
	first := func() (int, bool, string) {
		return PlanCheck(issues, runs, o)
	}
	laneA, okA, reasonA := first()
	laneB, okB, reasonB := first()
	if laneA != laneB || okA != okB || reasonA != reasonB {
		t.Fatalf("PlanCheck not deterministic: (%d,%v,%q) then (%d,%v,%q)", laneA, okA, reasonA, laneB, okB, reasonB)
	}
	if laneA != 2 || !okA {
		t.Fatalf("PlanCheck = (%d,%v,%q); want lane 2, ok", laneA, okA, reasonA)
	}
}

// planCheckStdout runs runPlanCheck against the two files and returns what
// it printed on stdout, with its log output (the reason) discarded.
func planCheckStdout(t *testing.T, issuesPath, runsPath string) (string, error) {
	t.Helper()
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	oldLog := log.Writer()
	log.SetOutput(devnull)
	defer log.SetOutput(oldLog)

	oldArgs := os.Args
	os.Args = []string{"harness", "plan-check", issuesPath, runsPath}
	defer func() { os.Args = oldArgs }()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStdout := os.Stdout
	os.Stdout = w
	out := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(r)
		out <- string(data)
	}()
	err = runPlanCheck()
	os.Stdout = oldStdout
	w.Close()
	return <-out, err
}

// writePlanRuns writes a run list to a temp file and returns its path.
func writePlanRuns(t *testing.T, runs []runStatus) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runs.json")
	data, err := json.Marshal(runs)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// writePlanIssues writes an issue list to a temp file and returns its path.
// The JSON is built by hand, like TestRunNext's fixtures: an issue carries
// no pull_request field, and a round-trip through json.Marshal would write
// "pull_request":null, which the queue (and the planner) read as a PR —
// `gh api` omits the field for plain issues.
func writePlanIssues(t *testing.T, issues []item) string {
	t.Helper()
	var b strings.Builder
	b.WriteByte('[')
	for i, it := range issues {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"number":%d,"state":%q,"labels":[`, it.Number, it.State)
		for j, l := range it.Labels {
			if j > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `{"name":%q}`, l.Name)
		}
		b.WriteString("]}")
	}
	b.WriteByte(']')
	path := filepath.Join(t.TempDir(), "issues.json")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRunPlanCheck covers the CLI shape of `harness plan-check ISSUES_JSON
// RUNS_JSON`: on a dispatch exactly "plan=1" then "lane=<digits>" — the
// GITHUB_OUTPUT lines the queue job writes — and on a hold nothing at all,
// with the command exiting 0 either way. The week's spend total is
// required, as for `harness next`.
func TestRunPlanCheck(t *testing.T) {
	t.Setenv("PROXY_MODELS", "models.json")
	t.Setenv("CODEAF_WEEK_SPENT_USD", "1")
	low := writePlanIssues(t, []item{planIssue(1, "codeaf-queued"), planIssue(2, "codeaf-queued")})
	full := writePlanIssues(t, []item{
		planIssue(1, "codeaf-queued"),
		planIssue(2, "codeaf-queued"),
		planIssue(3, "codeaf-queued"),
	})
	lane0 := writePlanRuns(t, []runStatus{planRun("in_progress", "CodeAF #7 (lane 0)", stampAgo(1), 101)})

	// The default models.json sets min_backlog 3 and max_parallel_runs 2:
	// two queued issues is below the backlog, one run in flight puts the
	// plan run on lane 1.
	out, err := planCheckStdout(t, low, lane0)
	if err != nil || out != "plan=1\nlane=1\n" {
		t.Fatalf("planCheckStdout = %q, %v; want plan=1, lane=1", out, err)
	}
	// A hold prints nothing and still exits 0.
	if out, err := planCheckStdout(t, full, lane0); err != nil || out != "" {
		t.Fatalf("hold: planCheckStdout = %q, %v; wanted nothing", out, err)
	}
	// A fresh plan run trips the cooldown: also a hold, nothing printed.
	fresh := writePlanRuns(t, []runStatus{planRun("completed", "CodeAF plan", stampAgo(1), 104)})
	if out, err := planCheckStdout(t, low, fresh); err != nil || out != "" {
		t.Fatalf("cooldown: planCheckStdout = %q, %v; wanted nothing", out, err)
	}
	// Without the week's spend total the command refuses, like `harness
	// next`.
	t.Setenv("CODEAF_WEEK_SPENT_USD", "")
	if _, err := planCheckStdout(t, low, lane0); err == nil || !errors.Is(err, err) && !strings.Contains(err.Error(), "CODEAF_WEEK_SPENT_USD") {
		t.Fatalf("missing CODEAF_WEEK_SPENT_USD: err=%v; want a refusal", err)
	}
	// Bad arguments are a usage error.
	oldArgs := os.Args
	os.Args = []string{"harness", "plan-check", "only-one"}
	if err := runPlanCheck(); err == nil || !strings.Contains(err.Error(), "usage: harness plan-check") {
		t.Fatalf("bad args: err=%v; want usage", err)
	}
	os.Args = oldArgs
	// Malformed inputs are errors, never a silent dispatch.
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := planCheckStdout(t, bad, lane0); err == nil {
		t.Fatal("runPlanCheck accepted malformed issues JSON")
	}
	if _, err := planCheckStdout(t, low, bad); err == nil {
		t.Fatal("runPlanCheck accepted malformed runs JSON")
	}
}

// TestPlanCheckLogf checks that every refusal is explained through Logf,
// like Next explains its holds: a hold leaves a line naming the check.
func TestPlanCheckLogf(t *testing.T) {
	issues := []item{planIssue(1, "codeaf-queued"), planIssue(2, "codeaf-queued"), planIssue(3, "codeaf-queued")}
	var lines []string
	o := planOpts()
	o.Logf = func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	if _, ok, _ := PlanCheck(issues, nil, o); ok {
		t.Fatal("want a hold")
	}
	if len(lines) == 0 || !strings.Contains(strings.Join(lines, "\n"), "backlog sufficient") {
		t.Fatalf("Logf got %v; want a backlog-sufficient line", lines)
	}
	// A nil Logf discards: no panic, same decision.
	o.Logf = nil
	lines = nil
	if _, ok, _ := PlanCheck(issues, nil, o); ok {
		t.Fatal("nil Logf: want the same hold")
	}
}

// TestPlanCheckRunStampFromGhJSON guards the JSON key of runStatus.CreatedAt
// (issue #61): the queue job feeds plan-check the `gh run list --json ...
// createdAt` array, whose keys are camelCase — as are the struct's other
// tags. A snake_case tag would compile and pass every test that builds a
// runStatus in Go while leaving the stamp empty on the real path, and an
// empty stamp on a plan run is inside the cooldown: the planner would hold
// forever and never dispatch. This test unmarshals the exact shape the job
// writes, so the tag cannot drift from the data.
func TestPlanCheckRunStampFromGhJSON(t *testing.T) {
	// What `gh run list --json databaseId,status,displayTitle,createdAt`
	// prints, and what the job mirrors in after a plan dispatch.
	const ghJSON = `[
		{"databaseId":42,"status":"completed","displayTitle":"CodeAF plan","createdAt":"2026-10-17T06:30:00Z"},
		{"databaseId":43,"status":"completed","displayTitle":"CodeAF #7 (lane 1)","createdAt":"2026-10-17T07:00:00Z"}
	]`
	var runs []runStatus
	if err := json.Unmarshal([]byte(ghJSON), &runs); err != nil {
		t.Fatal(err)
	}
	if runs[0].CreatedAt != "2026-10-17T06:30:00Z" {
		t.Fatalf("plan run created_at = %q; want the stamp from the JSON — the tag must be createdAt", runs[0].CreatedAt)
	}
	if !isPlanRun(runs[0]) {
		t.Fatalf("displayTitle %q not read as a plan run", runs[0].DisplayTitle)
	}
	// The stamp parses as RFC3339, so the cooldown is measurable: a plan run
	// older than the cooldown does not hold the next one.
	issues := []item{planIssue(1, "codeaf-queued")}
	o := planOpts()
	if _, err := time.Parse(time.RFC3339, runs[0].CreatedAt); err != nil {
		t.Fatalf("plan run stamp %q is not RFC3339: %v", runs[0].CreatedAt, err)
	}
	old := time.Now().UTC().Add(-time.Duration(24) * time.Hour).Format(time.RFC3339)
	runs[0].CreatedAt = old
	if _, ok, reason := PlanCheck(issues, runs, o); !ok {
		t.Fatalf("a day-old plan run: ok=%v reason=%q; want a plan run", ok, reason)
	}
	// And a fresh one holds.
	runs[0].CreatedAt = time.Now().UTC().Format(time.RFC3339)
	if _, ok, reason := PlanCheck(issues, runs, o); ok || !strings.Contains(reason, "plan cooldown active") {
		t.Fatalf("a fresh plan run: ok=%v reason=%q; want a hold", ok, reason)
	}
}
