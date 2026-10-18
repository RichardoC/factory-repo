package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// nextOpts is the queue the tests decide in: a $10 weekly credit and the
// three default labels.
func nextOpts() queueOptions {
	return queueOptions{
		Label:         "codeaf",
		QueuedLabel:   "codeaf-queued",
		PriorityLabel: "codeaf-priority",
		WeekCap:       10,
	}
}

// queuedIssue builds an open issue carrying labels, in creation order. Its
// body passes the acceptance gate (issue #136) by default — the sections are
// not what most of these tests are about — and a test that cares sets Body
// itself.
func queuedIssue(n int, created string, labels ...string) item {
	it := item{Number: n, State: "open", CreatedAt: created, Body: readyBody}
	for _, name := range labels {
		it.Labels = append(it.Labels, label{Name: name})
	}
	return it
}

// areaIssue builds a queued issue carrying an area: "harness" or "app/<name>".
func areaIssue(n int, created, area string) item {
	return queuedIssue(n, created, "codeaf-queued", "area:"+area)
}

// nextPick runs Next with a log collector and returns the pick with the log
// lines, for the tests that check why the queue held.
func nextPick(issues []item, runs []runStatus, o queueOptions) (pick, []string) {
	var lines []string
	o.Logf = func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	return Next(issues, runs, o), lines
}

// reviewRun builds an in-flight review run on PR n (issue #137).
func reviewRun(n, lane, dbid int) runStatus {
	return runStatus{Status: "in_progress", DatabaseId: dbid,
		DisplayTitle: fmt.Sprintf("CodeAF review #%d (lane %d)", n, lane)}
}

// running builds an in-flight run whose title names issue n on a lane.
func running(n, lane, dbid int) runStatus {
	return runStatus{Status: "in_progress", DatabaseId: dbid,
		DisplayTitle: fmt.Sprintf("CodeAF #%d (lane %d)", n, lane)}
}

func TestNextNothingQueued(t *testing.T) {
	pr := queuedIssue(9, "2024-01-01T00:00:00Z", "codeaf-queued")
	pr.PullRequest = json.RawMessage(`{"number":9}`)
	closed := queuedIssue(3, "2024-01-01T00:00:00Z", "codeaf-queued")
	closed.State = "closed"
	cases := []struct {
		name   string
		issues []item
	}{
		{"empty list", nil},
		{"no queued label", []item{queuedIssue(3, "2024-01-01T00:00:00Z")}},
		{"already labelled codeaf", []item{queuedIssue(3, "2024-01-01T00:00:00Z", "codeaf-queued", "codeaf")}},
		{"a pull request", []item{pr}},
		{"closed issue", []item{closed}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if p := Next(c.issues, nil, nextOpts()); p.Kind != "" || p.Number != 0 {
				t.Fatalf("Next = %+v; want a zero pick, nothing queued", p)
			}
		})
	}
}

// TestNextParallelRuns covers `harness next` with N runs in progress: with
// this test's max_parallel_runs 2 (built on purpose below, never read from
// models.json) the issue starts while fewer than two CodeAF
// runs are queued or in progress, and is held at two. Queued counts like
// in_progress; completed does not count. The candidate and the in-flight
// issues carry different areas, so only the capacity can hold it.
func TestNextParallelRuns(t *testing.T) {
	issues := []item{
		areaIssue(3, "2024-01-01T00:00:00Z", "app/x"),
		areaIssue(5, "2024-01-01T00:00:00Z", "app/y"),
		areaIssue(7, "2024-01-01T00:00:00Z", "app/z"),
	}
	o := nextOpts() // weekly credit $10
	o.MaxParallelRuns = 2

	if p := Next(issues, nil, o); p.Kind != kindIssue || p.Number != 3 || p.Lane != 0 {
		t.Fatalf("no runs: Next = %+v; want issue 3, lane 0", p)
	}
	// One in flight, queued or in_progress alike: the issue starts, on the
	// one free lane.
	for _, status := range []string{"queued", "in_progress"} {
		r := running(5, 0, 100)
		r.Status = status
		if p := Next(issues, []runStatus{r}, o); p.Kind != kindIssue || p.Number != 3 || p.Lane != 1 {
			t.Fatalf("%s run: Next = %+v; want issue 3, lane 1", status, p)
		}
	}
	// Two in flight: at the capacity, held.
	two := []runStatus{running(5, 0, 100), running(7, 1, 101)}
	if p, lines := nextPick(issues, two, o); p.Kind != "" || !slices.Contains(lines, "lane capacity full") {
		t.Fatalf("two runs: Next = %+v, log %v; want held, lane capacity full", p, lines)
	}
	// Completed runs do not count.
	done := []runStatus{{Status: "completed", DatabaseId: 100, DisplayTitle: "CodeAF #5 (lane 0)"},
		{Status: "completed", DatabaseId: 101, DisplayTitle: "CodeAF #7 (lane 1)"}}
	if p := Next(issues, done, o); p.Number != 3 {
		t.Fatalf("after completed runs Next = %+v; want 3", p)
	}
}

func TestNextSpendThreshold(t *testing.T) {
	issues := []item{areaIssue(3, "2024-01-01T00:00:00Z", "app/x")}
	o := nextOpts()   // weekly credit $10
	o.WeekSpent = 7.5 // exactly 0.75 of the credit
	if p, lines := nextPick(issues, nil, o); p.Kind != "" || !slices.Contains(lines, "spend threshold reached") {
		t.Fatalf("at the threshold Next = %+v, log %v", p, lines)
	}
	o.WeekSpent = 7.4999
	if p := Next(issues, nil, o); p.Number != 3 {
		t.Fatalf("just under the threshold Next = %+v", p)
	}
}

// The spend total comes from the shared state, which is untrusted, so
// spentWeek must refuse whatever does not parse as a finite total at least
// zero.
func TestNextSpentEnv(t *testing.T) {
	for _, bad := range []string{"abc", "-1", "NaN", "Inf", "-Inf", "1e400", "5 USD"} {
		t.Setenv("CODEAF_WEEK_SPENT_USD", bad)
		if _, err := spentWeek("CODEAF_WEEK_SPENT_USD"); err == nil {
			t.Errorf("spentWeek accepted %q", bad)
		}
	}
	t.Setenv("CODEAF_WEEK_SPENT_USD", "1.25")
	if spent, err := spentWeek("CODEAF_WEEK_SPENT_USD"); err != nil || spent != 1.25 {
		t.Fatalf("spentWeek(1.25) = %v, %v", spent, err)
	}
}

func TestNextDependsOn(t *testing.T) {
	candidate := queuedIssue(3, "2024-01-01T00:00:00Z", "codeaf-queued", "area:app/x")
	dep := queuedIssue(12, "2024-01-02T00:00:00Z")
	openPR := queuedIssue(13, "2024-01-02T00:00:00Z")
	openPR.PullRequest = json.RawMessage(`{"number":13}`)
	closedPR := openPR
	closedPR.State = "closed"
	selfDep := queuedIssue(5, "2024-01-01T00:00:00Z", "codeaf-queued")
	selfDep.Body = "Depends on #5"
	blocked := candidate
	blocked.Body = "Depends on #12\n" + readyBody
	dependsOnPR := candidate
	dependsOnPR.Body = "see #13; depends on #13\n" + readyBody
	unknownDep := candidate
	unknownDep.Body = "Depends on #99\n" + readyBody
	mixed := candidate
	mixed.Body = "Depends on #12, #13"
	other := queuedIssue(7, "2024-01-03T00:00:00Z", "codeaf-queued", "area:app/x")

	cases := []struct {
		name   string
		issues []item
		want   int
		reason string
	}{
		{"dependency open", []item{blocked, dep}, 0, "dependencies open for #12"},
		{"dependency closed", []item{blocked, closed(dep)}, 3, ""},
		{"dependency is an open PR", []item{dependsOnPR, openPR}, 0, "dependencies open for #13"},
		{"dependency is a closed PR", []item{dependsOnPR, closedPR}, 3, ""},
		{"self-dependency", []item{selfDep}, 0, "dependencies open for #5"},
		{"dependency not in the list", []item{unknownDep}, 3, ""},
		{"mixed dependencies, one open", []item{mixed, dep, closedPR}, 0, "dependencies open for #12"},
		{"blocked candidate is skipped", []item{blocked, dep, other}, 7, ""},
		{"lowercase depends on", []item{withBody(candidate, "later, depends on #12"), dep}, 0, "dependencies open for #12"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, lines := nextPick(c.issues, nil, nextOpts())
			if p.Number != c.want || (c.reason != "" && !slices.Contains(lines, c.reason)) {
				t.Fatalf("Next = %+v, log %v; want %d, %q", p, lines, c.want, c.reason)
			}
		})
	}
}

func closed(it item) item {
	it.State = "closed"
	return it
}

func withBody(it item, body string) item {
	it.Body = body
	return it
}

func TestNextOrdering(t *testing.T) {
	oldest := queuedIssue(7, "2024-01-01T00:00:00Z", "codeaf-queued", "area:app/x")
	newer := queuedIssue(6, "2024-06-01T00:00:00Z", "codeaf-queued", "area:app/x")
	priority := queuedIssue(9, "2024-09-01T00:00:00Z", "codeaf-queued", "codeaf-priority", "area:app/x")
	priorityOlder := queuedIssue(4, "2024-02-01T00:00:00Z", "codeaf-queued", "codeaf-priority", "area:app/x")
	twin := queuedIssue(5, "2024-03-01T00:00:00Z", "codeaf-queued", "area:app/x")
	twinLater := queuedIssue(11, "2024-03-01T00:00:00Z", "codeaf-queued", "area:app/x")
	badStamp := queuedIssue(1, "not-a-date", "codeaf-queued", "area:app/x")

	if p := Next([]item{newer, oldest}, nil, nextOpts()); p.Number != 7 {
		t.Fatalf("oldest first: Next = %+v", p)
	}
	if p := Next([]item{oldest, priority, newer}, nil, nextOpts()); p.Number != 9 {
		t.Fatalf("priority jumps the queue: Next = %+v", p)
	}
	if p := Next([]item{priority, priorityOlder}, nil, nextOpts()); p.Number != 4 {
		t.Fatalf("two priority issues, oldest first: Next = %+v", p)
	}
	if p := Next([]item{twinLater, twin}, nil, nextOpts()); p.Number != 5 {
		t.Fatalf("tie-break by number: Next = %+v", p)
	}
	if p := Next([]item{badStamp, newer}, nil, nextOpts()); p.Number != 6 {
		t.Fatalf("unparseable created_at sorts last: Next = %+v", p)
	}
	// The same input twice picks the same issue, whatever the order.
	a := Next([]item{newer, oldest, priority}, nil, nextOpts())
	b := Next([]item{priority, oldest, newer}, nil, nextOpts())
	if a.Number != 9 || b.Number != 9 {
		t.Fatalf("Next is not deterministic: %d then %d", a.Number, b.Number)
	}
}

func TestReadItems(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "issues.json")
	if err := os.WriteFile(path, []byte(`[{"number":3,"state":"open","created_at":"2024-01-01T00:00:00Z","labels":[{"name":"codeaf-queued"}]}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	issues, err := readItems(path)
	if err != nil || len(issues) != 1 || issues[0].Number != 3 || issues[0].CreatedAt != "2024-01-01T00:00:00Z" {
		t.Fatalf("readItems = %v, %v", issues, err)
	}
	if err := os.WriteFile(path, []byte(`{"number":3}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readItems(path); err == nil {
		t.Fatal("readItems accepted an object where an array belongs")
	}
	if _, err := readItems(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("readItems accepted a missing file")
	}
}

func TestReadRuns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runs.json")
	if err := os.WriteFile(path, []byte(`[{"status":"completed"},{"status":"queued"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	runs, err := readRuns(path)
	if err != nil || len(runs) != 2 || runs[1].Status != "queued" {
		t.Fatalf("readRuns = %v, %v", runs, err)
	}
	if err := os.WriteFile(path, []byte(`nope`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readRuns(path); err == nil {
		t.Fatal("readRuns accepted malformed JSON")
	}
}

// runNextStdout runs runNext against the two files and returns what it
// printed on stdout, with its log output (the reason) discarded.
func runNextStdout(t *testing.T, issuesPath, runsPath string) (string, error) {
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
	os.Args = []string{"harness", "next", issuesPath, runsPath}
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
	err = runNext()
	os.Stdout = oldStdout
	w.Close()
	return <-out, err
}

func TestRunNext(t *testing.T) {
	cfg, err := loadConfig("models.json")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROXY_MODELS", "models.json")

	dir := t.TempDir()
	issuesPath := filepath.Join(dir, "issues.json")
	runsPath := filepath.Join(dir, "runs.json")
	write := func(path, data string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	issues := `[{"number":3,"state":"open","created_at":"2024-01-01T00:00:00Z","body":"## Done when\n\n- tests pass\n\n## End-to-end check\n\nharness next prints number=3\n","labels":[{"name":"codeaf-queued"},{"name":"area:app/x"}]},
	            {"number":9,"state":"open","created_at":"2024-02-01T00:00:00Z","body":"Depends on #3","labels":[{"name":"codeaf"},{"name":"area:app/y"}]}]`
	write(issuesPath, issues)
	write(runsPath, `[{"status":"completed"}]`)

	// A queued issue is picked, and the stdout carries exactly the
	// GITHUB_OUTPUT lines: the kind, the number, then the lane (issue #137).
	t.Setenv("CODEAF_WEEK_SPENT_USD", "1")
	if out, err := runNextStdout(t, issuesPath, runsPath); err != nil || out != "kind=issue\nnumber=3\nlane=0\n" {
		t.Fatalf("runNext printed %q, %v", out, err)
	}

	// Nothing to run: empty stdout, no error.
	write(issuesPath, `[{"number":3,"state":"open","created_at":"2024-01-01T00:00:00Z","labels":[{"name":"codeaf"}]}]`)
	if out, err := runNextStdout(t, issuesPath, runsPath); err != nil || out != "" {
		t.Fatalf("runNext printed %q, %v; wanted nothing", out, err)
	}

	// One run in progress on another area: the issue starts on the free
	// lane.
	write(issuesPath, issues)
	write(runsPath, `[{"status":"in_progress","databaseId":100,"displayTitle":"CodeAF #9 (lane 0)"}]`)
	if out, err := runNextStdout(t, issuesPath, runsPath); err != nil || out != "kind=issue\nnumber=3\nlane=1\n" {
		t.Fatalf("runNext printed %q, %v", out, err)
	}

	// Two runs in progress, the queue's max_parallel_runs: empty stdout,
	// no error.
	write(runsPath, `[{"status":"in_progress","databaseId":100,"displayTitle":"CodeAF #9 (lane 0)"},
	                  {"status":"queued","databaseId":101,"displayTitle":"CodeAF #11 (lane 1)"}]`)
	if out, err := runNextStdout(t, issuesPath, runsPath); err != nil || out != "" {
		t.Fatalf("runNext printed %q, %v; wanted nothing", out, err)
	}

	// The threshold is taken against the config's weekly credit.
	t.Setenv("CODEAF_WEEK_SPENT_USD", fmt.Sprintf("%v", cfg.Limits.WeeklyCreditUSD*0.75))
	if out, err := runNextStdout(t, issuesPath, runsPath); err != nil || out != "" {
		t.Fatalf("runNext at the threshold printed %q, %v; wanted nothing", out, err)
	}

	// Bad and missing spend totals fail the command.
	for _, spent := range []string{"-3", "tomorrow", ""} {
		t.Setenv("CODEAF_WEEK_SPENT_USD", spent)
		if _, err := runNextStdout(t, issuesPath, runsPath); err == nil {
			t.Fatalf("runNext accepted CODEAF_WEEK_SPENT_USD=%q", spent)
		}
	}

	// Malformed input fails the command.
	t.Setenv("CODEAF_WEEK_SPENT_USD", "1")
	write(issuesPath, `{}`)
	if _, err := runNextStdout(t, issuesPath, runsPath); err == nil {
		t.Fatal("runNext accepted malformed issues JSON")
	}
	if _, err := runNextStdout(t, filepath.Join(dir, "missing.json"), runsPath); err == nil {
		t.Fatal("runNext accepted a missing file")
	}
}

// TestNextAreas covers area conflicts: an area comes from a label
// (area:harness, area:app/<name>) or from paths in the body, an issue with
// no area conflicts with everything, and so does a run whose issue cannot be
// worked out (title does not parse, issue not in the open list).
func TestNextAreas(t *testing.T) {
	o := nextOpts()
	o.MaxParallelRuns = 2
	issue5 := areaIssue(5, "2024-01-01T00:00:00Z", "harness") // the in-flight run's issue
	held := runStatus{Status: "in_progress", DatabaseId: 100, DisplayTitle: "CodeAF #5 (lane 0)"}
	bodyHeld := withBody(queuedIssue(3, "2024-01-01T00:00:00Z", "codeaf-queued"), "touch .github/workflows/ci.yml")
	bodyFree := withBody(queuedIssue(3, "2024-01-01T00:00:00Z", "codeaf-queued"), "paths app/x/main.go\n"+readyBody)

	cases := []struct {
		name   string
		issues []item
		runs   []runStatus
		want   int
	}{
		{"same label area holds", []item{issue5, areaIssue(3, "2024-01-01T00:00:00Z", "harness")}, []runStatus{held}, 0},
		{"different label area starts", []item{issue5, areaIssue(3, "2024-01-01T00:00:00Z", "app/x")}, []runStatus{held}, 3},
		{"body path on the busy area holds", []item{issue5, bodyHeld}, []runStatus{held}, 0},
		{"body path elsewhere starts", []item{issue5, bodyFree}, []runStatus{held}, 3},
		{"no area conflicts with a run", []item{issue5, queuedIssue(3, "2024-01-01T00:00:00Z", "codeaf-queued")}, []runStatus{held}, 0},
		{"no area is held even with nothing running", []item{queuedIssue(3, "2024-01-01T00:00:00Z", "codeaf-queued")}, nil, 0},
		{"an unknown in-flight issue conflicts", []item{issue5, areaIssue(3, "2024-01-01T00:00:00Z", "app/x")},
			[]runStatus{{Status: "in_progress", DatabaseId: 7, DisplayTitle: "CodeAF #99 (lane 0)"}}, 0},
		{"a garbage title conflicts", []item{issue5, areaIssue(3, "2024-01-01T00:00:00Z", "app/x")},
			[]runStatus{{Status: "in_progress", DatabaseId: 7, DisplayTitle: "queued: something"}}, 0},
		{"a queued run holds its area too", []item{issue5, areaIssue(3, "2024-01-01T00:00:00Z", "harness")},
			[]runStatus{{Status: "queued", DatabaseId: 100, DisplayTitle: "CodeAF #5 (lane 0)"}}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if p := Next(c.issues, c.runs, o); p.Number != c.want {
				t.Fatalf("Next = %+v; want %d", p, c.want)
			}
		})
	}
}

// TestNextLanes covers lane assignment: the new run gets the lowest lane in
// [0, max_parallel_runs) no in-flight run occupies, a lane parsed out of
// range still occupies its value but in-range lanes are assigned regardless,
// and two runs never share a lane.
func TestNextLanes(t *testing.T) {
	o := nextOpts()
	o.MaxParallelRuns = 3
	cand := areaIssue(3, "2024-01-01T00:00:00Z", "app/z")
	i4 := areaIssue(4, "2024-01-01T00:00:00Z", "app/x")
	i6 := areaIssue(6, "2024-01-01T00:00:00Z", "app/y")
	issues := []item{cand, i4, i6}
	// A title without the lane suffix parses to lane 0.
	noSuffix := func(n, dbid int) runStatus {
		return runStatus{Status: "in_progress", DatabaseId: dbid, DisplayTitle: fmt.Sprintf("CodeAF #%d", n)}
	}

	cases := []struct {
		name string
		runs []runStatus
		lane int
	}{
		{"nothing in flight", nil, 0},
		{"lane 0 taken", []runStatus{running(4, 0, 100)}, 1},
		{"lane 1 taken", []runStatus{running(4, 1, 100)}, 0},
		{"lanes 0 and 1 taken", []runStatus{running(4, 0, 100), running(6, 1, 101)}, 2},
		{"an out-of-range lane does not block in-range lanes", []runStatus{running(4, 9, 100)}, 0},
		{"two runs on lane 0 never share it with a third", []runStatus{noSuffix(4, 100), noSuffix(6, 101)}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := Next(issues, c.runs, o)
			if p.Number != 3 || p.Lane != c.lane {
				t.Fatalf("Next = %+v; want 3, lane %d", p, c.lane)
			}
		})
	}
}

// TestRunLane pins the strict parse of an in-flight run's display title,
// which is untrusted data: only "CodeAF #<n>" and "CodeAF #<n> (lane <n>)"
// match, and anything else — wrong prefix, letters in the lane, trailing
// prose, the empty title — is lane 0. A lane that parses out of range still
// comes back as its value; only Next's assignment is limited to the range.
func TestRunLane(t *testing.T) {
	cases := []struct {
		title string
		want  int
	}{
		{"CodeAF #3", 0},
		{"CodeAF #3 (lane 0)", 0},
		{"CodeAF #3 (lane 1)", 1},
		{"CodeAF #3 (lane 12)", 12},
		{"CodeAF #3 (lane 99)", 99}, // out of range, still its value
		{"CodeAF #3 (lane abc)", 0}, // lane letters
		{"CodeAF #3 (lane )", 0},
		{"CodeAF #3 (lane -1)", 0},
		{"harness next #3", 0}, // wrong prefix
		{"CodeAF 3", 0},
		{"codeaf #3 (lane 1)", 0}, // case
		{"CodeAF #3 (lane 1) extra", 0},
		{"", 0}, // empty title
	}
	for _, c := range cases {
		if got := runLane(runStatus{DisplayTitle: c.title}); got != c.want {
			t.Errorf("runLane(%q) = %d; want %d", c.title, got, c.want)
		}
	}
	// The issue number parses out of the same title.
	if issue, lane := runTitleFields("CodeAF #3 (lane 12)"); issue != 3 || lane != 12 {
		t.Fatalf("runTitleFields = %d, %d; want 3, 12", issue, lane)
	}
	if issue, _ := runTitleFields("CodeAF #3"); issue != 3 {
		t.Fatalf("runTitleFields issue = %d; want 3", issue)
	}
}

// TestIssueAreas covers the area sources: labels win, body paths add to
// them, the set is sorted, and an issue that names nothing has none.
func TestIssueAreas(t *testing.T) {
	it := queuedIssue(3, "2024-01-01T00:00:00Z", "area:app/y")
	it.Body = "see .github/workflows/ci.yml, then app/z/main.go, and app/x."
	got := issueAreas(it)
	want := []string{"app/x", "app/y", "app/z", "harness"}
	if !slices.Equal(got, want) {
		t.Fatalf("issueAreas = %v; want %v", got, want)
	}
	if areas := issueAreas(queuedIssue(4, "2024-01-01T00:00:00Z")); len(areas) != 0 {
		t.Fatalf("issueAreas = %v; want none", areas)
	}
	// Both sources naming one area, and paths that name nothing.
	it = queuedIssue(5, "2024-01-01T00:00:00Z", "area:harness")
	it.Body = "app/ alone, wrap/z inside a word, but the area is app/x"
	if got := issueAreas(it); !slices.Equal(got, []string{"app/x", "harness"}) {
		t.Fatalf("issueAreas = %v; want [app/x harness]", got)
	}
}

// TestNextLogs pins what the queue shows: one line for the runs in progress
// with their lanes, a reason line per held candidate, and the started line.
func TestNextLogs(t *testing.T) {
	// The in-flight issue carries the active label, so it is not a
	// candidate; the others queue.
	inFlightIssue := func(area string) item {
		return queuedIssue(5, "2024-01-01T00:00:00Z", "codeaf-queued", "codeaf", "area:"+area)
	}
	cases := []struct {
		name   string
		issues []item
		runs   []runStatus
		want   []string
		start  int // the issue started, or 0
		lane   int
	}{
		{
			name: "a start",
			issues: []item{
				areaIssue(3, "2024-01-01T00:00:00Z", "app/y"), // same area as the run: held
				areaIssue(4, "2024-01-01T00:00:00Z", "app/z"),
				inFlightIssue("app/y"),
			},
			runs: []runStatus{running(5, 0, 100)},
			want: []string{
				"runs in progress: 1/2, lanes in use: 0 (run 100)",
				"holding #3: area app/y busy (run 100)",
				"starting #4 (lane 1): kind=issue, next queued issue",
			},
			start: 4, lane: 1,
		},
		{
			name: "capacity holds everything",
			issues: []item{
				areaIssue(3, "2024-01-01T00:00:00Z", "app/x"),
				inFlightIssue("app/y"),
				queuedIssue(6, "2024-01-01T00:00:00Z", "codeaf-queued", "codeaf", "area:app/z"),
			},
			runs: []runStatus{running(5, 0, 100), running(6, 1, 101)},
			want: []string{
				"runs in progress: 2/2, lanes in use: 0 (run 100), 1 (run 101)",
				"holding #3: lane capacity full",
				"lane capacity full",
			},
		},
		{
			name: "an issue with no area",
			issues: []item{
				queuedIssue(3, "2024-01-01T00:00:00Z", "codeaf-queued"),
				inFlightIssue("app/y"),
			},
			runs: []runStatus{running(5, 0, 100)},
			want: []string{
				"runs in progress: 1/2, lanes in use: 0 (run 100)",
				"holding #3: no area label",
				"no area label",
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var lines []string
			o := nextOpts()
			o.MaxParallelRuns = 2
			o.Logf = func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
			p := Next(c.issues, c.runs, o)
			if p.Number != c.start || p.Lane != c.lane {
				t.Fatalf("Next = %+v; want %d, lane %d", p, c.start, c.lane)
			}
			if !slices.Equal(lines, c.want) {
				t.Fatalf("log lines:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(c.want, "\n"))
			}
			// Without a Logf, Next still decides (the nil log is discarded).
			o.Logf = nil
			if p2 := Next(c.issues, c.runs, o); p2.Number != c.start || p2.Lane != c.lane {
				t.Fatalf("silent Next = %+v; want %d, lane %d", p2, c.start, c.lane)
			}
		})
	}
}

// TestPlanRunTitleParsing covers the plan display title (issue #61): "CodeAF
// plan" and "CodeAF plan (lane N)" are plan runs on lane 0 and N, and no
// other shape is one. A plan title is never matched by runTitleRe — it
// parses to issue 0 — and an issue title is never matched by runPlanRe; the
// two shapes are disjoint.
func TestPlanRunTitleParsing(t *testing.T) {
	cases := []struct {
		title string
		lane  int
		plan  bool
	}{
		{"CodeAF plan", 0, true},
		{"CodeAF plan (lane 0)", 0, true},
		{"CodeAF plan (lane 1)", 1, true},
		{"CodeAF plan (lane 12)", 12, true},
		// Not plan titles: trailing prose, letters in the lane, an issue
		// title, anything else.
		{"CodeAF plan run", 0, false},
		{"CodeAF plan (lane x)", 0, false},
		{"CodeAF plan (lane 1) extra", 0, false},
		{"codeaf plan", 0, false},
		{"CodeAF #3", 0, false},
		{"CodeAF #3 (lane 1)", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		lane, ok := runPlanFields(c.title)
		if lane != c.lane || ok != c.plan {
			t.Errorf("runPlanFields(%q) = (%d, %v); want (%d, %v)", c.title, lane, ok, c.lane, c.plan)
		}
		if got := isPlanRun(runStatus{DisplayTitle: c.title}); got != c.plan {
			t.Errorf("isPlanRun(%q) = %v; want %v", c.title, got, c.plan)
		}
		// runLane reads the plan lane too, and only the plan lane.
		if got := runLane(runStatus{DisplayTitle: c.title}); got != c.lane && c.plan {
			t.Errorf("runLane(%q) = %d; want %d", c.title, got, c.lane)
		}
		// The issue-title regex still refuses plan titles: issue 0, lane 0.
		issue, issueLane := runTitleFields(c.title)
		if c.plan && (issue != 0 || issueLane != 0) {
			t.Errorf("runTitleFields(%q) = (%d, %d); a plan run must parse to issue 0", c.title, issue, issueLane)
		}
	}
	// An issue title still parses as before, plan parsing or no.
	if issue, lane := runTitleFields("CodeAF #3 (lane 1)"); issue != 3 || lane != 1 {
		t.Fatalf("runTitleFields issue title = (%d, %d); want (3, 1)", issue, lane)
	}
	if lane := runLane(running(3, 1, 100)); lane != 1 {
		t.Fatalf("runLane issue title = %d; want 1", lane)
	}
}

// TestNextPlanRunNoAreaConflict covers the queue side of plan runs: a plan
// run in flight occupies its lane but holds no areas, so it never blocks an
// issue run by area — and an in-flight issue run on the same area blocks a
// second issue run just the same, plan runs or no.
func TestNextPlanRunNoAreaConflict(t *testing.T) {
	issues := []item{
		areaIssue(3, "2024-01-01T00:00:00Z", "app/x"),
		areaIssue(5, "2024-01-01T00:00:00Z", "app/x"),
	}
	o := nextOpts()
	o.MaxParallelRuns = 2
	// A plan run on lane 0 and the same-area issue still starts, on lane 1.
	planRun := runStatus{Status: "in_progress", DatabaseId: 100, DisplayTitle: "CodeAF plan"}
	if p := Next(issues, []runStatus{planRun}, o); p.Number != 3 || p.Lane != 1 {
		t.Fatalf("plan run in flight: Next = %+v; want 3, lane 1", p)
	}
	// An issue run on the same area blocks the same-area candidate, as
	// always: run 100 works app/x, so #3 is held and #5 (its own area
	// aside) is not picked either — the same area on both.
	sameArea := []runStatus{running(3, 0, 100)}
	p, lines := nextPick([]item{areaIssue(5, "2024-01-01T00:00:00Z", "app/x")}, sameArea, o)
	if p.Kind != "" || !strings.Contains(strings.Join(lines, "\n"), "busy") {
		t.Fatalf("same-area issue run: Next = %+v, log %v; want held", p, lines)
	}
}

// TestQueueDispatchesPassTheGate feeds the exact `-f` inputs of every
// `gh workflow run codeaf.yml` step in codeaf-queue.yml through Decide
// (issue #61): a queue step that dispatches a combination the gate skips
// would start runs that die in the gate with nothing labelled and nothing
// published, so this test fails first, on the default branch's queue.
// Shell variables in a flag's value stand for the digits the queue sends,
// so `-f lane=$LANE` is decided as a valid lane.
func TestQueueDispatchesPassTheGate(t *testing.T) {
	t.Setenv("GITHUB_REPOSITORY", "RichardoC/factory-repo")
	t.Setenv("GITHUB_RUN_ID", "42")
	// The queue labels the issue before dispatching it, so the lookup
	// sees what the gate will see at run time: an open, labelled issue.
	labelled := item{Number: 3, State: "open", Labels: []label{{Name: "codeaf"}}}
	lookup := func(int) (item, error) { return labelled, nil }
	// Reviews and continuations (issue #137) dispatch a PR number: the gate
	// looks it up as a CodeAF PR on a branch of this repository.
	pr := item{Number: 3, State: "open", Labels: []label{{Name: "codeaf"}}}
	pr.Head.Ref = "codeaf/issue-3"
	pr.Head.Repo = &struct {
		FullName string `json:"full_name"`
	}{FullName: "RichardoC/factory-repo"}
	pr.Base.Ref = "main"
	lookupPR := func(int) (item, error) { return pr, nil }
	data, err := os.ReadFile("../workflows/codeaf-queue.yml")
	if err != nil {
		t.Fatal(err)
	}
	// One block per step; a step's block runs to the next "- name:" line.
	steps := strings.Split(string(data), "\n      - name: ")
	var flags []map[string]string
	for _, step := range steps {
		for _, line := range strings.Split(step, "\n") {
			if !strings.Contains(line, "gh workflow run codeaf.yml") {
				continue
			}
			inputs := map[string]string{}
			for _, m := range regexp.MustCompile(`-f\s+"?([a-z_]+)=([^"\s]+)"?`).FindAllStringSubmatch(line, -1) {
				value := m[2]
				if strings.Contains(value, "$") {
					value = "3" // a lane or number the queue resolved, digits only
				}
				inputs[m[1]] = value
			}
			if len(inputs) == 0 {
				t.Fatalf("queue dispatch line has no -f inputs to decide: %s", line)
			}
			flags = append(flags, inputs)
		}
	}
	// Issue #137: the queue starts reviews and continuations too, so it
	// dispatches four kinds: review, continuation (pr), issue and planner.
	if len(flags) != 4 {
		t.Fatalf("codeaf-queue.yml dispatches codeaf.yml %d times; want 4 (review, continuation, issue and planner)", len(flags))
	}
	for _, inputs := range flags {
		ev := event{Inputs: inputs}
		ev.Repository.DefaultBranch = "main"
		got, err := Decide("workflow_dispatch", ev, "codeaf", lookupPR, lookup)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Run {
			t.Fatalf("the queue dispatches %v and the gate skips it: %s", inputs, got.Reason)
		}
	}
	// The planner's dispatch must be the one that reaches plan mode.
	last := flags[len(flags)-1]
	if last["plan"] != "true" {
		t.Fatalf("the queue's last codeaf.yml dispatch is %v; want the planner's plan=true", last)
	}
}

// TestRunAreasOfPullRequest covers the PR side of the area rule (issue #132):
// an in-flight review run titled `CodeAF #124` takes its areas from the issue
// its PR closes (#119, area:harness) united with the PR's changed files (here
// under .github/, the same harness area). An app candidate starts on the free
// lane; a harness candidate is held with the usual busy reason.
func TestRunAreasOfPullRequest(t *testing.T) {
	o := nextOpts()
	o.MaxParallelRuns = 2
	o.PRFiles = map[int][]string{124: {".github/codeaf/planreport.go"}}
	pr := queuedIssue(124, "2024-01-01T00:00:00Z", "codeaf")
	pr.Body = "Closes #119"
	pr.PullRequest = json.RawMessage(`{"number":124}`)
	linked := areaIssue(119, "2024-01-01T00:00:00Z", "harness")
	review := runStatus{Status: "in_progress", DatabaseId: 39072194971, DisplayTitle: "CodeAF #124"}
	byNumber := map[int]item{124: pr, 119: linked}

	if got := runAreas(review, byNumber, o.PRFiles); !slices.Equal(got, []string{"harness"}) {
		t.Fatalf("runAreas = %v; want [harness]", got)
	}
	issues := []item{pr, linked, areaIssue(3, "2024-01-01T00:00:00Z", "app/kube-sqlite3-vfs")}
	if p := Next(issues, []runStatus{review}, o); p.Kind != kindIssue || p.Number != 3 || p.Lane != 1 {
		t.Fatalf("app candidate vs PR review run: Next = %+v; want 3, lane 1", p)
	}
	held := []item{pr, linked, areaIssue(4, "2024-01-01T00:00:00Z", "harness")}
	p, lines := nextPick(held, []runStatus{review}, o)
	if p.Kind != "" || !slices.Contains(lines, "area harness busy (run 39072194971)") {
		t.Fatalf("harness candidate vs PR review run: Next = %+v, log %v; want held", p, lines)
	}
}

// TestRunAreasFromChangedFilesOnly covers a PR with no `Closes` line and no
// area labels: its areas come from its changed files alone.
func TestRunAreasFromChangedFilesOnly(t *testing.T) {
	o := nextOpts()
	o.MaxParallelRuns = 2
	o.PRFiles = map[int][]string{10: {"app/x/main.go", "app/x/util/a.go"}}
	pr := queuedIssue(10, "2024-01-01T00:00:00Z", "codeaf")
	pr.PullRequest = json.RawMessage(`{"number":10}`)
	review := runStatus{Status: "in_progress", DatabaseId: 5, DisplayTitle: "CodeAF #10"}

	if got := runAreas(review, map[int]item{10: pr}, o.PRFiles); !slices.Equal(got, []string{"app/x"}) {
		t.Fatalf("runAreas = %v; want [app/x]", got)
	}
	// A candidate on the same area is held; another area starts.
	if p := Next([]item{pr, areaIssue(3, "2024-01-01T00:00:00Z", "app/x")}, []runStatus{review}, o); p.Kind != "" {
		t.Fatalf("same-area candidate: Next = %+v; want held", p)
	}
	if p := Next([]item{pr, areaIssue(3, "2024-01-01T00:00:00Z", "app/y")}, []runStatus{review}, o); p.Number != 3 {
		t.Fatalf("other-area candidate: Next = %+v; want 3", p)
	}
}

// TestNoAreaIssueIsHeldNotBlocking covers the candidate side of issue #132:
// an issue with no area label and no path in its body is held with reason
// "no area label", never started, and the next candidate with an area still
// starts.
func TestNoAreaIssueIsHeldNotBlocking(t *testing.T) {
	o := nextOpts()
	var lines []string
	o.Logf = func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	if p := Next([]item{queuedIssue(3, "2024-01-01T00:00:00Z", "codeaf-queued")}, nil, o); p.Kind != "" || !slices.Contains(lines, "no area label") {
		t.Fatalf("area-less candidate alone: Next = %+v, log %v; want 0, no area label", p, lines)
	}
	issues := []item{queuedIssue(3, "2024-01-01T00:00:00Z", "codeaf-queued"), areaIssue(5, "2024-01-01T00:00:00Z", "app/x")}
	p := Next(issues, nil, o)
	if p.Number != 5 || p.Lane != 0 {
		t.Fatalf("area-less candidate first: Next = %+v; want 5, lane 0", p)
	}
	if !slices.Contains(lines, "holding #3: no area label") {
		t.Fatalf("log lines = %v; want the no-area hold", lines)
	}
}

// TestPRRunWithUnknownAreasStillBlocks pins the unchanged fail-closed rule: a
// PR run with no `Closes` line, no area labels and no entry in prFiles has an
// empty area set, which conflicts with every candidate.
func TestPRRunWithUnknownAreasStillBlocks(t *testing.T) {
	o := nextOpts()
	o.MaxParallelRuns = 2
	pr := queuedIssue(10, "2024-01-01T00:00:00Z", "codeaf")
	pr.PullRequest = json.RawMessage(`{"number":10}`)
	review := runStatus{Status: "in_progress", DatabaseId: 100, DisplayTitle: "CodeAF #10"}
	if got := runAreas(review, map[int]item{10: pr}, nil); len(got) != 0 {
		t.Fatalf("runAreas = %v; want none", got)
	}
	p, lines := nextPick([]item{pr, areaIssue(3, "2024-01-01T00:00:00Z", "app/x")}, []runStatus{review}, o)
	if p.Kind != "" || !slices.Contains(lines, "area app/x busy (run 100)") {
		t.Fatalf("Next = %+v, log %v; want held on the unknown PR run", p, lines)
	}
}

// TestHoldCommentIsPostedOnce covers the hold comment (issue #132) against a
// fake API: the first call posts the marker comment, the second — with the
// comment present — posts nothing.
func TestHoldCommentIsPostedOnce(t *testing.T) {
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7/comments":
			body := "[]"
			if posts > 0 {
				body = `[{"body":"an earlier note <!-- codeaf-queue-hold -->"}]`
			}
			fmt.Fprint(w, body)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/7/comments":
			posts++
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	g := github{api: server.URL, repo: "o/r", token: "t", client: &http.Client{Timeout: 5 * time.Second}}

	body := holdCommentBody(7)
	if !strings.Contains(body, queueHoldMarker) || !strings.Contains(body, "area:harness") || strings.Contains(body, "untrusted") {
		t.Fatalf("holdCommentBody = %q", body)
	}
	if err := ensureHoldComment(g, 7); err != nil {
		t.Fatal(err)
	}
	if posts != 1 {
		t.Fatalf("first call posted %d comments; want 1", posts)
	}
	if err := ensureHoldComment(g, 7); err != nil {
		t.Fatal(err)
	}
	if posts != 1 {
		t.Fatalf("second call posted again (%d posts); want the marker to stop it", posts)
	}
}

// TestNextRepicksStrandedPR covers the stranded re-pick (issue #215): an
// open codeaf PR with no queue label and no run in progress is picked as a
// review; the marker comment bounds the re-pick to once per head commit; a
// review-passed PR, or one with a run in progress, is never re-picked; and
// an unknown or non-hex head means no re-pick (the guard fails closed).
func TestNextRepicksStrandedPR(t *testing.T) {
	shaA := strings.Repeat("a", 40)
	shaB := strings.Repeat("b", 40)
	pr := queuedIssue(204, "2024-01-01T00:00:00Z", "codeaf")
	pr.PullRequest = json.RawMessage(`{"number":204}`)

	o := nextOpts()
	o.PRHeads = map[int]string{204: shaA}
	o.PRMarkers = map[int][]string{204: nil}
	if p, lines := nextPick([]item{pr}, nil, o); p.Kind != kindReview || p.Number != 204 || p.Lane != 0 {
		t.Fatalf("Next = %+v; want review #204 on lane 0", p)
	} else if !slices.Contains(lines, "re-picking stranded PR #204 at "+shaA) {
		t.Fatalf("log %v; want the re-pick line at %s", lines, shaA)
	}

	// The marker names the head the re-pick was for: the PR is skipped
	// while its head still matches, and picked again once the head moves.
	o.PRMarkers[204] = []string{shaA}
	if p, lines := nextPick([]item{pr}, nil, o); p.Kind != "" || !strings.Contains(strings.Join(lines, "\n"), "stranded re-pick already recorded") {
		t.Fatalf("Next with the marker = %+v, log %v; want held", p, lines)
	}
	o.PRHeads[204] = shaB
	if p, _ := nextPick([]item{pr}, nil, o); p.Kind != kindReview || p.Number != 204 {
		t.Fatalf("Next after the head moved = %+v; want review #204 again", p)
	}

	// A PR labelled codeaf-review-passed is never re-picked.
	passed := queuedIssue(205, "2024-01-01T00:00:00Z", "codeaf", "codeaf-review-passed")
	passed.PullRequest = json.RawMessage(`{"number":205}`)
	o2 := nextOpts()
	o2.PRHeads = map[int]string{205: shaA}
	o2.PRMarkers = map[int][]string{205: nil}
	if p, _ := nextPick([]item{passed}, nil, o2); p.Kind != "" {
		t.Fatalf("Next on a review-passed PR = %+v; want nothing", p)
	}

	// A PR with a run in progress is never re-picked (issue #137's guard).
	o3 := nextOpts()
	o3.PRHeads = map[int]string{204: shaA}
	o3.PRMarkers = map[int][]string{204: nil}
	if p, _ := nextPick([]item{pr}, []runStatus{reviewRun(204, 0, 100)}, o3); p.Kind != "" {
		t.Fatalf("Next with the review in flight = %+v; want nothing", p)
	}

	// An unknown or non-hex head means no re-pick: the re-pick could not be
	// recorded, so it could repeat on every tick.
	o4 := nextOpts()
	o4.PRHeads = map[int]string{}
	o4.PRMarkers = map[int][]string{}
	if p, _ := nextPick([]item{pr}, nil, o4); p.Kind != "" {
		t.Fatalf("Next with no head SHA = %+v; want nothing", p)
	}
	o4.PRHeads[204] = "not-a-sha"
	if p, _ := nextPick([]item{pr}, nil, o4); p.Kind != "" {
		t.Fatalf("Next with a non-hex head = %+v; want nothing", p)
	}
}

// TestRepickMarkers covers the marker parse (issue #215): only a full
// 40-hex SHA after the marker is kept, everything else — a truncated SHA,
// prose, a comment without the marker — is ignored as if absent.
func TestRepickMarkers(t *testing.T) {
	sha := strings.Repeat("c", 40)
	got := repickMarkers(bodyComments{
		{"a person's note"},
		{"<!-- codeaf-queue-repick " + sha[:39] + " -->"},
		{"<!-- codeaf-queue-repick " + sha + " -->"},
		{"quote: the queue posts <!-- codeaf-queue-repick zzz --> on stranded PRs"},
	})
	if len(got) != 1 || got[0] != sha {
		t.Fatalf("repickMarkers = %v; want [%s]", got, sha)
	}
	if !fullSHARx.MatchString(sha) || fullSHARx.MatchString(sha[:39]) || fullSHARx.MatchString("zzz") {
		t.Fatal("fullSHARx accepted a SHA it should not")
	}
}

// TestRunNextRepicksStrandedPR is the end-to-end check of issue #215, on the
// fake GitHub server: an open PR labelled only codeaf, with no in-progress
// runs and no comments, is picked (kind=review) and gets one marker comment
// naming its head SHA; with that comment served the PR is not picked again;
// a moved head is picked again, with a second marker for the new commit.
func TestRunNextRepicksStrandedPR(t *testing.T) {
	shaA := strings.Repeat("a", 40)
	shaB := strings.Repeat("b", 40)
	sha := shaA
	var comments []string
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/RichardoC/factory-repo/issues" && r.URL.Query().Get("state") == "open":
			fmt.Fprint(w, `[{"number":204,"state":"open","created_at":"2024-01-01T00:00:00Z","pull_request":{"number":204},"labels":[{"name":"codeaf"}]}]`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/RichardoC/factory-repo/pulls/204":
			fmt.Fprintf(w, `{"number":204,"head":{"ref":"branch","sha":%q}}`, sha)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/RichardoC/factory-repo/issues/204/comments":
			if len(comments) == 0 {
				fmt.Fprint(w, `[]`)
				return
			}
			var items []string
			for _, c := range comments {
				items = append(items, fmt.Sprintf(`{"body":%q}`, c))
			}
			fmt.Fprint(w, "["+strings.Join(items, ",")+"]")
		case r.Method == http.MethodPost && r.URL.Path == "/repos/RichardoC/factory-repo/issues/204/comments":
			posts++
			var body struct {
				Body string `json:"body"`
			}
			data, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(data, &body)
			comments = append(comments, body.Body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/RichardoC/factory-repo/contents/app":
			fmt.Fprint(w, `[]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	t.Setenv("GITHUB_API_URL", server.URL)
	t.Setenv("GITHUB_REPOSITORY", "RichardoC/factory-repo")
	t.Setenv("GH_TOKEN", "tok")
	t.Setenv("PROXY_MODELS", "models.json")
	t.Setenv("CODEAF_WEEK_SPENT_USD", "1")
	if _, err := loadConfig("models.json"); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	issuesPath := filepath.Join(dir, "issues.json")
	runsPath := filepath.Join(dir, "runs.json")
	write := func(path, data string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The issues list the queue job hands `harness next` on the command
	// line: the PR, labelled only codeaf. The fake server serves the calls
	// runNext itself makes: the PR's head, its comments, the app listing.
	write(issuesPath, `[{"number":204,"state":"open","created_at":"2024-01-01T00:00:00Z","pull_request":{"number":204},"labels":[{"name":"codeaf"}]}]`)
	write(runsPath, `[{"status":"completed"}]`)

	// First tick: the stranded PR is picked, one marker comment names the
	// head SHA.
	if out, err := runNextStdout(t, issuesPath, runsPath); err != nil || out != "kind=review\nnumber=204\nlane=0\n" {
		t.Fatalf("runNext printed %q, %v", out, err)
	}
	if posts != 1 || len(comments) != 1 || !strings.Contains(comments[0], shaA) || !strings.Contains(comments[0], queueRepickMarker) {
		t.Fatalf("marker comments = %q (posts %d); want one naming %s", comments, posts, shaA)
	}

	// Second tick, the comment served: nothing is printed for the PR.
	if out, err := runNextStdout(t, issuesPath, runsPath); err != nil || out != "" {
		t.Fatalf("runNext with the marker served printed %q, %v; wanted nothing", out, err)
	}
	if posts != 1 {
		t.Fatalf("runNext posted again (%d posts); want the marker to stop it", posts)
	}

	// The head moves: the PR is picked again, with a second marker naming
	// the new commit.
	sha = shaB
	if out, err := runNextStdout(t, issuesPath, runsPath); err != nil || out != "kind=review\nnumber=204\nlane=0\n" {
		t.Fatalf("runNext after the head moved printed %q, %v", out, err)
	}
	if posts != 2 || len(comments) != 2 || !strings.Contains(comments[1], shaB) {
		t.Fatalf("marker comments after the move = %q (posts %d); want one naming %s", comments, posts, shaB)
	}
} // TestNextPicksReviewBeforeIssues covers the queue's new head of the pass
// (issue #137): a pull request labelled codeaf and codeaf-review-needed is
// picked with kind review on the lowest free lane, ahead of any queued
// issue — and, once the review is in flight as a mirrored run, it does not
// block an area:harness issue picked in the same pass: a review holds no
// area, and the pull request is not picked again while a run already has it
// as its subject.
func TestNextPicksReviewBeforeIssues(t *testing.T) {
	o := nextOpts()
	o.MaxParallelRuns = 2
	pr := queuedIssue(124, "2024-01-01T00:00:00Z", "codeaf", "codeaf-review-needed", "area:harness")
	pr.PullRequest = json.RawMessage(`{"number":124}`)
	issue := areaIssue(3, "2024-01-01T00:00:00Z", "harness")
	issues := []item{pr, issue}

	if p := Next(issues, nil, o); p.Kind != kindReview || p.Number != 124 || p.Lane != 0 {
		t.Fatalf("Next = %+v; want review #124 on lane 0", p)
	}
	// The review is now in flight as a mirrored run: the harness issue
	// starts too, on the one free lane, and no second review is picked.
	mirrored := []runStatus{reviewRun(124, 0, 100)}
	if p := Next(issues, mirrored, o); p.Kind != kindIssue || p.Number != 3 || p.Lane != 1 {
		t.Fatalf("Next after the review run = %+v; want issue 3 on lane 1", p)
	}
	// At the lane capacity the review is held like any other run.
	o.MaxParallelRuns = 1
	if p, lines := nextPick(issues, mirrored, o); p.Kind != "" || !slices.Contains(lines, "lane capacity full") {
		t.Fatalf("Next at capacity = %+v, log %v; want held", p, lines)
	}
}

// TestNextReviewHoldsNoArea covers the review side of areaBusy (issue #137):
// an in-flight `CodeAF review #5 (lane 1)` run never conflicts with any
// candidate, although the pull request it reviews closes a harness issue and
// touches harness files — a `CodeAF #5` run in its place would hold the
// harness area and block the harness candidate.
func TestNextReviewHoldsNoArea(t *testing.T) {
	o := nextOpts()
	o.MaxParallelRuns = 2
	o.PRFiles = map[int][]string{5: {".github/codeaf/queue.go"}}
	pr := queuedIssue(5, "2024-01-01T00:00:00Z", "codeaf")
	pr.Body = "Closes #119"
	pr.PullRequest = json.RawMessage(`{"number":5}`)
	linked := areaIssue(119, "2024-01-01T00:00:00Z", "harness")
	byNumber := map[int]item{5: pr, 119: linked}
	review := reviewRun(5, 1, 100)

	// areaBusy itself: the review run is skipped exactly like a plan run.
	if busy, r, conflict := areaBusy([]string{"harness"}, []runStatus{review}, byNumber, o.PRFiles); conflict {
		t.Fatalf("areaBusy held on a review run: %q (run %d)", busy, r.DatabaseId)
	}
	// A harness candidate starts although the review's PR is all harness.
	harness := []item{pr, linked, areaIssue(3, "2024-01-01T00:00:00Z", "harness")}
	if p := Next(harness, []runStatus{review}, o); p.Kind != kindIssue || p.Number != 3 || p.Lane != 0 {
		t.Fatalf("harness candidate vs review run: Next = %+v; want 3 on lane 0", p)
	}
	// An app candidate starts the same way.
	app := []item{pr, linked, areaIssue(4, "2024-01-01T00:00:00Z", "app/x")}
	if p := Next(app, []runStatus{review}, o); p.Kind != kindIssue || p.Number != 4 || p.Lane != 0 {
		t.Fatalf("app candidate vs review run: Next = %+v; want 4 on lane 0", p)
	}
	// The same PR run as a `CodeAF #5` continuation does hold the area:
	// the harness candidate is blocked, the review shape is what makes the
	// difference.
	continueRun := running(5, 1, 100)
	if p, lines := nextPick(harness, []runStatus{continueRun}, o); p.Kind != "" || !slices.Contains(lines, "area harness busy (run 100)") {
		t.Fatalf("harness candidate vs continuation run: Next = %+v, log %v; want held", p, lines)
	}
}

// TestNextContinueHoldsPRAreas covers the continuation side (issue #137): an
// in-flight `CodeAF #5 (lane 1)` run on a pull request that closes an
// area:harness issue blocks an area:harness candidate, while the pull
// request itself is not picked again as a continuation candidate — and with
// nothing in flight, the continuation is picked ahead of the queued issues.
func TestNextContinueHoldsPRAreas(t *testing.T) {
	o := nextOpts()
	o.MaxParallelRuns = 2
	pr := queuedIssue(5, "2024-01-01T00:00:00Z", "codeaf", "codeaf-continue-needed")
	pr.Body = "Closes #119"
	pr.PullRequest = json.RawMessage(`{"number":5}`)
	linked := areaIssue(119, "2024-01-01T00:00:00Z", "harness")
	cont := running(5, 1, 100)

	// A harness candidate is blocked by the continuation's PR areas.
	held := []item{pr, linked, areaIssue(3, "2024-01-01T00:00:00Z", "harness")}
	if p, lines := nextPick(held, []runStatus{cont}, o); p.Kind != "" || !slices.Contains(lines, "area harness busy (run 100)") {
		t.Fatalf("harness candidate vs in-flight continuation: Next = %+v, log %v; want held", p, lines)
	}
	// The pull request is not picked again while its run is in flight; an
	// candidate on another area still starts, on the free lane.
	free := []item{pr, linked, areaIssue(4, "2024-01-01T00:00:00Z", "app/x")}
	if p := Next(free, []runStatus{cont}, o); p.Kind != kindIssue || p.Number != 4 || p.Lane != 0 {
		t.Fatalf("app candidate vs in-flight continuation: Next = %+v; want 4 on lane 0", p)
	}
	// With nothing in flight the continuation is picked before the issues.
	if p := Next(held, nil, o); p.Kind != kindContinue || p.Number != 5 || p.Lane != 0 {
		t.Fatalf("nothing in flight: Next = %+v; want continue #5 on lane 0", p)
	}
}

// TestNextCountsReviewsAgainstLanes covers the capacity rule across kinds
// (issue #137): in-flight runs of every kind — reviews, continuations, plan
// runs and issue runs — count against max_parallel_runs alike, so with four
// in flight and a capacity of four nothing is picked, for the usual reason.
func TestNextCountsReviewsAgainstLanes(t *testing.T) {
	o := nextOpts()
	o.MaxParallelRuns = 4
	pr := queuedIssue(124, "2024-01-01T00:00:00Z", "codeaf", "codeaf-review-needed")
	pr.PullRequest = json.RawMessage(`{"number":124}`)
	runs := []runStatus{
		reviewRun(120, 0, 1),
		running(121, 1, 2),
		{Status: "in_progress", DatabaseId: 3, DisplayTitle: "CodeAF plan (lane 2)"},
		{Status: "queued", DatabaseId: 4, DisplayTitle: "CodeAF #122 (lane 3)"},
	}
	issues := []item{pr, areaIssue(3, "2024-01-01T00:00:00Z", "app/x")}
	p, lines := nextPick(issues, runs, o)
	if p.Kind != "" || p.Number != 0 || !slices.Contains(lines, "holding #124: lane capacity full") {
		t.Fatalf("Next = %+v, log %v; want held, lane capacity full", p, lines)
	}
	// One run finished: the review is picked, on the freed lane.
	free := []runStatus{reviewRun(120, 0, 1), running(121, 1, 2),
		{Status: "in_progress", DatabaseId: 3, DisplayTitle: "CodeAF plan (lane 2)"},
		{Status: "completed", DatabaseId: 4, DisplayTitle: "CodeAF #122 (lane 3)"}}
	if p := Next(issues, free, o); p.Kind != kindReview || p.Number != 124 || p.Lane != 3 {
		t.Fatalf("one lane free: Next = %+v; want review #124 on lane 3", p)
	}
}

// TestRunLaneReadsReviewTitles pins the strict parse of a review run's
// display title (issue #137): only "CodeAF review #<n>" and "CodeAF review
// #<n> (lane <n>)" match, and anything else — wrong prefix, letters in the
// lane, trailing prose, the empty title — is no review run at all. A review
// title parses to issue 0 in runTitleFields, which keeps matching only
// `CodeAF #<n>` shapes; and runLane reads the review lane.
func TestRunLaneReadsReviewTitles(t *testing.T) {
	cases := []struct {
		title  string
		number int
		lane   int
		review bool
		// issue marks an issue title the list carries to pin the shapes'
		// disjointness: it parses to a real issue in runTitleFields.
		issue bool
	}{
		{"CodeAF review #12", 12, 0, true, false},
		{"CodeAF review #12 (lane 0)", 12, 0, true, false},
		{"CodeAF review #12 (lane 3)", 12, 3, true, false},
		{"CodeAF review #12 (lane 99)", 12, 99, true, false}, // out of range, still its value
		{"CodeAF review #12 (lane abc)", 0, 0, false, false}, // lane letters
		{"CodeAF review #12 (lane )", 0, 0, false, false},
		{"CodeAF review #12 (lane -1)", 0, 0, false, false},
		{"CodeAF review 12 (lane 3)", 0, 0, false, false},        // no #
		{"CodeAF reviews #12 (lane 3)", 0, 0, false, false},      // wrong prefix
		{"codeaf review #12 (lane 3)", 0, 0, false, false},       // case
		{"CodeAF review #12 (lane 3) extra", 0, 0, false, false}, // trailing prose
		{"CodeAF #12 (lane 3)", 0, 0, false, true},               // an issue title
		{"CodeAF plan (lane 3)", 0, 0, false, false},             // a plan title
		{"", 0, 0, false, false},                                 // empty title
	}
	for _, c := range cases {
		number, lane, ok := runReviewFields(c.title)
		if ok != c.review || (ok && (number != c.number || lane != c.lane)) {
			t.Errorf("runReviewFields(%q) = (%d, %d, %v); want (%d, %d, %v)",
				c.title, number, lane, ok, c.number, c.lane, c.review)
		}
		if got := isReviewRun(runStatus{DisplayTitle: c.title}); got != c.review {
			t.Errorf("isReviewRun(%q) = %v; want %v", c.title, got, c.review)
		}
		// runLane reads the review lane, and only the review lane.
		if got := runLane(runStatus{DisplayTitle: c.title}); c.review && got != c.lane {
			t.Errorf("runLane(%q) = %d; want %d", c.title, got, c.lane)
		}
		// runTitleFields keeps matching only `CodeAF #<n>` shapes: a review
		// title parses to issue 0 there.
		if issue, _ := runTitleFields(c.title); issue != 0 && !c.issue {
			t.Errorf("runTitleFields(%q) = %d; want 0 for a review shape", c.title, issue)
		}
	}
	// An issue title still parses as before, review parsing or no.
	if issue, lane := runTitleFields("CodeAF #12 (lane 3)"); issue != 12 || lane != 3 {
		t.Fatalf("runTitleFields issue title = (%d, %d); want (12, 3)", issue, lane)
	}
}

// TestRunNextOutputShape pins `harness next`'s stdout in the three pick
// cases (issue #137): exactly "kind=<kind>", then "number=<n>", then
// "lane=<n>", and nothing else — and an empty stdout when the queue holds.
func TestRunNextOutputShape(t *testing.T) {
	if _, err := loadConfig("models.json"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROXY_MODELS", "models.json")
	t.Setenv("CODEAF_WEEK_SPENT_USD", "1")

	dir := t.TempDir()
	issuesPath := filepath.Join(dir, "issues.json")
	runsPath := filepath.Join(dir, "runs.json")
	write := func(path, data string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pr := func(n int, extra string) string {
		return fmt.Sprintf(`{"number":%d,"state":"open","created_at":"2024-01-01T00:00:00Z","pull_request":{"number":%d},"labels":[{"name":"codeaf"}%s]}`, n, n, extra)
	}
	cases := []struct {
		name   string
		issues string
		runs   string
		want   string
	}{
		// The issue body must pass the acceptance gate (issue #132/#136):
		// both sections, readyBody's, or the gate holds it and runNext
		// prints nothing.
		{"issue", fmt.Sprintf(`[{"number":3,"state":"open","created_at":"2024-01-01T00:00:00Z","body":%q,"labels":[{"name":"codeaf-queued"},{"name":"area:app/x"}]}]`, readyBody),
			`[{"status":"completed"}]`, "kind=issue\nnumber=3\nlane=0\n"},
		{"review", "[" + pr(124, `,{"name":"codeaf-review-needed"}`) + "]",
			`[{"status":"completed"}]`, "kind=review\nnumber=124\nlane=0\n"},
		{"continue", "[" + pr(125, `,{"name":"codeaf-continue-needed"},{"name":"area:app/x"}`) + "]",
			`[{"status":"completed"}]`, "kind=continue\nnumber=125\nlane=0\n"},
		{"held at the capacity", "[" + pr(124, `,{"name":"codeaf-review-needed"}`) + "]",
			`[{"status":"in_progress","databaseId":1,"displayTitle":"CodeAF review #120 (lane 0)"},
			  {"status":"in_progress","databaseId":2,"displayTitle":"CodeAF #121 (lane 1)"},
			  {"status":"in_progress","databaseId":3,"displayTitle":"CodeAF plan (lane 2)"},
			  {"status":"queued","databaseId":4,"displayTitle":"CodeAF #122 (lane 3)"}]`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			write(issuesPath, c.issues)
			write(runsPath, c.runs)
			if out, err := runNextStdout(t, issuesPath, runsPath); err != nil || out != c.want {
				t.Fatalf("runNext printed %q, %v; want %q", out, err, c.want)
			}
		})
	}
}

// readyBody is a body that passes the acceptance gate: both sections, with
// a bullet under Done when and text under End-to-end check, and no path in
// it that would add an area. and text under End-to-end check, and no path in
// it that would add an area.
const readyBody = "## Problem\n\np\n\n## Done when\n\n- TestIssueReady passes\n\n## End-to-end check\n\nharness next holds #117\n"

func TestIssueReady(t *testing.T) {
	withArea := func(body string, labels ...string) item {
		return queuedIssue(3, "2024-01-01T00:00:00Z", append([]string{"codeaf-queued", "area:harness"}, labels...)...).with(body)
	}
	cases := []struct {
		name string
		it   item
		want []string
	}{
		{"both sections and an area", withArea(readyBody), nil},
		{"no area label", queuedIssue(3, "2024-01-01T00:00:00Z", "codeaf-queued").with(readyBody), []string{"area label"}},
		{"done when without bullets", withArea("## Done when\n\nno bullets here\n\n## End-to-end check\n\ntext\n"), []string{"## Done when bullets"}},
		{"done when missing", withArea("## End-to-end check\n\ntext\n"), []string{"## Done when bullets"}},
		{"end-to-end missing", withArea("## Done when\n\n- bullet\n"), []string{"## End-to-end check"}},
		{"end-to-end empty", withArea("## Done when\n\n- bullet\n\n## End-to-end check\n\n   \n"), []string{"## End-to-end check"}},
		{"headings in any case and level", withArea("# DONE WHEN\n\n- bullet\n\n### end-to-end check\n\ntext\n"), nil},
		{"both missing", withArea("## Problem\n\np\n"), []string{"## Done when bullets", "## End-to-end check"}},
		{"area and both missing", queuedIssue(3, "2024-01-01T00:00:00Z", "codeaf-queued").with("## Problem\n\np\n"), []string{"area label", "## Done when bullets", "## End-to-end check"}},
		{"ref-update body is exempt", withArea("generated body, no sections\n", "codeaf-ref-update"), nil},
		{"experiment body is exempt", withArea("generated body, no sections\n", "codeaf-experiment"), nil},
		{"ref-update without an area still misses the area", queuedIssue(3, "2024-01-01T00:00:00Z", "codeaf-queued", "codeaf-ref-update").with("no sections\n"), []string{"area label"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := issueReady(c.it); !slices.Equal(got, c.want) {
				t.Fatalf("issueReady = %v; want %v", got, c.want)
			}
		})
	}
	// Headings at level 4 or deeper are not acceptance sections: a body
	// whose "Done when" is only a #### heading misses its bullets, and the
	// section the #### heading sits in still ends at the next real
	// heading. Widening the heading regex to any level would pass the
	// suite, so both halves are pinned.
	if got := issueReady(withArea("#### Done when\n\n- bullet\n\n## End-to-end check\n\ntext\n")); !slices.Equal(got, []string{"## Done when bullets"}) {
		t.Fatalf("level-4 Done when accepted: issueReady = %v; want [## Done when bullets]", got)
	}
	if got := issueReady(withArea("## Done when\n\n- bullet\n\n#### end-to-end check\n\ntext\n")); !slices.Equal(got, []string{"## End-to-end check"}) {
		t.Fatalf("level-4 End-to-end check accepted: issueReady = %v; want [## End-to-end check]", got)
	}
	if got := issueReady(withArea("#### done when\n\n- bullet\n\n#### end-to-end check\n\ntext\n")); !slices.Equal(got, []string{"## Done when bullets", "## End-to-end check"}) {
		t.Fatalf("both level-4 accepted: issueReady = %v; want both missing", got)
	}
	// The untouched issue template is rejected: its Done when has no
	// bullet and its End-to-end check is still the placeholder (issue
	// #136 review), so an issue opened from it and never filled in is
	// held, not queued.
	template, err := os.ReadFile("issue-template.md")
	if err != nil {
		t.Fatal(err)
	}
	if got := issueReady(withArea(string(template))); !slices.Equal(got, []string{"## Done when bullets", "## End-to-end check"}) {
		t.Fatalf("untouched template passes the gate: issueReady = %v; want both missing", got)
	}
	// The End-to-end check placeholder marks hold a body whose section is
	// otherwise well-formed: the whole placeholder, a copy truncated after
	// the first phrase ("(fill in before") and a half-edited copy that
	// keeps only the tail phrase. The truncated probe is the review
	// finding on the first fix round: holding on the full phrase alone let
	// it through (issue #136).
	e2eSection := "## Done when\n\n- bullet\n\n## End-to-end check\n\n"
	for name, e2e := range map[string]string{
		"whole placeholder":   proposalE2EPlaceholder,
		"truncated head":      "(fill in before",
		"half-edited tail":    "the seams work)",
		"phrase plus command": e2ePlaceholderMarks[0] + " queueing; run: go test ./...",
	} {
		if got := issueReady(withArea(e2eSection + e2e + "\n")); !slices.Equal(got, []string{"## End-to-end check"}) {
			t.Fatalf("%s placeholder accepted: issueReady = %v; want [## End-to-end check]", name, got)
		}
	}
	// A real check with none of the placeholder's phrases passes.
	if got := issueReady(withArea(e2eSection + "go test -race ./... in .github/codeaf is green\n")); len(got) != 0 {
		t.Fatalf("real End-to-end check held: issueReady = %v", got)
	}
	// Ordinary prose that happens to echo the template's wording passes:
	// only the placeholder's own tail, with its closing parenthesis, marks
	// it unfilled (issue #136 review: "the seams work" held a real check).
	if got := issueReady(withArea(e2eSection + "Re-run the #61 patch without its gate change: go test fails, which proves the seams work.\n")); len(got) != 0 {
		t.Fatalf("real check echoing the template held: issueReady = %v", got)
	}
	// Text hidden in an HTML comment is not a check (issue #136 review): a
	// section holding only a comment — the template's instructions, a
	// reviewer's note — is unfilled, and an unclosed "<!--" comments out
	// the rest of the section. A real check next to a commented-out
	// placeholder passes: the marks are read on the visible text.
	for name, e2e := range map[string]string{
		"comment only":        "<!-- note -->",
		"comment and blanks":  "<!-- instructions -->\n\n   \n",
		"unclosed comment":    "<!-- note\n",
		"commented-out check": "<!-- go test ./... is green -->",
	} {
		if got := issueReady(withArea(e2eSection + e2e + "\n")); !slices.Equal(got, []string{"## End-to-end check"}) {
			t.Fatalf("%s accepted as a check: issueReady = %v; want [## End-to-end check]", name, got)
		}
	}
	if got := issueReady(withArea(e2eSection + "<!-- " + proposalE2EPlaceholder + " -->\n\ngo test -race ./... is green\n")); len(got) != 0 {
		t.Fatalf("real check beside a commented-out placeholder held: issueReady = %v", got)
	}
	// A body over 64 KiB is scanned only up to the cap: the sections past
	// it are invisible to the gate, the ones inside it are found.
	filler := strings.Repeat("filler line\n", 1)
	huge := strings.Repeat("filler line\n", 1<<16)
	if got := issueReady(withArea(huge + readyBody)); !slices.Equal(got, []string{"## Done when bullets", "## End-to-end check"}) {
		t.Fatalf("sections past the cap: issueReady = %v; want both missing", got)
	}
	if got := issueReady(withArea(filler + readyBody)); len(got) != 0 {
		t.Fatalf("sections inside the cap: issueReady = %v; want none", got)
	}
}

// with returns the item with a body; a small helper the gate tests read by.
func (i item) with(body string) item { i.Body = body; return i }

func TestNextHoldsUnreadyIssue(t *testing.T) {
	o := nextOpts()
	var lines []string
	o.Logf = func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	unready := queuedIssue(117, "2024-01-01T00:00:00Z", "codeaf-queued", "area:harness")
	unready.Body = "## Problem\n\nno acceptance sections\n"
	ready := areaIssue(118, "2024-01-01T00:00:00Z", "app/x")
	ready.Body = readyBody
	// Alone, the unready issue is held with the not-ready reason: no
	// pick, and the hold logged (issue #137: the reason rides on Logf).
	if p := Next([]item{unready}, nil, o); p.Kind != "" {
		t.Fatalf("unready alone: Next = %+v; want no pick", p)
	}
	if !slices.Contains(lines, "holding #117: not ready: ## Done when bullets, ## End-to-end check") {
		t.Fatalf("log lines = %v; want the not-ready hold", lines)
	}
	// The next ready candidate still starts: the hold blocks no one.
	if p := Next([]item{unready, ready}, nil, o); p.Kind != kindIssue || p.Number != 118 {
		t.Fatalf("unready first: Next = %+v; want issue 118", p)
	}
	if !slices.Contains(lines, "holding #117: not ready: ## Done when bullets, ## End-to-end check") {
		t.Fatalf("log lines = %v; want the not-ready hold", lines)
	}
}

// TestNextHoldsUnreadyIssueWithNoArea pins the combined hold (issue #136
// review): an issue missing the area label and both sections is held with a
// reason that names all three, not just the area the earlier check saw
// first, and the hold comment the queue posts names them too.
func TestNextHoldsUnreadyIssueWithNoArea(t *testing.T) {
	o := nextOpts()
	var lines []string
	o.Logf = func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	held := map[int][]string{}
	o.NotReadyHeld = func(n int, missing []string) { held[n] = missing }
	it := queuedIssue(42, "2024-01-01T00:00:00Z", "codeaf-queued")
	it.Body = "## Problem\n\np\n"
	if p := Next([]item{it}, nil, o); p.Kind != "" {
		t.Fatalf("Next = %+v; want no pick", p)
	}
	if !slices.Contains(lines, "holding #42: not ready: area label, ## Done when bullets, ## End-to-end check") {
		t.Fatalf("log lines = %v; want the combined hold", lines)
	}
	if want := []string{"area label", "## Done when bullets", "## End-to-end check"}; !slices.Equal(held[42], want) {
		t.Fatalf("NotReadyHeld missing = %v; want %v", held[42], want)
	}
	if !slices.Contains(lines, "holding #42: not ready: area label, ## Done when bullets, ## End-to-end check") {
		t.Fatalf("log lines = %v; want the combined hold", lines)
	}
	if body := notReadyHoldCommentBody(42, held[42]); !strings.Contains(body, "Missing: area label, ## Done when bullets, ## End-to-end check") {
		t.Fatalf("hold comment = %q; want it to name all three", body)
	}
}

func TestNextExemptsRefUpdate(t *testing.T) {
	o := nextOpts()
	var lines []string
	o.Logf = func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	it := queuedIssue(9, "2024-01-01T00:00:00Z", "codeaf-queued", "area:harness", "codeaf-ref-update")
	it.Body = "Upstream head: abc\n\n## What CodeAF should do\n\n1. Report.\n"
	if p := Next([]item{it}, nil, o); p.Kind != kindIssue || p.Number != 9 {
		t.Fatalf("ref-update without the sections: Next = %+v; want issue 9", p)
	}
	// Exempt from the body checks, not the area check: without an area the
	// issue is still held.
	bare := queuedIssue(11, "2024-01-01T00:00:00Z", "codeaf-queued", "codeaf-ref-update")
	bare.Body = it.Body
	if p := Next([]item{bare}, nil, o); p.Kind != "" || !slices.Contains(lines, "holding #11: no area label") {
		t.Fatalf("ref-update without an area: Next = %+v, lines = %v; want held: no area label", p, lines)
	}
}
