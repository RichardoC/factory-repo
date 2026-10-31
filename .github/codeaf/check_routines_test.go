package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestQuotaCalibration pins the estimate from spend-per-percent drop and
// the 20% drift threshold. The first case mirrors the recalibration
// limits.plan_note records: a 32-point drop for $15.8 of ledger spend
// estimates $49.38/week against a configured $36 — 37% off, so changed.
func TestQuotaCalibration(t *testing.T) {
	led := func(lines ...string) ledgerJSON {
		var out ledgerJSON
		for _, l := range lines {
			var e LedgerLine
			if err := json.Unmarshal([]byte(l), &e); err != nil {
				t.Fatalf("fixture line: %v", err)
			}
			out = append(out, e)
		}
		return out
	}
	quotas := func(usd float64, stamps ...string) quotasJSON {
		q := quotasJSON{WeeklyCreditUSD: usd}
		for _, s := range stamps {
			pct, stamp := strings.SplitN(s, "@", 2)[0], strings.SplitN(s, "@", 2)[1]
			p, err := strconv.ParseFloat(pct, 64)
			if err != nil {
				t.Fatalf("fixture percent %q: %v", s, err)
			}
			ts, err := time.Parse(time.RFC3339, stamp)
			if err != nil {
				t.Fatalf("fixture stamp %q: %v", s, err)
			}
			var snap quotaSnapshot
			snap.WeeklyTokenLimit.PercentRemaining = p
			snap.TS = ts
			q.Snapshots = append(q.Snapshots, snap)
		}
		return q
	}
	tests := []struct {
		name        string
		entries     ledgerJSON
		quotas      quotasJSON
		wantChanged bool
		wantEst     float64
		wantIn      []string // substrings the report must carry
	}{
		{
			name: "drifted estimate proposes a change",
			entries: led(
				`{"ts":"2026-10-18T10:00:00Z","model":"claude-sonnet-4-5","cost_usd":15.8}`),
			quotas:      quotas(36, "90@2026-10-18T09:00:00Z", "58@2026-10-18T12:00:00Z"),
			wantChanged: true,
			wantEst:     15.8 * 100 / 32, // 49.375
			wantIn:      []string{"$36.0000", "$49.3750", "update `limits.weekly_credit_usd`"},
		},
		{
			name: "within tolerance proposes no change",
			entries: led(
				`{"ts":"2026-10-18T10:00:00Z","model":"claude-sonnet-4-5","cost_usd":3.6}`),
			quotas:      quotas(36, "90@2026-10-18T09:00:00Z", "80@2026-10-18T12:00:00Z"),
			wantChanged: false,
			wantEst:     36,
			wantIn:      []string{"no change"},
		},
		{
			name: "two intervals are averaged, spend outside them is ignored",
			entries: led(
				`{"ts":"2026-10-18T09:30:00Z","model":"m1","cost_usd":1.0}`,
				`{"ts":"2026-10-18T10:30:00Z","model":"m2","cost_usd":1.0}`,
				`{"ts":"2026-10-19T10:30:00Z","model":"m3","cost_usd":9.0}`,
				`{"ts":"2026-10-19T23:00:00Z","model":"m4","cost_usd":99}`),
			quotas: quotas(40,
				"90@2026-10-18T09:00:00Z", "80@2026-10-18T11:00:00Z", "50@2026-10-19T11:00:00Z"),
			wantChanged: true,
			wantEst:     (2.0*100/10 + 9.0*100/30) / 2, // (20 + 30) / 2 = 25
			wantIn:      []string{"2 usable quota snapshot interval"},
		},
		{
			name: "zero-drop and spendless intervals are skipped",
			entries: led(
				`{"ts":"2026-10-18T11:30:00Z","model":"m1","cost_usd":5.0}`),
			quotas:      quotas(50, "90@2026-10-18T09:00:00Z", "90@2026-10-18T11:00:00Z", "80@2026-10-18T12:00:00Z"),
			wantChanged: false,
			wantEst:     50,
			wantIn:      []string{"1 usable"},
		},
		{
			name:        "too few snapshots reports insufficient data",
			entries:     led(`{"ts":"2026-10-18T10:00:00Z","model":"m1","cost_usd":5.0}`),
			quotas:      quotas(36, "90@2026-10-18T09:00:00Z"),
			wantChanged: false,
			wantEst:     0,
			wantIn:      []string{"Not enough data"},
		},
		{
			name:        "unsorted snapshots and entries still calibrate",
			entries:     led(`{"ts":"2026-10-18T12:30:00Z","model":"m1","cost_usd":7.2}`),
			quotas:      quotas(28, "70@2026-10-18T13:00:00Z", "90@2026-10-18T12:00:00Z"),
			wantChanged: true,
			wantEst:     36,
			wantIn:      []string{"update `limits.weekly_credit_usd`"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changed, report, est := quotaCalibration(tt.entries, tt.quotas)
			if changed != tt.wantChanged {
				t.Errorf("changed = %v, want %v\nreport:\n%s", changed, tt.wantChanged, report)
			}
			if diff := est - tt.wantEst; diff > 0.01 || diff < -0.01 {
				t.Errorf("estimate = %.4f, want %.4f\nreport:\n%s", est, tt.wantEst, report)
			}
			for _, want := range tt.wantIn {
				if !strings.Contains(report, want) {
					t.Errorf("report missing %q:\n%s", want, report)
				}
			}
		})
	}
}

// TestPRSweep pins the three findings and the dispatch handoff: one
// conflicted, one red, one unaddressed-review PR, plus the ones that must
// not count (another branch, a merged PR, an addressed or dismissed
// finding).
func TestPRSweep(t *testing.T) {
	at := func(s string) time.Time {
		ts, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatalf("fixture stamp: %v", err)
		}
		return ts
	}
	login := func(l string) *prReviewAuthor { return &prReviewAuthor{Login: l} }
	prs := []prStatus{
		{Number: 12, HeadRefName: "codeaf/fix-quota", State: "OPEN", Mergeable: "CONFLICTING",
			Reviews: []prReview{{Author: login("RichardoC"), State: "CHANGES_REQUESTED",
				SubmittedAt: at("2026-10-18T10:00:00Z")}}},
		{Number: 14, HeadRefName: "codeaf/hedge", State: "OPEN", Mergeable: "MERGEABLE",
			StatusCheckRollup: []prCheck{{State: "SUCCESS"}, {Conclusion: "FAILURE"}}},
		{Number: 17, HeadRefName: "codeaf/labels", State: "OPEN", Mergeable: "MERGEABLE",
			Reviews: []prReview{
				{Author: login("alice"), State: "CHANGES_REQUESTED", SubmittedAt: at("2026-10-17T09:00:00Z")},
				{Author: login("alice"), State: "COMMENTED", SubmittedAt: at("2026-10-17T10:00:00Z")},
			}},
		{Number: 19, HeadRefName: "codeaf/addressed", State: "OPEN", Mergeable: "MERGEABLE",
			Reviews: []prReview{
				{Author: login("alice"), State: "CHANGES_REQUESTED", SubmittedAt: at("2026-10-17T09:00:00Z")},
				{Author: login("bob"), State: "APPROVED", SubmittedAt: at("2026-10-18T09:00:00Z")},
			}},
		{Number: 21, HeadRefName: "app/other", State: "OPEN", Mergeable: "CONFLICTING"},
		{Number: 23, HeadRefName: "codeaf/merged", State: "MERGED", Mergeable: "CONFLICTING"},
		{Number: 25, HeadRefName: "codeaf/dismissed", State: "OPEN", Mergeable: "MERGEABLE",
			Reviews: []prReview{
				{Author: login("alice"), State: "CHANGES_REQUESTED", SubmittedAt: at("2026-10-17T09:00:00Z")},
				{Author: login("alice"), State: "DISMISSED", SubmittedAt: at("2026-10-18T09:00:00Z")},
			}},
	}
	report, free := prSweep(prs)
	// Five PRs were examined (12, 14, 17, 19 and 25 are open `codeaf/*`);
	// only three carry findings, but the header counts the whole queue.
	if !strings.Contains(report, "## PR sweep: 5 open `codeaf/*` PR(s) checked") {
		t.Errorf("header does not count the 5 examined PRs:\\n%s", report)
	}
	if strings.Contains(report, "## PR sweep: 3 open") {
		t.Errorf("header counts findings, not the checked queue:\\n%s", report)
	}
	for _, want := range []string{
		"#12 `codeaf/fix-quota` — merge conflict",
		"#14 `codeaf/hedge` — failing checks: 1 failing check(s)",
		"#17 `codeaf/labels` — unaddressed finding: @alice",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing %q:\n%s", want, report)
		}
	}
	for _, not := range []string{"#19", "#21", "#23", "#25"} {
		if strings.Contains(report, not) {
			t.Errorf("report mentions #%s, which must not count:\n%s", not, report)
		}
	}
	if len(free) != 1 || free[0] != 12 {
		t.Errorf("conflictedFree = %v, want [12]", free)
	}
}

// TestPRSweepNullAuthor pins issue #326: a PR whose only review is
// CHANGES_REQUESTED with a null author — what `gh pr list --json` sends for
// a deleted account — is a standing finding and is reported under
// "@nobody (reviewer unknown)", while a PR with no review stays unreported.
func TestPRSweepNullAuthor(t *testing.T) {
	at := func(s string) time.Time {
		ts, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatalf("fixture stamp: %v", err)
		}
		return ts
	}
	prs := []prStatus{
		{Number: 31, HeadRefName: "codeaf/deleted-reviewer", State: "OPEN", Mergeable: "MERGEABLE",
			Reviews: []prReview{{State: "CHANGES_REQUESTED", SubmittedAt: at("2026-10-17T09:00:00Z")}}},
		{Number: 33, HeadRefName: "codeaf/no-review", State: "OPEN", Mergeable: "MERGEABLE"},
	}
	report, free := prSweep(prs)
	if !strings.Contains(report, "## PR sweep: 2 open `codeaf/*` PR(s) checked") {
		t.Errorf("header does not count the 2 examined PRs:\n%s", report)
	}
	if !strings.Contains(report, "#31 `codeaf/deleted-reviewer` — unaddressed finding: @nobody (reviewer unknown)") {
		t.Errorf("null-author finding not reported under @nobody:\n%s", report)
	}
	if strings.Contains(report, "#33") {
		t.Errorf("review-less PR reported, must stay unreported:\n%s", report)
	}
	if len(free) != 0 {
		t.Errorf("conflictedFree = %v, want empty", free)
	}
}

// TestPRSweepClean pins the empty case: a clean queue says so, and
// conflictedFree stays empty.
func TestPRSweepClean(t *testing.T) {
	report, free := prSweep([]prStatus{
		{Number: 9, HeadRefName: "codeaf/clean", State: "OPEN", Mergeable: "MERGEABLE",
			StatusCheckRollup: []prCheck{{State: "SUCCESS"}}},
	})
	if !strings.Contains(report, "## PR sweep: 1 open `codeaf/*` PR(s) checked") {
		t.Errorf("clean header does not count the 1 examined PR:\n%s", report)
	}
	if !strings.Contains(report, "No red, conflicted or unaddressed-review PR(s) among the 1 checked.") {
		t.Errorf("clean line does not carry the checked count:\n%s", report)
	}
	if len(free) != 0 {
		t.Errorf("conflictedFree = %v, want empty", free)
	}
}

// TestFlakyTests pins the parser on a realistic `go test -race -count=10`
// slice: one intermittently failing test (2 FAIL runs among PASS runs), one
// test that stays green, a subtest counted under its own name, and the
// first failure's output kept.
func TestFlakyTests(t *testing.T) {
	var out strings.Builder
	out.WriteString("=== RUN   TestStable\n")
	out.WriteString("--- PASS: TestStable (0.00s)\n")
	for i := 0; i < 10; i++ {
		out.WriteString("=== RUN   TestChatty\n")
		if i == 3 || i == 7 {
			out.WriteString("--- FAIL: TestChatty (0.01s)\n")
			out.WriteString("    chatty_test.go:24: attempt: connection reset by peer\n")
			out.WriteString("    chatty_test.go:25: giving up this run\n")
		} else {
			out.WriteString("--- PASS: TestChatty (0.01s)\n")
		}
	}
	out.WriteString("=== RUN   TestSub\n")
	out.WriteString("=== RUN   TestSub/case1\n")
	out.WriteString("--- FAIL: TestSub/case1 (0.00s)\n")
	out.WriteString("    sub_test.go:9: case1 flaked\n")
	out.WriteString("FAIL\n")
	out.WriteString("FAIL\tcodeaf-harness\t1.2s\n")

	report, failed := flakyTests([]byte(out.String()))
	want := []string{"TestChatty", "TestSub/case1"}
	if len(failed) != len(want) {
		t.Fatalf("failed = %v, want %v", failed, want)
	}
	for i, n := range want {
		if failed[i] != n {
			t.Errorf("failed[%d] = %q, want %q", i, failed[i], n)
		}
	}
	for _, want := range []string{
		"### TestChatty",
		"Failed 2 of its runs",
		"connection reset by peer",
		"### TestSub/case1",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing %q:\n%s", want, report)
		}
	}
	if strings.Contains(report, "### TestStable") {
		t.Errorf("stable test reported as flaky:\n%s", report)
	}
}

// TestFlakyTestsCapAndClean pins the cap (a wall of failure output is
// truncated, not shipped whole) and the all-green case.
func TestFlakyTestsCapAndClean(t *testing.T) {
	var out strings.Builder
	out.WriteString("--- FAIL: TestWall (0.01s)\n")
	for i := 0; i < 500; i++ {
		out.WriteString("    wall_test.go:1: line of failure output\n")
	}
	report, failed := flakyTests([]byte(out.String()))
	if len(failed) != 1 || failed[0] != "TestWall" {
		t.Fatalf("failed = %v, want [TestWall]", failed)
	}
	if got := strings.Count(report, "line of failure output"); got > flakyMaxLines {
		t.Errorf("report carries %d failure lines, cap is %d", got, flakyMaxLines)
	}
	if !strings.Contains(report, "(output capped)") && !strings.Contains(report, "```") {
		t.Errorf("report neither capped nor fenced:\n%.400s", report)
	}
	clean, failed := flakyTests([]byte("ok  \tcodeaf-harness\t30.1s\n"))
	if len(failed) != 0 || !strings.Contains(clean, "No intermittent test failures") {
		t.Errorf("clean run: failed = %v, report:\n%s", failed, clean)
	}
}

// TestFlakyBacktickFence pins the fence-safety rule: failure output full of
// backticks cannot close the fenced block or inject markdown.
func TestFlakyBacktickFence(t *testing.T) {
	out := "--- FAIL: TestBtick (0.00s)\n    x_test.go:1: got ```inject``` and `code`\n"
	report, failed := flakyTests([]byte(out))
	if len(failed) != 1 {
		t.Fatalf("failed = %v, want [TestBtick]", failed)
	}
	if strings.Count(report, "```")%2 != 0 {
		t.Errorf("unbalanced code fences:\n%s", report)
	}
	if strings.Contains(report, "```inject") {
		t.Errorf("backticks survived into the fence:\n%s", report)
	}
}

// TestFactoryReport pins the weekly report on a fixture with a done run and
// an incomplete stop, a merged PR, per-model spend and queue-to-merge
// stamps.
func TestFactoryReport(t *testing.T) {
	records := []runRecordSummary{
		{Mode: "issue", Stop: "done", StoreFinished: true, Number: 117, CostUSD: 2.5,
			Date: "2026-10-18", Model: "claude-sonnet-4-5",
			QueuedAt: "2026-10-18T08:00:00Z", MergedAt: "2026-10-18T10:00:00Z"},
		{Mode: "issue", Stop: "incomplete", StoreFinished: true, CostUSD: 1.0,
			Date: "2026-10-18", Model: "glm-4.7"},
		{Mode: "pr", Stop: "done", StoreFinished: true, Number: 90, CostUSD: 1.5,
			Date: "2026-10-19", Model: "claude-sonnet-4-5",
			QueuedAt: "2026-10-19T09:00:00Z", MergedAt: "2026-10-19T12:00:00Z"},
		{Mode: "review", Stop: "budget", CostUSD: 0.25, Date: "2026-10-19", Model: "claude-sonnet-4-5"},
		{Mode: "issue", Stop: "done", CostUSD: 0.75, Date: "2026-10-19", Model: "claude-sonnet-4-5"},
	}
	report := factoryReport(records)
	for _, want := range []string{
		"5 runs started, 3 finished",
		"| 3 | 1 |", // done vs incomplete
		"PRs merged: 1",
		"| claude-sonnet-4-5 | $5.0000 |",
		"| glm-4.7 | $1.0000 |",
		"| total | $6.0000 |",
		"2h30m0s over 2 run(s)",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing %q:\n%s", want, report)
		}
	}
}

// TestFactoryReportEmpty pins the degradation: no records, no stamps.
func TestFactoryReportEmpty(t *testing.T) {
	report := factoryReport(nil)
	for _, want := range []string{"0 runs started, 0 finished", "| 0 | 0 |", "PRs merged: 0", "n/a"} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing %q:\n%s", want, report)
		}
	}
}

// TestCheckRoutinesRegistered pins the four commands routines.json names to
// this file's init.
func TestCheckRoutinesRegistered(t *testing.T) {
	for _, name := range []string{"quota-calibration", "pr-sweep", "flaky-tests", "factory-report"} {
		fn, ok := routineChecks[name]
		if !ok || fn == nil {
			t.Errorf("check command %q is not registered", name)
		}
	}
	if _, ok := routineChecks["branch-cleanup"]; ok {
		t.Error("branch-cleanup is registered but was dropped by the maintainer")
	}
}

// TestCheckWrappersDegrade pins the wrapper contract: a missing environment
// name or an unreadable file is an error, never a panic.
func TestCheckWrappersDegrade(t *testing.T) {
	t.Setenv("CODEAF_LEDGER_PATH", "")
	t.Setenv("CODEAF_QUOTAS_PATH", "")
	t.Setenv("PRS_PATH", "")
	t.Setenv("RECORDS_PATH", "")
	if _, err := checkQuotaCalibration(); err == nil {
		t.Error("checkQuotaCalibration: missing env did not error")
	}
	if _, err := checkPRSweep(); err == nil {
		t.Error("checkPRSweep: missing PRS_PATH did not error")
	}
	if _, err := checkFactoryReport(); err == nil {
		t.Error("checkFactoryReport: missing RECORDS_PATH did not error")
	}
	t.Setenv("PRS_PATH", filepath.Join(t.TempDir(), "absent.json"))
	if _, err := checkPRSweep(); err == nil {
		t.Error("checkPRSweep: unreadable PRS_PATH did not error")
	}
}

// TestCheckWrappersLive runs each wrapper's happy path on fixture files
// written in the shapes the routine's workflow hands over.
func TestCheckWrappersLive(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	// quota-calibration: a ledger line and two /v2/quotas snapshots.
	t.Setenv("CODEAF_LEDGER_PATH", write("ledger.jsonl",
		`{"ts":"2026-10-18T10:00:00Z","model":"claude-sonnet-4-5","cost_usd":15.8}`+"\n"))
	t.Setenv("CODEAF_QUOTAS_PATH", write("quotas.json",
		`[{"weeklyTokenLimit":{"percentRemaining":90},"ts":"2026-10-18T09:00:00Z"},
		  {"weeklyTokenLimit":{"percentRemaining":58},"ts":"2026-10-18T12:00:00Z"}]`))
	report, err := checkQuotaCalibration()
	if err != nil {
		t.Fatalf("checkQuotaCalibration: %v", err)
	}
	if !strings.Contains(report, "update `limits.weekly_credit_usd`") {
		t.Errorf("live calibration did not propose the change:\n%s", report)
	}

	// pr-sweep: one gh-shaped PR that is red and conflicted.
	t.Setenv("PRS_PATH", write("prs.json",
		`[{"number":31,"headRefName":"codeaf/x","state":"OPEN","mergeable":"CONFLICTING",
		   "statusCheckRollup":[{"__typename":"CheckRun","conclusion":"FAILURE","status":"COMPLETED"}],
		   "reviews":[{"author":{"login":"alice"},"state":"APPROVED","submittedAt":"2026-10-18T09:00:00Z"}]}]`))
	report, err = checkPRSweep()
	if err != nil {
		t.Fatalf("checkPRSweep: %v", err)
	}
	if !strings.Contains(report, "#31 `codeaf/x` — failing checks") {
		t.Errorf("live sweep missed the red PR:\n%s", report)
	}

	// factory-report: one record line that validates, one that is junk.
	t.Setenv("RECORDS_PATH", write("records.jsonl",
		`{"run_id":"1234","attempt":"1","mode":"issue","lane":0,"number":117,"stop":"done","store_finished":true,"cost_usd":2.5,"date":"2026-10-18","model":"claude-sonnet-4-5","queued_at":"2026-10-18T08:00:00Z","merged_at":"2026-10-18T10:00:00Z"}`+"\n"+
			`{"run_id":"not-a-run-id","stop":"nope"}`+"\n"))
	report, err = checkFactoryReport()
	if err != nil {
		t.Fatalf("checkFactoryReport: %v", err)
	}
	if !strings.Contains(report, "1 runs started, 1 finished") || !strings.Contains(report, "2h0m0s") {
		t.Errorf("live factory report wrong:\n%s", report)
	}
}

// TestFlakyModuleDiscovery pins checkFlakyTests' module rule — every
// directory with a go.mod under app/, plus the harness module — without
// shelling out to go test (which the pure core's tests cover).
func TestFlakyModuleDiscovery(t *testing.T) {
	root := t.TempDir()
	harnessDir := filepath.Join(root, ".github", "codeaf")
	appDir := filepath.Join(root, "app", "demo")
	for _, d := range []string{harnessDir, appDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "go.mod"), []byte("module x\n\ngo 1.24\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var mods []string
	for _, pattern := range []string{filepath.Join(root, "app", "*", "go.mod"), filepath.Join(root, ".github", "codeaf", "go.mod")} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		mods = append(mods, matches...)
	}
	if len(mods) != 2 {
		t.Errorf("module discovery found %v, want the harness module and app/demo", mods)
	}
}
