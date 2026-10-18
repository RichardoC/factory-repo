// closing_test.go covers the store ending (issue #134): a run that finished
// in the store reports `stop: incomplete` with an empty answer, and the run
// record beside the result carries the real answer and the end reason. The
// fixtures in testdata/store-finished are shared by the report-command tests.
package main

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	// fixtureResult is the shared result envelope: an incomplete stop with
	// an empty answer, exactly what a store-finished run leaves behind.
	fixtureResult = "testdata/store-finished/codeaf-result.json"

	fixtureReview = "testdata/store-finished/review.jsonl"
	fixturePlan   = "testdata/store-finished/plan.jsonl"
	fixtureIssue  = "testdata/store-finished/issue.jsonl"

	// reviewFindings is the answer the review fixture's end line carries.
	reviewFindings = "## Findings\n\n- [high] app/queue/main.go:42 - the mutex is released twice on the error path\n- [low] docs/readme.md:3 - a typo in the flag name\n"
)

func useFixture(t *testing.T, trajectory string) {
	t.Helper()
	t.Setenv("RESULT_PATH", fixtureResult)
	t.Setenv("TRAJECTORY_PATH", trajectory)
}

// TestReadClosingStoreFinished: every fixture gives StoreFinished and the
// expected answer; a record without an end line is the zero value; a 3 MiB
// record still finds its end line, because the read starts from the tail.
func TestReadClosingStoreFinished(t *testing.T) {
	for name, want := range map[string]struct {
		answer string
	}{
		fixtureReview: {strings.TrimSpace(strings.SplitN(reviewFindings, "\n\n", 2)[1])},
		fixturePlan:   {"### Proposal: Fix the queue drain bug"},
		fixtureIssue:  {"The run implemented the queue drain fix"},
	} {
		c := readClosing(name)
		if !c.StoreFinished {
			t.Errorf("%s: StoreFinished = false, want true", name)
		}
		if c.Reason != storeFinishedReason {
			t.Errorf("%s: Reason = %q", name, c.Reason)
		}
		// The answer is long; check its start and that the tail read kept
		// the whole record, not just its head.
		if len(c.Answer) == 0 {
			t.Errorf("%s: Answer is empty", name)
		}
		if !strings.Contains(c.Answer, want.answer) {
			t.Errorf("%s: Answer does not carry %q: %.120q", name, want.answer, c.Answer)
		}
	}

	t.Run("a record with no end line is the zero value", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "trajectory.jsonl")
		if err := os.WriteFile(path, []byte("{\"kind\":\"step\",\"n\":1}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if c := readClosing(path); c != (closing{}) {
			t.Errorf("readClosing = %+v, want the zero value", c)
		}
	})

	t.Run("a missing record is the zero value", func(t *testing.T) {
		if c := readClosing(filepath.Join(t.TempDir(), "absent.jsonl")); c != (closing{}) {
			t.Errorf("readClosing = %+v, want the zero value", c)
		}
	})

	t.Run("a 3 MiB record still finds its end line", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "trajectory.jsonl")
		padding := strings.Repeat("#", 3<<20) + "\n"
		record := padding + `{"kind":"end","reason":"finished in the store","result":"the store answer"}` + "\n"
		if err := os.WriteFile(path, []byte(record), 0o644); err != nil {
			t.Fatal(err)
		}
		c := readClosing(path)
		if !c.StoreFinished || c.Answer != "the store answer" {
			t.Errorf("readClosing = %+v, want the end line read from the tail", c)
		}
	})
}

// TestPrModePromotesStoreFinished: with the issue fixture the PR is ready,
// not a draft; with a plain incomplete result and no record it stays early.
func TestPrModePromotesStoreFinished(t *testing.T) {
	t.Run("a store-finished run is ready", func(t *testing.T) {
		useFixture(t, fixtureIssue)
		out, err := captureStdout(t, prMode)
		if err != nil {
			t.Fatal(err)
		}
		if out != "early=false\nready=true\nstop=done\n" {
			t.Errorf("stdout = %q", out)
		}
	})

	t.Run("a plain incomplete result with no record stays early", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "result.json")
		if err := os.WriteFile(path, []byte(`{"stop":"incomplete","answer":""}`), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("RESULT_PATH", path) // no codeaf-trajectory.jsonl beside it
		t.Setenv("TRAJECTORY_PATH", "")
		out, err := captureStdout(t, prMode)
		if err != nil {
			t.Fatal(err)
		}
		if out != "early=true\nready=false\nstop=incomplete\n" {
			t.Errorf("stdout = %q", out)
		}
	})
}

// TestContinuePlanDoesNotContinueStoreFinished: a store-finished run counts
// as done, so no continuation starts.
func TestContinuePlanDoesNotContinueStoreFinished(t *testing.T) {
	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs })
	os.Args = []string{"harness", "continue-plan", "7"}
	useFixture(t, fixtureIssue)
	out, err := captureStdout(t, runContinuePlan)
	if err != nil {
		t.Fatal(err)
	}
	if out != "action=none\n" {
		t.Errorf("stdout = %q, want action=none", out)
	}
}

// TestReviewReportFromClosingResult: the review fixture's findings reach the
// report through the closing, one blocking and one advisory, and both are
// listed in the comment.
func TestReviewReportFromClosingResult(t *testing.T) {
	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs })
	os.Args = []string{"harness", "review-report", "7"}
	server := commentsServer(t, reportComments(1), http.StatusOK)
	ghEnv(t, server)
	useFixture(t, fixtureReview)
	reviewLedger(t)
	comment := filepath.Join(t.TempDir(), "nested", "review-comment.md")
	t.Setenv("REVIEW_COMMENT_PATH", comment)
	out, err := captureStdout(t, runReviewReport)
	if err != nil {
		t.Fatal(err)
	}
	if out != "action=fix\nused=0\nblocking=1\n" {
		t.Errorf("stdout = %q", out)
	}
	data, err := os.ReadFile(comment)
	if err != nil {
		t.Fatalf("comment not written: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, "the mutex is released twice") || !strings.Contains(body, "a typo in the flag name") {
		t.Errorf("the comment does not list both findings: %.400q", body)
	}
}

// TestPlanReportFromClosingResult: the plan fixture's two proposals reach
// PROPOSALS_PATH through the closing.
func TestPlanReportFromClosingResult(t *testing.T) {
	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs })
	os.Args = []string{"harness", "plan-report", "7"}
	useFixture(t, fixturePlan)
	proposals := filepath.Join(t.TempDir(), "nested", "proposals.json")
	t.Setenv("PROPOSALS_PATH", proposals)
	openIssues := filepath.Join(t.TempDir(), "open-issues.json")
	if err := os.WriteFile(openIssues, []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPEN_ISSUES_PATH", openIssues)
	out, err := captureStdout(t, runPlanReport)
	if err != nil {
		t.Fatal(err)
	}
	if out != "proposals=2 held=0\n" {
		t.Errorf("stdout = %q", out)
	}
	data, err := os.ReadFile(proposals)
	if err != nil {
		t.Fatalf("proposals not written: %v", err)
	}
	for _, title := range []string{"Fix the queue drain bug", "Add a README TODO list for app/widgets"} {
		if !strings.Contains(string(data), title) {
			t.Errorf("proposals are missing %q: %s", title, data)
		}
	}
}

// TestReportShowsPromotion: the report carries the closing row when the
// promotion happened, and not for a plain done result.
func TestReportShowsPromotion(t *testing.T) {
	t.Setenv("HARNESS_DIR", ".")
	useFixture(t, fixtureIssue)
	reviewLedger(t)

	read := func() string {
		out, err := captureStdout(t, runReport)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	body := read()
	if !strings.Contains(body, "| closing | finished in the store (stop promoted from incomplete to done) |") {
		t.Errorf("the report does not name the promotion: %.600q", body)
	}
	if strings.Contains(body, "### Unfinished work") {
		t.Errorf("a store-finished run with an answer is not unfinished: %.600q", body)
	}

	t.Run("a done result shows no closing row", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "result.json")
		if err := os.WriteFile(path, []byte(`{"stop":"done","ok":true,"answer":"all finished"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("RESULT_PATH", path)
		t.Setenv("TRAJECTORY_PATH", "")
		if body := read(); strings.Contains(body, "| closing |") {
			t.Errorf("a done result grew a closing row: %.400q", body)
		}
	})
}

// TestSummaryShipsRecordInEveryMode: the run record ships beside the result
// whatever MODE says, so every report command can read the closing.
func TestSummaryShipsRecordInEveryMode(t *testing.T) {
	for _, mode := range []string{"", "issue", "pr", "plan", "review"} {
		t.Run("mode="+mode, func(t *testing.T) {
			home := t.TempDir()
			out := filepath.Join(t.TempDir(), "out")
			if err := os.MkdirAll(filepath.Join(home, "runs", "7", "tasks", "root"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(out, 0o755); err != nil {
				t.Fatal(err)
			}
			record := filepath.Join(home, "runs", "7", "tasks", "root", "trajectory.jsonl")
			if err := os.WriteFile(record, []byte("{\"kind\":\"end\",\"result\":\"the answer\"}\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(out, "result.json"), []byte(`{"stop":"done"}`), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CODEAF_HOME", home)
			t.Setenv("RESULT_PATH", filepath.Join(out, "result.json"))
			t.Setenv("MODE", mode)
			if _, err := captureStdout(t, runSummary); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(out, trajectoryFileName)); err != nil {
				t.Errorf("mode %q did not ship the record: %v", mode, err)
			}
		})
	}
}

// TestPromotionIsNarrow (the trust rule behind issue #134): an answer from
// the untrusted run record promotes only an incomplete stop, never another,
// and a record that does not say "finished in the store" promotes nothing.
func TestPromotionIsNarrow(t *testing.T) {
	promoted := closing{Answer: "text", Reason: storeFinishedReason, StoreFinished: true}
	if got := effectiveStop(Result{Stop: stopIncomplete}, promoted); got != stopDone {
		t.Errorf("effectiveStop = %q, want done", got)
	}
	if got := effectiveStop(Result{Stop: stopError}, promoted); got != stopError {
		t.Errorf("effectiveStop = %q, want error left alone", got)
	}
	if got := effectiveStop(Result{Stop: stopIncomplete}, closing{Answer: "text"}); got != stopIncomplete {
		t.Errorf("effectiveStop = %q, want incomplete left alone", got)
	}
	if got := effectiveAnswer(Result{Answer: "envelope"}, promoted); got != "envelope" {
		t.Errorf("effectiveAnswer = %q, want the envelope's answer", got)
	}
	if got := effectiveAnswer(Result{}, promoted); got != "text" {
		t.Errorf("effectiveAnswer = %q, want the closing's answer", got)
	}
	// A malformed record line is skipped, never fatal.
	path := filepath.Join(t.TempDir(), "trajectory.jsonl")
	lines := "{not json\n" + `{"kind":"end","reason":"finished in the store","result":"kept"}` + "\n"
	if err := os.WriteFile(path, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	if c := readClosing(path); !c.StoreFinished || c.Answer != "kept" {
		t.Errorf("readClosing = %+v", c)
	}
	if _, err := os.Stat(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}
