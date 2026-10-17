package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeReviewResult points RESULT_PATH at a result file with the given stop
// reason and answer, the two fields the review report reads.
func writeReviewResult(t *testing.T, stop, answer string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "result.json")
	data, err := json.Marshal(map[string]any{"stop": stop, "model": "zai-org/glm-5.3-flash", "answer": answer})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RESULT_PATH", path)
}

// reviewLedger points LEDGER_PATH at a two-call ledger worth 0.25 USD; the
// proxy_start marker line must not count toward the total.
func reviewLedger(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	lines := `{"kind":"proxy_start"}
{"model":"zai-org/glm-5.3-flash","cost_usd":0.25}
{"model":"zai-org/glm-5.3-flash","cost_usd":0}
`
	if err := os.WriteFile(path, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEDGER_PATH", path)
}

// reviewServer serves the fake API for the review tests: routes maps a path
// to a body, and diffBody is served on the pull request route when the
// client asks for a diff. The Accept header of the first pull request
// request is recorded in sawAccept when it is given.
func reviewServer(t *testing.T, routes map[string]string, diffBody string, sawAccept *string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/RichardoC/factory-repo/pulls/7" {
			if sawAccept != nil {
				*sawAccept = r.Header.Get("Accept")
			}
			if diffBody != "" && strings.Contains(r.Header.Get("Accept"), "diff") {
				_, _ = io.WriteString(w, diffBody)
				return
			}
		}
		body, ok := routes[r.URL.Path]
		if !ok {
			http.Error(w, "not served: "+r.URL.Path, http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return server
}

// reviewFindingsAnswer is an answer carrying the whole contract: all three
// severities, an unknown severity word, every separator, a nested bullet,
// hostile text, and a bullet before and after the findings section.
const reviewFindingsAnswer = "Some prose first.\n\n" +
	"- [high] before.go:1 - a bullet before the section must not parse\n\n" +
	"## Summary\n\n- [high] also-before.go:2 - still before the section\n\n" +
	"### findings\n\n" +
	"- [high] app/x/main.go:42 - the mutex is released twice on the error path\n" +
	"* [Medium] app/y/queue.go:7 — the queue never drains when a send fails\n" +
	"- [low] docs/readme.md:3 : typo in the flag name\n" +
	"- [CRITICAL] app/z/z.go:1 – the universe ends\n" +
	"- [high] app/w/lock.go:5 -- | pipes | and <!-- html comments --> and `backticks`\n" +
	"  - [high] app/w/nested.go:9 - an indented bullet still parses\n\n" +
	"## Notes\n\n" +
	"- [high] after.go:1 - a bullet after the next header must not parse\n"

func TestParseFindings(t *testing.T) {
	findings, sawSection := parseFindings(reviewFindingsAnswer)
	if !sawSection {
		t.Fatal("sawSection = false, want true")
	}
	if len(findings) != 6 {
		t.Fatalf("got %d findings, want 6: %+v", len(findings), findings)
	}
	want := []struct {
		severity   string
		path       string
		line       int
		text       string
		recognized bool
	}{
		{"high", "app/x/main.go", 42, "the mutex is released twice on the error path", true},
		{"Medium", "app/y/queue.go", 7, "the queue never drains when a send fails", true},
		{"low", "docs/readme.md", 3, "typo in the flag name", true},
		{"CRITICAL", "app/z/z.go", 1, "the universe ends", false},
		{"high", "app/w/lock.go", 5, "| pipes | and <!-- html comments --> and `backticks`", true},
		{"high", "app/w/nested.go", 9, "an indented bullet still parses", true},
	}
	for i, w := range want {
		f := findings[i]
		if f.Severity != w.severity || f.Path != w.path || f.Line != w.line || f.Text != w.text || f.Recognized != w.recognized {
			t.Errorf("findings[%d] = %+v, want severity %q path %q line %d text %q recognized %t",
				i, f, w.severity, w.path, w.line, w.text, w.recognized)
		}
	}
	// Bullets outside the section never parsed, and hostile text is kept
	// verbatim, never truncated or rewritten.
	for _, banned := range []string{"before.go", "after.go", "also-before.go"} {
		for _, f := range findings {
			if strings.Contains(f.Path, banned) {
				t.Errorf("finding %q came from outside the findings section", f.Path)
			}
		}
	}

	// CRLF endings parse the same as LF.
	findings, sawSection = parseFindings(strings.ReplaceAll(reviewFindingsAnswer, "\n", "\r\n"))
	if !sawSection || len(findings) != 6 || findings[0].Text != want[0].text {
		t.Errorf("CRLF parse = %d findings, sawSection %t, first text %q", len(findings), sawSection, findings[0].Text)
	}

	// No findings section at all.
	if findings, sawSection := parseFindings("Just an answer, no section, no header."); findings != nil || sawSection {
		t.Errorf("missing section = (%v, %t), want (nil, false)", findings, sawSection)
	}

	// A header-like word that is not the section: "Findings" inside a
	// sentence does not open it, and does not count as sawSection.
	if findings, sawSection := parseFindings("## Notes on findings so far\n\n- [high] a.go:1 - x"); findings != nil || sawSection {
		t.Errorf("prose header = (%v, %t), want (nil, false)", findings, sawSection)
	}

	// Malformed bullets are ignored: none of these is a finding.
	malformed := []string{
		"- high app/x.go:42 - no brackets",
		"- [high app/x.go:42 - never closed",
		"- [] app/x.go:42 - empty severity",
		"- [high]app/x.go:42 - no space after the severity",
		"- [high] app/x.go - no line number",
		"- [high] app/x.go:42",
		"- [high] app/x.go:42 no separator at all",
		"- [high] :42 - no path",
		"- [high] app/x.go:abc - line is not digits",
		"plain prose in the section",
		"- [high] app/x.go:42 - ",
	}
	var b strings.Builder
	b.WriteString("## Findings\n\n")
	for _, m := range malformed {
		b.WriteString(m + "\n")
	}
	if findings, _ := parseFindings(b.String()); len(findings) != 0 {
		t.Errorf("malformed bullets produced %d findings: %+v", len(findings), findings)
	}

	// The cap: 60 valid bullets yield exactly maxReviewFindings.
	b.Reset()
	b.WriteString("## Findings\n\n")
	for i := 1; i <= 60; i++ {
		fmt.Fprintf(&b, "- [low] app/cap.go:%d - finding %d\n", i, i)
	}
	findings, _ = parseFindings(b.String())
	if len(findings) != maxReviewFindings {
		t.Fatalf("got %d findings, want the cap %d", len(findings), maxReviewFindings)
	}
	if findings[maxReviewFindings-1].Line != maxReviewFindings {
		t.Errorf("cap kept the wrong findings: last line = %d", findings[maxReviewFindings-1].Line)
	}
}

// reviewHostileFindings is a finding set a hostile reviewer could have
// written: markdown, mentions and shell in the cells, and a path that must
// never reach the fix command line.
func reviewHostileFindings() []Finding {
	return []Finding{
		{Severity: "high", Path: "app/x/main.go", Line: 42, Text: "| pipes | <!-- html --> and @alice and [link](https://x)", Recognized: true},
		{Severity: "medium", Path: "a$(reboot).go", Line: 7, Text: "missing error check", Recognized: true},
	}
}

func TestReviewPlan(t *testing.T) {
	hostile := reviewHostileFindings()
	// No result at all: report-only, nothing concluded, no label, no fix.
	server := commentsServer(t, reportComments(1), http.StatusOK)
	ghEnv(t, server)
	t.Setenv("LEDGER_PATH", "")
	action, body, used, err := reviewPlan(newGitHub(), 7, Result{}, false, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if action != "report-only" || used != 0 {
		t.Fatalf("no result: action = %q, used = %d", action, used)
	}
	if !strings.Contains(body, "produced no result") || strings.Contains(body, reviewPassedLabel) || strings.HasPrefix(body, command+" fix:") {
		t.Errorf("no-result body wrong: %q", body)
	}
	assertReviewBodyClean(t, body)

	// The whole decision table over a finished and an unfinished review.
	reports := map[int]int{1: 0, 2: 1} // comment count -> continuations used
	for n, wantUsed := range reports {
		server := commentsServer(t, reportComments(n), http.StatusOK)
		ghEnv(t, server)
		action, body, used, err := reviewPlan(newGitHub(), 7, Result{Stop: stopDone, Model: "deepseek-ai/deepseek-v4.1-flash"},
			true, hostile, true)
		if err != nil {
			t.Fatal(err)
		}
		if action != "fix" || used != wantUsed {
			t.Fatalf("reports=%d: action = %q, used = %d, want fix/%d", n, action, used, wantUsed)
		}
		if !strings.HasPrefix(body, "/codeaf fix: 2 blocking findings - high app/x/main.go:42; medium (unsafe path):7\n") {
			t.Errorf("fix body does not start with the command (reports=%d): %q", n, body)
		}
		if !strings.Contains(body, fmt.Sprintf("continuation %d of %d", wantUsed+1, maxContinuations)) {
			t.Errorf("fix body does not name the continuation (reports=%d): %q", n, body)
		}
		if !strings.Contains(body, "| severity | location | finding |") {
			t.Errorf("fix body carries no findings table: %q", body)
		}
		assertReviewBodyClean(t, body)
	}
	// The hostile path never reaches the fix command line unfiltered.
	if strings.Contains(body, "$(reboot)") {
		t.Errorf("the fix command line carries a hostile path: %q", body)
	}

	// Cap reached: report-only with the cap message, no command line.
	server = commentsServer(t, reportComments(3), http.StatusOK)
	ghEnv(t, server)
	action, body, used, err = reviewPlan(newGitHub(), 7, Result{Stop: stopDone}, true, hostile, true)
	if err != nil {
		t.Fatal(err)
	}
	if action != "report-only" || used != 2 {
		t.Fatalf("cap: action = %q, used = %d", action, used)
	}
	if strings.HasPrefix(body, command+" fix:") {
		t.Errorf("the cap body starts a fix run: %q", body)
	}
	for _, want := range []string{"automatic fix budget of 2", "/codeaf comment", "| severity | location | finding |"} {
		if !strings.Contains(body, want) {
			t.Errorf("cap body missing %q: %q", want, body)
		}
	}
	assertReviewBodyClean(t, body)

	// A finished review with an unrecognized severity: report-only, the raw
	// word shown, label withheld.
	server = commentsServer(t, reportComments(1), http.StatusOK)
	ghEnv(t, server)
	mixed := []Finding{{Severity: "critical", Path: "app/z/z.go", Line: 1, Text: "scale unknown", Recognized: false}}
	action, body, _, err = reviewPlan(newGitHub(), 7, Result{Stop: stopDone}, true, mixed, true)
	if err != nil {
		t.Fatal(err)
	}
	if action != "report-only" {
		t.Fatalf("unrecognized: action = %q", action)
	}
	if !strings.Contains(body, "withheld") || !strings.Contains(body, "`critical`") {
		t.Errorf("unrecognized body missing the note or the raw word: %q", body)
	}
	if strings.Contains(body, reviewPassedLabel+" label was added") {
		t.Errorf("unrecognized body added the label: %q", body)
	}
	assertReviewBodyClean(t, body)

	// A clean pass: the label, the table only when low findings exist.
	server = commentsServer(t, reportComments(1), http.StatusOK)
	ghEnv(t, server)
	action, body, _, err = reviewPlan(newGitHub(), 7, Result{Stop: stopDone}, true, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if action != "label" || !strings.Contains(body, reviewPassedLabel) {
		t.Fatalf("clean pass: action = %q, body = %q", action, body)
	}
	if strings.Contains(body, "| severity | location | finding |") {
		t.Errorf("a clean pass must carry no table: %q", body)
	}
	low := []Finding{{Severity: "low", Path: "docs/readme.md", Line: 3, Text: "typo", Recognized: true}}
	_, body, _, err = reviewPlan(newGitHub(), 7, Result{Stop: stopDone}, true, low, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, reviewPassedLabel) || !strings.Contains(body, "| severity | location | finding |") {
		t.Errorf("pass with low findings: table or label missing: %q", body)
	}

	// An unfinished review: report-only, the stop reason named, the table
	// marked incomplete, no label and no fix.
	server = commentsServer(t, reportComments(1), http.StatusOK)
	ghEnv(t, server)
	action, body, _, err = reviewPlan(newGitHub(), 7, Result{Stop: stopDeadline}, true, hostile, true)
	if err != nil {
		t.Fatal(err)
	}
	if action != "report-only" {
		t.Fatalf("early stop: action = %q", action)
	}
	for _, want := range []string{"`deadline`", "incomplete review", "| severity | location | finding |"} {
		if !strings.Contains(body, want) {
			t.Errorf("early-stop body missing %q: %q", want, body)
		}
	}
	if strings.Contains(body, reviewPassedLabel) {
		t.Errorf("an incomplete review must not name the pass label: %q", body)
	}
	assertReviewBodyClean(t, body)

	// A failing comments fetch is an error, like continuePlan.
	server = commentsServer(t, nil, http.StatusInternalServerError)
	ghEnv(t, server)
	if _, _, _, err := reviewPlan(newGitHub(), 7, Result{Stop: stopDone}, true, hostile, true); err == nil {
		t.Error("a failing comment fetch must return an error")
	}
}

// assertReviewBodyClean checks the invariants every review comment must
// hold: no report marker, and no unescaped hostile text.
func assertReviewBodyClean(t *testing.T, body string) {
	t.Helper()
	if strings.Contains(body, reportMarker) {
		t.Errorf("the comment carries the run-report marker: %q", body)
	}
	if strings.Contains(body, "<!--") || strings.Contains(body, "| pipes |") || strings.Contains(body, "@alice") || strings.Contains(body, "[link](") {
		t.Errorf("unescaped hostile text leaked into the comment: %q", body)
	}
}

func TestReviewFooter(t *testing.T) {
	reviewLedger(t)
	t.Setenv("GITHUB_SERVER_URL", "https://github.com")
	ghEnv(t, commentsServer(t, nil, http.StatusOK)) // the artifacts call finds nothing here
	t.Setenv("GITHUB_RUN_ID", "55")
	body := reviewFooter("deepseek-ai/deepseek-v4.1-flash", stopDone)
	for _, want := range []string{
		"`deepseek-ai/deepseek-v4.1-flash`",
		"`done`",
		"metered spend this run: $0.2500",
		"[Workflow run](https://github.com/RichardoC/factory-repo/actions/runs/55)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("footer missing %q: %q", want, body)
		}
	}
	// A missing ledger reads as zero; an unreadable one as unknown, not as
	// zero; and the stop word is sanitized however it arrives.
	t.Setenv("LEDGER_PATH", "")
	if body := reviewFooter("", ""); !strings.Contains(body, "$0.0000") || !strings.Contains(body, "`(unknown)`") {
		t.Errorf("footer with no ledger and no model: %q", body)
	}
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	if err := os.WriteFile(path, []byte("not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEDGER_PATH", path)
	if body := reviewFooter("", ""); !strings.Contains(body, "metered spend this run: (unknown)") || strings.Contains(body, "$0.0000") {
		t.Errorf("footer with an unreadable ledger: %q", body)
	}
}

func TestReviewerModel(t *testing.T) {
	cfg, err := loadConfig("models.json")
	if err != nil {
		t.Fatal(err)
	}
	// The worker is the base model; the mid model takes the review.
	got, err := reviewerModel(cfg, "zai-org/glm-5.3-flash")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "deepseek-ai/deepseek-v4.1-flash" || got.Role != "mid" {
		t.Errorf("reviewer = %q (role %q), want the mid model", got.ID, got.Role)
	}
	// Whatever the worker is, the reviewer never is.
	for _, m := range cfg.Models {
		got, err := reviewerModel(cfg, m.ID)
		if err != nil {
			t.Fatalf("worker %q: %v", m.ID, err)
		}
		if got.ID == m.ID {
			t.Errorf("reviewer = worker model %q", got.ID)
		}
		if !containsModel(cfg.Models, got.ID) {
			t.Errorf("reviewer %q is not an allowed model", got.ID)
		}
	}
	// A single-model config cannot host a review: it fails loudly.
	single := Config{Models: []Model{{ID: "only/model", Role: "mid", Upstream: "u", Efforts: []string{"high"}}}}
	if _, err := reviewerModel(single, "only/model"); err == nil {
		t.Error("a single-model config must fail, not review itself")
	}
}

func containsModel(models []Model, id string) bool {
	for _, m := range models {
		if m.ID == id {
			return true
		}
	}
	return false
}

func TestRunReviewConfig(t *testing.T) {
	dir := t.TempDir()
	config := `{"api_key":"via-local-proxy","models.tiers.worker":"zai-org/glm-5.3-flash","task.parallel":2,"telemetry":false,"daily_budget_usd":40}`
	if err := os.WriteFile(filepath.Join(dir, "codeaf-config.json"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARNESS_DIR", dir)
	t.Setenv("PROXY_MODELS", "models.json") // the real catalog, next to this file

	out, err := captureStdout(t, runReviewConfig)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("output does not end with a newline: %q", out)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if got["models.tiers.worker"] != "deepseek-ai/deepseek-v4.1-flash" {
		t.Errorf("worker tier = %v, want the reviewer model", got["models.tiers.worker"])
	}
	cfg, err := loadConfig("models.json")
	if err != nil {
		t.Fatal(err)
	}
	if !containsModel(cfg.Models, fmt.Sprint(got["models.tiers.worker"])) {
		t.Errorf("worker tier %v is not an allowed model", got["models.tiers.worker"])
	}
	// Every other key is unchanged, in value and in kind.
	if got["task.parallel"] != float64(2) || got["telemetry"] != false || got["daily_budget_usd"] != float64(40) || got["api_key"] != "via-local-proxy" {
		t.Errorf("other keys changed: %v", got)
	}

	// Failures are loud: unreadable config, and no model left to review on.
	t.Setenv("HARNESS_DIR", filepath.Join(dir, "missing"))
	if _, err := captureStdout(t, runReviewConfig); err == nil {
		t.Error("a missing codeaf-config.json must fail")
	}
	single := t.TempDir()
	worker := `{"api_key":"via-local-proxy","models.tiers.worker":"only/model"}`
	if err := os.WriteFile(filepath.Join(single, "codeaf-config.json"), []byte(worker), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(single, "single-models.json"), []byte(`{"limits":{"run_usd":5,"day_usd":40},"models":[{"id":"only/model","role":"mid","upstream":"u","efforts":["high"]}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARNESS_DIR", single)
	t.Setenv("PROXY_MODELS", filepath.Join(single, "single-models.json"))
	if _, err := captureStdout(t, runReviewConfig); err == nil {
		t.Error("a single-model config must fail, not review itself")
	}
}

func TestRunReviewBrief(t *testing.T) {
	routes := map[string]string{
		"/repos/RichardoC/factory-repo/pulls/7":           `{"number":7,"title":"Review target","body":"The change.\n\nCloses #3","state":"open","head":{"ref":"codeaf/issue-3-42"},"base":{"ref":"main"}}`,
		"/repos/RichardoC/factory-repo/issues/3":          `{"number":3,"title":"The task","body":"Build the thing the review judges against."}`,
		"/repos/RichardoC/factory-repo/issues/7/comments": `[]`,
	}
	var sawAccept string
	diffBody := "diff --git a/app/x/main.go b/app/x/main.go\n--- a/app/x/main.go\n+++ b/app/x/main.go\n@@ -1,2 +1,3 @@\n context\n-removed\n+added\n"
	server := reviewServer(t, routes, diffBody, &sawAccept)
	ghEnv(t, server)
	t.Setenv("NUMBER", "7")
	briefPath := filepath.Join(t.TempDir(), "nested", "review-brief.md")
	t.Setenv("BRIEF_PATH", briefPath)

	out, err := captureStdout(t, runReviewBrief)
	if err != nil {
		t.Fatal(err)
	}
	brief, err := os.ReadFile(briefPath)
	if err != nil {
		t.Fatalf("brief not written: %v", err)
	}
	if string(brief) != out {
		t.Errorf("stdout and BRIEF_PATH differ")
	}
	b := string(brief)
	if sawAccept != "application/vnd.github.v3.diff" {
		t.Errorf("diff fetch sent Accept %q", sawAccept)
	}
	for _, want := range []string{
		"You have ",            // the budget line opens the brief
		"independent reviewer", // the standing instructions
		"# The pull request under review: #7 — Review target", // number and title
		"Head branch `codeaf/issue-3-42`, base branch `main`", // the branches
		"## The issue this PR closes (#3) — The task",         // the linked issue
		"Build the thing the review judges against.",          // the issue's task text
		"# The diff under review",                             // the diff section
		"@@ -1,2 +1,3 @@",                                     // the diff body, hunk and all
		"## The previous CodeAF report",                       // the previous report section
		"# The output the harness parses",                     // the output contract
		"app/zip/queue.go:42",                                 // the concrete example bullet
	} {
		if !strings.Contains(b, want) {
			t.Errorf("brief missing %q", want)
		}
	}

	// A failing issue fetch degrades to a note and never fails the brief.
	delete(routes, "/repos/RichardoC/factory-repo/issues/3")
	briefPath = filepath.Join(t.TempDir(), "degraded.md")
	t.Setenv("BRIEF_PATH", briefPath)
	if _, err := captureStdout(t, runReviewBrief); err != nil {
		t.Fatalf("a failed issue fetch must not fail the brief: %v", err)
	}
	data, err := os.ReadFile(briefPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "is unavailable") {
		t.Errorf("the degrade note is missing: %q", data)
	}

	// A huge diff is capped, with a note.
	big := strings.Repeat("+ line of a hostile diff\n", 4000) // ~96KB
	routes["/repos/RichardoC/factory-repo/issues/3"] = `{"number":3,"title":"The task","body":"x"}`
	ghEnv(t, reviewServer(t, routes, big, nil))
	briefPath = filepath.Join(t.TempDir(), "capped.md")
	t.Setenv("BRIEF_PATH", briefPath)
	if _, err := captureStdout(t, runReviewBrief); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(briefPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), fmt.Sprintf("(diff truncated at %d bytes)", maxReviewDiffBytes)) {
		t.Errorf("the diff cap note is missing")
	}

	// Required inputs are refused loudly.
	t.Setenv("BRIEF_PATH", "")
	if _, err := captureStdout(t, runReviewBrief); err == nil {
		t.Error("an empty BRIEF_PATH must fail")
	}
	t.Setenv("NUMBER", "seven")
	t.Setenv("BRIEF_PATH", filepath.Join(t.TempDir(), "x.md"))
	if _, err := captureStdout(t, runReviewBrief); err == nil {
		t.Error("a non-numeric NUMBER must fail")
	}
}

func TestRunReviewReport(t *testing.T) {
	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs })
	setArgs := func(pr string) { os.Args = []string{"harness", "review-report", pr} }

	findings := "- [high] app/x/main.go:42 - the mutex is released twice\n- [medium] app/y/x.go:7 - missing error check\n"
	comment := filepath.Join(t.TempDir(), "nested", "review-comment.md")

	t.Run("a blocking review starts a fix run", func(t *testing.T) {
		server := commentsServer(t, reportComments(1), http.StatusOK)
		ghEnv(t, server)
		setArgs("7")
		writeReviewResult(t, stopDone, "## Findings\n\n"+findings)
		reviewLedger(t)
		t.Setenv("REVIEW_COMMENT_PATH", comment)
		out, err := captureStdout(t, runReviewReport)
		if err != nil {
			t.Fatal(err)
		}
		if out != "action=fix\nused=0\nblocking=2\n" {
			t.Errorf("stdout = %q", out)
		}
		data, err := os.ReadFile(comment)
		if err != nil {
			t.Fatalf("comment not written: %v", err)
		}
		if !strings.HasPrefix(string(data), "/codeaf fix: 2 blocking findings - high app/x/main.go:42; medium app/y/x.go:7\n") {
			t.Errorf("comment = %q", data)
		}
		assertReviewBodyClean(t, string(data))
	})

	t.Run("the cap turns the fix into a report", func(t *testing.T) {
		server := commentsServer(t, reportComments(3), http.StatusOK)
		ghEnv(t, server)
		setArgs("7")
		writeReviewResult(t, stopDone, "## Findings\n\n"+findings)
		reviewLedger(t)
		t.Setenv("REVIEW_COMMENT_PATH", comment)
		out, err := captureStdout(t, runReviewReport)
		if err != nil {
			t.Fatal(err)
		}
		if out != "action=report-only\nused=2\nblocking=2\n" {
			t.Errorf("stdout = %q", out)
		}
		data, err := os.ReadFile(comment)
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(string(data), command+" fix:") {
			t.Errorf("the cap comment starts a fix run: %q", data)
		}
	})

	t.Run("an unrecognized severity withholds the label", func(t *testing.T) {
		server := commentsServer(t, reportComments(1), http.StatusOK)
		ghEnv(t, server)
		setArgs("7")
		writeReviewResult(t, stopDone, "## Findings\n\n- [critical] app/z/z.go:1 - scale unknown\n")
		reviewLedger(t)
		t.Setenv("REVIEW_COMMENT_PATH", comment)
		out, err := captureStdout(t, runReviewReport)
		if err != nil {
			t.Fatal(err)
		}
		if out != "action=report-only\nused=0\nblocking=0\n" {
			t.Errorf("stdout = %q", out)
		}
	})

	t.Run("a clean review labels the pull request", func(t *testing.T) {
		server := commentsServer(t, reportComments(1), http.StatusOK)
		ghEnv(t, server)
		setArgs("7")
		writeReviewResult(t, stopDone, "## Findings\n\n")
		reviewLedger(t)
		t.Setenv("REVIEW_COMMENT_PATH", comment)
		out, err := captureStdout(t, runReviewReport)
		if err != nil {
			t.Fatal(err)
		}
		if out != "action=label\nused=0\nblocking=0\n" {
			t.Errorf("stdout = %q", out)
		}
		data, err := os.ReadFile(comment)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), reviewPassedLabel) {
			t.Errorf("clean comment does not name the label: %q", data)
		}
	})

	t.Run("an early stop is report-only, findings counted", func(t *testing.T) {
		server := commentsServer(t, reportComments(1), http.StatusOK)
		ghEnv(t, server)
		setArgs("7")
		writeReviewResult(t, stopDeadline, "## Findings\n\n"+findings)
		reviewLedger(t)
		t.Setenv("REVIEW_COMMENT_PATH", comment)
		out, err := captureStdout(t, runReviewReport)
		if err != nil {
			t.Fatal(err)
		}
		if out != "action=report-only\nused=0\nblocking=2\n" {
			t.Errorf("stdout = %q", out)
		}
	})

	t.Run("a missing result is report-only, not an error", func(t *testing.T) {
		server := commentsServer(t, reportComments(1), http.StatusOK)
		ghEnv(t, server)
		setArgs("7")
		t.Setenv("RESULT_PATH", filepath.Join(t.TempDir(), "absent.json"))
		reviewLedger(t)
		t.Setenv("REVIEW_COMMENT_PATH", comment)
		out, err := captureStdout(t, runReviewReport)
		if err != nil {
			t.Fatal(err)
		}
		if out != "action=report-only\nused=0\nblocking=0\n" {
			t.Errorf("stdout = %q", out)
		}
	})

	t.Run("required inputs and the argument are checked", func(t *testing.T) {
		server := commentsServer(t, reportComments(1), http.StatusOK)
		ghEnv(t, server)
		setArgs("seven; rm -rf /")
		writeReviewResult(t, stopDone, "")
		t.Setenv("REVIEW_COMMENT_PATH", comment)
		reviewLedger(t)
		if _, err := captureStdout(t, runReviewReport); err == nil {
			t.Error("a non-numeric PR argument must fail")
		}
		setArgs("7")
		t.Setenv("RESULT_PATH", "")
		if _, err := captureStdout(t, runReviewReport); err == nil {
			t.Error("an empty RESULT_PATH must fail")
		}
		writeReviewResult(t, stopDone, "")
		t.Setenv("LEDGER_PATH", "")
		if _, err := captureStdout(t, runReviewReport); err == nil {
			t.Error("an empty LEDGER_PATH must fail")
		}
		reviewLedger(t)
		t.Setenv("REVIEW_COMMENT_PATH", "")
		if _, err := captureStdout(t, runReviewReport); err == nil {
			t.Error("an empty REVIEW_COMMENT_PATH must fail")
		}
	})
}

func TestDecideDispatchReview(t *testing.T) {
	t.Setenv("GITHUB_REPOSITORY", "RichardoC/factory-repo")
	t.Setenv("GITHUB_RUN_ID", "42")
	labelled := []label{{Name: "codeaf"}}
	mkPR := func(number int, state string, labels []label, headRepo string) item {
		pr := item{Number: number, State: state, Labels: labels}
		pr.Head.Ref = "codeaf/issue-3-42"
		pr.Head.Repo = &struct {
			FullName string `json:"full_name"`
		}{FullName: headRepo}
		pr.Base.Ref = "main"
		return pr
	}
	openPR := mkPR(7, "open", labelled, "RichardoC/factory-repo")

	cases := []struct {
		name   string
		review string
		pr     item
		run    bool
		reason string
	}{
		{"labelled open pr", "7", openPR, true, "review dispatch"},
		{"unlabelled pr", "7", mkPR(7, "open", nil, "RichardoC/factory-repo"), false, "PR is not labelled codeaf"},
		{"closed pr", "7", mkPR(7, "closed", labelled, "RichardoC/factory-repo"), false, "PR is not open"},
		{"fork head", "7", mkPR(7, "open", labelled, "evil/factory-repo"), false, "PR head is in a fork; CodeAF only pushes to branches in RichardoC/factory-repo"},
		{"non-numeric input", "seven", openPR, false, "workflow_dispatch needs a numeric issue or pr input"},
	}
	for _, c := range cases {
		t.Run("review "+c.name, func(t *testing.T) {
			lookupPR := func(n int) (item, error) {
				if n != 7 {
					t.Fatalf("lookupPR(%d), want 7", n)
				}
				return c.pr, nil
			}
			lookupIssue := func(int) (item, error) { t.Fatal("unexpected issue lookup"); return item{}, nil }
			ev := event{Inputs: map[string]string{"review": c.review}}
			got, err := Decide("workflow_dispatch", ev, "codeaf", lookupPR, lookupIssue)
			if err != nil {
				t.Fatal(err)
			}
			if got.Run != c.run || got.Reason != c.reason {
				t.Fatalf("Decide = %+v, want run %t with reason %q", got, c.run, c.reason)
			}
			if c.run && (got.Mode != "review" || got.Number != 7 || got.Branch != openPR.Head.Ref || got.Base != openPR.Base.Ref) {
				t.Fatalf("Decide = %+v, want mode review with the PR's number and branches", got)
			}
		})
	}

	t.Run("a failing PR lookup is an error", func(t *testing.T) {
		lookupPR := func(int) (item, error) { return item{}, fmt.Errorf("500") }
		ev := event{Inputs: map[string]string{"review": "7"}}
		if _, err := Decide("workflow_dispatch", ev, "codeaf", lookupPR, func(int) (item, error) { return item{}, nil }); err == nil {
			t.Fatal("a failing PR lookup must be returned as an error")
		}
	})

	t.Run("the pr input still wins over the review input", func(t *testing.T) {
		lookupPR := func(int) (item, error) { return openPR, nil }
		ev := event{Inputs: map[string]string{"pr": "7", "review": "7"}}
		got, err := Decide("workflow_dispatch", ev, "codeaf", lookupPR, func(int) (item, error) { return item{}, nil })
		if err != nil {
			t.Fatal(err)
		}
		if !got.Run || got.Mode != "pr" || got.Reason != "workflow dispatch" {
			t.Fatalf("Decide = %+v, want the continuation path", got)
		}
	})
}
