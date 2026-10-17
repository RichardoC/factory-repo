package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
)

// nextOpts is the queue the tests decide in: a $10 day cap and the three
// default labels.
func nextOpts() queueOptions {
	return queueOptions{
		Label:         "codeaf",
		QueuedLabel:   "codeaf-queued",
		PriorityLabel: "codeaf-priority",
		DayCap:        10,
	}
}

// queuedIssue builds an open issue carrying labels, in creation order.
func queuedIssue(n int, created string, labels ...string) item {
	it := item{Number: n, State: "open", CreatedAt: created}
	for _, name := range labels {
		it.Labels = append(it.Labels, label{Name: name})
	}
	return it
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
			if n, reason := Next(c.issues, nil, nextOpts()); n != 0 || reason != "nothing queued" {
				t.Fatalf("Next = %d, %q; want 0, nothing queued", n, reason)
			}
		})
	}
}

func TestNextBlocksOnARun(t *testing.T) {
	issues := []item{queuedIssue(3, "2024-01-01T00:00:00Z", "codeaf-queued")}
	for _, status := range []string{"queued", "in_progress"} {
		if n, reason := Next(issues, []runStatus{{Status: status}}, nextOpts()); n != 0 || reason != "a CodeAF run is in progress or waiting" {
			t.Fatalf("%s run: Next = %d, %q", status, n, reason)
		}
	}
	// A completed run does not block.
	if n, _ := Next(issues, []runStatus{{Status: "completed"}, {Status: "completed"}}, nextOpts()); n != 3 {
		t.Fatalf("after completed runs Next = %d", n)
	}
}

func TestNextSpendThreshold(t *testing.T) {
	issues := []item{queuedIssue(3, "2024-01-01T00:00:00Z", "codeaf-queued")}
	o := nextOpts()  // day cap $10
	o.DaySpent = 7.5 // exactly 0.75 of the cap
	if n, reason := Next(issues, nil, o); n != 0 || reason != "spend threshold reached" {
		t.Fatalf("at the threshold Next = %d, %q", n, reason)
	}
	o.DaySpent = 7.4999
	if n, _ := Next(issues, nil, o); n != 3 {
		t.Fatalf("just under the threshold Next = %d", n)
	}
}

// The spend total comes from the shared state, which is untrusted, so
// spentToday must refuse whatever does not parse as a finite total at least
// zero.
func TestNextSpentEnv(t *testing.T) {
	for _, bad := range []string{"abc", "-1", "NaN", "Inf", "-Inf", "1e400", "5 USD"} {
		t.Setenv("CODEAF_DAY_SPENT_USD", bad)
		if _, err := spentToday("CODEAF_DAY_SPENT_USD"); err == nil {
			t.Errorf("spentToday accepted %q", bad)
		}
	}
	t.Setenv("CODEAF_DAY_SPENT_USD", "1.25")
	if spent, err := spentToday("CODEAF_DAY_SPENT_USD"); err != nil || spent != 1.25 {
		t.Fatalf("spentToday(1.25) = %v, %v", spent, err)
	}
}

func TestNextDependsOn(t *testing.T) {
	candidate := queuedIssue(3, "2024-01-01T00:00:00Z", "codeaf-queued")
	dep := queuedIssue(12, "2024-01-02T00:00:00Z")
	openPR := queuedIssue(13, "2024-01-02T00:00:00Z")
	openPR.PullRequest = json.RawMessage(`{"number":13}`)
	closedPR := openPR
	closedPR.State = "closed"
	selfDep := queuedIssue(5, "2024-01-01T00:00:00Z", "codeaf-queued")
	selfDep.Body = "Depends on #5"
	blocked := candidate
	blocked.Body = "Depends on #12\n"
	dependsOnPR := candidate
	dependsOnPR.Body = "see #13; depends on #13"
	unknownDep := candidate
	unknownDep.Body = "Depends on #99"
	mixed := candidate
	mixed.Body = "Depends on #12, #13"
	other := queuedIssue(7, "2024-01-03T00:00:00Z", "codeaf-queued")

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
			n, reason := Next(c.issues, nil, nextOpts())
			if n != c.want || (c.reason != "" && reason != c.reason) {
				t.Fatalf("Next = %d, %q; want %d, %q", n, reason, c.want, c.reason)
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
	oldest := queuedIssue(7, "2024-01-01T00:00:00Z", "codeaf-queued")
	newer := queuedIssue(6, "2024-06-01T00:00:00Z", "codeaf-queued")
	priority := queuedIssue(9, "2024-09-01T00:00:00Z", "codeaf-queued", "codeaf-priority")
	priorityOlder := queuedIssue(4, "2024-02-01T00:00:00Z", "codeaf-queued", "codeaf-priority")
	twin := queuedIssue(5, "2024-03-01T00:00:00Z", "codeaf-queued")
	twinLater := queuedIssue(11, "2024-03-01T00:00:00Z", "codeaf-queued")
	badStamp := queuedIssue(1, "not-a-date", "codeaf-queued")

	if n, _ := Next([]item{newer, oldest}, nil, nextOpts()); n != 7 {
		t.Fatalf("oldest first: Next = %d", n)
	}
	if n, _ := Next([]item{oldest, priority, newer}, nil, nextOpts()); n != 9 {
		t.Fatalf("priority jumps the queue: Next = %d", n)
	}
	if n, _ := Next([]item{priority, priorityOlder}, nil, nextOpts()); n != 4 {
		t.Fatalf("two priority issues, oldest first: Next = %d", n)
	}
	if n, _ := Next([]item{twinLater, twin}, nil, nextOpts()); n != 5 {
		t.Fatalf("tie-break by number: Next = %d", n)
	}
	if n, _ := Next([]item{badStamp, newer}, nil, nextOpts()); n != 6 {
		t.Fatalf("unparseable created_at sorts last: Next = %d", n)
	}
	// The same input twice picks the same issue, whatever the order.
	a, _ := Next([]item{newer, oldest, priority}, nil, nextOpts())
	b, _ := Next([]item{priority, oldest, newer}, nil, nextOpts())
	if a != 9 || b != 9 {
		t.Fatalf("Next is not deterministic: %d then %d", a, b)
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
	issues := `[{"number":3,"state":"open","created_at":"2024-01-01T00:00:00Z","labels":[{"name":"codeaf-queued"}]},
	            {"number":9,"state":"open","created_at":"2024-02-01T00:00:00Z","body":"Depends on #3","labels":[{"name":"codeaf-queued"}]}]`
	write(issuesPath, issues)
	write(runsPath, `[{"status":"completed"}]`)

	// A queued issue is picked, and nothing but its number is printed.
	t.Setenv("CODEAF_DAY_SPENT_USD", "1")
	if out, err := runNextStdout(t, issuesPath, runsPath); err != nil || out != "3\n" {
		t.Fatalf("runNext printed %q, %v", out, err)
	}

	// Nothing to run: empty stdout, no error.
	write(issuesPath, `[{"number":3,"state":"open","created_at":"2024-01-01T00:00:00Z","labels":[{"name":"codeaf"}]}]`)
	if out, err := runNextStdout(t, issuesPath, runsPath); err != nil || out != "" {
		t.Fatalf("runNext printed %q, %v; wanted nothing", out, err)
	}

	// A run in progress: empty stdout, no error.
	write(issuesPath, issues)
	write(runsPath, `[{"status":"in_progress"}]`)
	if out, err := runNextStdout(t, issuesPath, runsPath); err != nil || out != "" {
		t.Fatalf("runNext printed %q, %v; wanted nothing", out, err)
	}

	// The threshold is taken against the config's day cap.
	t.Setenv("CODEAF_DAY_SPENT_USD", fmt.Sprintf("%v", cfg.Limits.DayUSD*0.75))
	if out, err := runNextStdout(t, issuesPath, runsPath); err != nil || out != "" {
		t.Fatalf("runNext at the threshold printed %q, %v; wanted nothing", out, err)
	}

	// Bad and missing spend totals fail the command.
	for _, spent := range []string{"-3", "tomorrow", ""} {
		t.Setenv("CODEAF_DAY_SPENT_USD", spent)
		if _, err := runNextStdout(t, issuesPath, runsPath); err == nil {
			t.Fatalf("runNext accepted CODEAF_DAY_SPENT_USD=%q", spent)
		}
	}

	// Malformed input fails the command.
	t.Setenv("CODEAF_DAY_SPENT_USD", "1")
	write(issuesPath, `{}`)
	if _, err := runNextStdout(t, issuesPath, runsPath); err == nil {
		t.Fatal("runNext accepted malformed issues JSON")
	}
	if _, err := runNextStdout(t, filepath.Join(dir, "missing.json"), runsPath); err == nil {
		t.Fatal("runNext accepted a missing file")
	}
}
