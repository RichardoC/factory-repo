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
// reason and answer, the two fields the review report reads, and returns the
// path so a test can put the run record beside it.
func writeReviewResult(t *testing.T, stop, answer string) string {
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
	return path
}

// writeTrajectory points TRAJECTORY_PATH at a run record holding the given
// lines, the fallback source the review report reads when the result's
// answer carries no findings.
func writeTrajectory(t *testing.T, lines string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "trajectory.jsonl")
	if err := os.WriteFile(path, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TRAJECTORY_PATH", path)
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
	// The section still holds its six bullets, and the bullets written
	// outside it now parse as inline findings (issue #49) — in answer order,
	// so the section findings sit between the ones before and after it.
	if len(findings) != 9 {
		t.Fatalf("got %d findings, want 9: %+v", len(findings), findings)
	}
	want := []struct {
		severity   string
		path       string
		line       int
		text       string
		recognized bool
	}{
		{"high", "before.go", 1, "a bullet before the section must not parse", true},
		{"high", "also-before.go", 2, "still before the section", true},
		{"high", "app/x/main.go", 42, "the mutex is released twice on the error path", true},
		{"Medium", "app/y/queue.go", 7, "the queue never drains when a send fails", true},
		{"low", "docs/readme.md", 3, "typo in the flag name", true},
		{"CRITICAL", "app/z/z.go", 1, "the universe ends", false},
		{"high", "app/w/lock.go", 5, "| pipes | and <!-- html comments --> and `backticks`", true},
		{"high", "app/w/nested.go", 9, "an indented bullet still parses", true},
		{"high", "after.go", 1, "a bullet after the next header must not parse", true},
	}
	for i, w := range want {
		f := findings[i]
		if f.Severity != w.severity || f.Path != w.path || f.Line != w.line || f.Text != w.text || f.Recognized != w.recognized {
			t.Errorf("findings[%d] = %+v, want severity %q path %q line %d text %q recognized %t",
				i, f, w.severity, w.path, w.line, w.text, w.recognized)
		}
	}
	// Hostile text is kept verbatim, never truncated or rewritten.
	if findings[6].Text != "| pipes | and <!-- html comments --> and `backticks`" {
		t.Errorf("hostile text was rewritten: %q", findings[6].Text)
	}

	// CRLF endings parse the same as LF.
	findings, sawSection = parseFindings(strings.ReplaceAll(reviewFindingsAnswer, "\n", "\r\n"))
	if !sawSection || len(findings) != 9 || findings[0].Text != want[0].text {
		t.Errorf("CRLF parse = %d findings, sawSection %t, first text %q", len(findings), sawSection, findings[0].Text)
	}

	// No findings section at all, and nothing inline either.
	if findings, sawSection := parseFindings("Just an answer, no section, no header."); findings != nil || sawSection {
		t.Errorf("missing section = (%v, %t), want (nil, false)", findings, sawSection)
	}

	// A header-like word that is not the section: "Findings" inside a
	// sentence does not open it, and does not count as sawSection — but the
	// bullet under it is an inline finding now (issue #49).
	findings, sawSection = parseFindings("## Notes on findings so far\n\n- [high] a.go:1 - x")
	if sawSection {
		t.Error("prose header counted as sawSection")
	}
	if len(findings) != 1 || findings[0].Path != "a.go" || findings[0].Line != 1 {
		t.Errorf("prose header inline parse = %+v, want one a.go:1 finding", findings)
	}

	// Malformed bullets are ignored: none of these is a finding, under the
	// section contract or the inline one. A token followed by text that
	// opens like a path:line is dropped whole rather than read as a finding
	// whose text is the half-written location.
	malformed := []string{
		"- high app/x.go:42 - no brackets",
		"- [high app/x.go:42 - never closed",
		"- [] app/x.go:42 - empty severity",
		"- [high]app/x.go:42 - no space after the severity",
		"- [high] app/x.go:42",
		"- [high] app/x.go:42 no separator at all",
		"- [high] :42 - no path",
		"plain prose in the section",
		"- [high] app/x.go:42 - ",
		"- [x] a task-list bullet is not a finding",
		"- [ ] nor an empty one",
		"see [the diff](https://example.com/a:b) and [TODO] fix later",
		"[critical] an unknown severity with no location is prose, not a finding",
	}
	var b strings.Builder
	b.WriteString("## Findings\n\n")
	for _, m := range malformed {
		b.WriteString(m + "\n")
	}
	if findings, _ := parseFindings(b.String()); len(findings) != 0 {
		t.Errorf("malformed bullets produced %d findings: %+v", len(findings), findings)
	}

	// The lenient inline forms (issue #49): a located finding anywhere in a
	// line, with any separator and any case, and a finding with no location
	// when the severity is one of the three known words.
	inline := []struct {
		line       string
		severity   string
		path       string
		line_      int
		text       string
		recognized bool
	}{
		{"Findings: [high] app/x/main.go:42 - the mutex is released twice", "high", "app/x/main.go", 42, "the mutex is released twice", true},
		{"- [HIGH] app/y/queue.go:7 — em dash, capital, no section", "HIGH", "app/y/queue.go", 7, "em dash, capital, no section", true},
		{"[Medium] the queue never drains when a send fails", "Medium", "", 0, "the queue never drains when a send fails", true},
		{"[low] docs: the flag is undocumented", "low", "", 0, "docs: the flag is undocumented", true},
		{"[CRITICAL] app/z/z.go:1 - an unknown scale with a location still reports", "CRITICAL", "app/z/z.go", 1, "an unknown scale with a location still reports", false},
	}
	for _, w := range inline {
		findings, sawSection := parseFindings("prose before\n\n" + w.line + "\n\nprose after")
		if sawSection {
			t.Errorf("%q: inline parse set sawSection", w.line)
		}
		if len(findings) != 1 {
			t.Errorf("%q: got %d findings, want 1: %+v", w.line, len(findings), findings)
			continue
		}
		f := findings[0]
		if f.Severity != w.severity || f.Path != w.path || f.Line != w.line_ || f.Text != w.text || f.Recognized != w.recognized {
			t.Errorf("%q: parsed %+v, want severity %q path %q line %d text %q recognized %t",
				w.line, f, w.severity, w.path, w.line_, w.text, w.recognized)
		}
	}

	// Two findings written on one line both parse.
	findings, _ = parseFindings("[high] a.go:1 - first and [low] b.go:2 - second")
	if len(findings) != 2 || findings[0].Path != "a.go" || findings[1].Path != "b.go" {
		t.Errorf("two inline findings on one line = %+v, want a.go then b.go", findings)
	}

	// Several findings on one line, separated by `;` (issue #111): run
	// 39062136141 closed with one paragraph in exactly this shape, and
	// each finding must keep its own text, not swallow the rest of the
	// line. The `FINDINGS (n, …):` prefix is prose, not a finding.
	paragraph := "FINDINGS (3, 2 blocking): [medium] main.go:1155 - the mutex is released twice under the error path; " +
		"[medium] queue.go:20 - the queue never drains when a send fails; " +
		"[low] docs/readme.md:3 - typo in the flag name"
	findings, sawSection = parseFindings(paragraph)
	if sawSection {
		t.Error("a FINDINGS paragraph counted as sawSection")
	}
	if len(findings) != 3 {
		t.Fatalf("FINDINGS paragraph = %d findings, want 3: %+v", len(findings), findings)
	}
	wantParagraph := []struct {
		severity string
		path     string
		line     int
		text     string
	}{
		{"medium", "main.go", 1155, "the mutex is released twice under the error path"},
		{"medium", "queue.go", 20, "the queue never drains when a send fails"},
		{"low", "docs/readme.md", 3, "typo in the flag name"},
	}
	for i, w := range wantParagraph {
		f := findings[i]
		if f.Severity != w.severity || f.Path != w.path || f.Line != w.line || f.Text != w.text || !f.Recognized {
			t.Errorf("paragraph findings[%d] = %+v, want %v", i, f, w)
		}
	}

	// A `;` before a word that is not a severity is not a boundary: the
	// word is prose and stays in the text, never a finding.
	multiline := "The review is complete.\n\n" +
		"[high] a.go:1 - use the slow path; see the notes on [the queue](x) first; " +
		"[low] b.go:2 - second"
	findings, _ = parseFindings(multiline)
	if len(findings) != 2 || findings[0].Path != "a.go" || findings[0].Text != "use the slow path; see the notes on [the queue](x) first" ||
		findings[1].Path != "b.go" || findings[1].Text != "second" {
		t.Errorf("chained parse = %+v, want a.go with its full text then b.go", findings)
	}

	// A hostile FINDINGS paragraph — markdown, a mention, a very long
	// line — parses as findings and the renderer escapes and caps it,
	// never carrying it verbatim.
	hostileParagraph := "FINDINGS (2, 1 blocking): [high] " + strings.Repeat("x/", 200) + "deep.go:1 - | pipes | and <!-- html comments --> and @alice; " +
		"[low] b.go:2 - " + strings.Repeat("long ", 2000) + "text"
	findings, _ = parseFindings(hostileParagraph)
	if len(findings) != 2 || findings[0].Path != strings.Repeat("x/", 200)+"deep.go" || findings[1].Path != "b.go" {
		t.Fatalf("hostile paragraph = %+v", findings)
	}
	if findings[0].Text != "| pipes | and <!-- html comments --> and @alice" {
		t.Errorf("hostile paragraph text was rewritten: %q", findings[0].Text)
	}
	table := reviewTable(findings)
	assertReviewBodyClean(t, table)
	if !strings.Contains(table, strings.Repeat("x/", 12)) || len(table) > 4096 {
		t.Errorf("hostile paragraph table not capped: %d bytes", len(table))
	}

	// The same finding reached by both passes is kept once.
	findings, _ = parseFindings("## Findings\n\n- [high] app/x/main.go:42 - the mutex is released twice\n" +
		"[high] app/x/main.go:42 - the mutex is released twice\n")
	if len(findings) != 1 {
		t.Errorf("duplicate parse = %d findings, want 1: %+v", len(findings), findings)
	}

	// The cap counts section and inline findings together.
	b.Reset()
	b.WriteString("## Findings\n\n")
	for i := 1; i <= 30; i++ {
		fmt.Fprintf(&b, "- [low] app/cap.go:%d - section finding %d\n", i, i)
	}
	for i := 1; i <= 30; i++ {
		fmt.Fprintf(&b, "[low] app/cap.go:%d - inline finding %d\n", i+100, i)
	}
	findings, _ = parseFindings(b.String())
	if len(findings) != maxReviewFindings {
		t.Fatalf("mixed cap: got %d findings, want %d", len(findings), maxReviewFindings)
	}

	// An empty answer is no findings and no section.
	if findings, sawSection := parseFindings(""); findings != nil || sawSection {
		t.Errorf("empty answer = (%v, %t), want (nil, false)", findings, sawSection)
	}

	// A hostile line of brackets is scanned once, not once per bracket: the
	// one real finding buried in it still parses, and the scan stays linear.
	hostile := strings.Repeat("[[]] ", 20000) + "[high] app/x/main.go:42 - the mutex is released twice" + strings.Repeat(" [ ]", 20000)
	findings, _ = parseFindings(hostile)
	if len(findings) != 1 || findings[0].Path != "app/x/main.go" || findings[0].Line != 42 {
		t.Errorf("hostile bracket line = %+v, want the one real finding", findings)
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

// TestDoneWhenFindingIsActionable pins the format the review brief's
// acceptance-criteria bullet tells the reviewer to quote (issue #136):
// "[high] done-when: <bullet>". It carries no path:line location, so the
// section pass alone rejects it — but parseFindings runs the inline pass
// over every line, inside the findings section too, and textOnlyFinding
// reads it as a recognized high finding. Either way the reviewer's answer
// carries it, and it blocks.
func TestDoneWhenFindingIsActionable(t *testing.T) {
	bullet := "TestIssueReady bullets unmet"
	for name, answer := range map[string]string{
		"in the findings section": "## Findings\n\n- [high] done-when: " + bullet + "\n",
		"inline in prose":         "The acceptance criteria are unmet: [high] done-when: " + bullet + "\n",
	} {
		findings, _ := parseFindings(answer)
		if len(findings) != 1 {
			t.Fatalf("%s: got %d findings, want 1: %+v", name, len(findings), findings)
		}
		f := findings[0]
		if asciiLower(f.Severity) != "high" || !f.Recognized || f.Text != "done-when: "+bullet {
			t.Fatalf("%s: finding = %+v; want a recognized high done-when finding", name, f)
		}
		if got := blockingFindings(findings); len(got) != 1 {
			t.Fatalf("%s: blockingFindings = %+v; want the done-when finding to block", name, got)
		}
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

	// An unfinished review with blocking findings starts a fix run (issue
	// #49): findings parsed from either source act, whatever the stop reason.
	server = commentsServer(t, reportComments(1), http.StatusOK)
	ghEnv(t, server)
	action, body, used, err = reviewPlan(newGitHub(), 7, Result{Stop: stopDeadline}, true, hostile, true)
	if err != nil {
		t.Fatal(err)
	}
	if action != "fix" || used != 0 {
		t.Fatalf("early stop with blocking findings: action = %q, used = %d", action, used)
	}
	if !strings.HasPrefix(body, command+" fix: 2 blocking findings") || !strings.Contains(body, "`deadline`") {
		t.Errorf("early-stop fix body wrong: %q", body)
	}
	assertReviewBodyClean(t, body)

	// The same, when the result envelope is missing entirely and the
	// findings came from the run record: they still act.
	server = commentsServer(t, reportComments(1), http.StatusOK)
	ghEnv(t, server)
	action, body, _, err = reviewPlan(newGitHub(), 7, Result{}, false, hostile, false)
	if err != nil {
		t.Fatal(err)
	}
	if action != "fix" {
		t.Fatalf("no result with blocking findings: action = %q", action)
	}
	assertReviewBodyClean(t, body)

	// An unfinished review whose findings are not blocking stays advisory:
	// the findings shown, no label and no fix — but the report says the run
	// ended early after finishing its review, not that nothing was found
	// (issue #111).
	server = commentsServer(t, reportComments(1), http.StatusOK)
	ghEnv(t, server)
	lowOnly := []Finding{{Severity: "low", Path: "docs/readme.md", Line: 3, Text: "typo", Recognized: true}}
	action, body, _, err = reviewPlan(newGitHub(), 7, Result{Stop: stopDeadline}, true, lowOnly, false)
	if err != nil {
		t.Fatal(err)
	}
	if action != "report-only" {
		t.Fatalf("early stop with low findings: action = %q", action)
	}
	for _, want := range []string{"`deadline`", "after finishing its review", "| severity | location | finding |"} {
		if !strings.Contains(body, want) {
			t.Errorf("advisory early-stop body missing %q: %q", want, body)
		}
	}
	if strings.Contains(body, "No findings section") {
		t.Errorf("a finished review must not read as one with no findings: %q", body)
	}
	if strings.Contains(body, reviewPassedLabel) || strings.HasPrefix(body, command+" fix:") {
		t.Errorf("an advisory review must not label or fix: %q", body)
	}
	assertReviewBodyClean(t, body)

	// An unfinished review with no findings anywhere: advisory, and the
	// verdict says neither source carried findings.
	server = commentsServer(t, reportComments(1), http.StatusOK)
	ghEnv(t, server)
	action, body, _, err = reviewPlan(newGitHub(), 7, Result{Stop: stopIncomplete}, true, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if action != "report-only" {
		t.Fatalf("early stop without findings: action = %q", action)
	}
	if !strings.Contains(body, "no findings section and no inline findings were found in the answer or the run record") {
		t.Errorf("advisory body does not say where it looked: %q", body)
	}
	assertReviewBodyClean(t, body)

	// A failing comments fetch is an error, like continuePlan.
	server = commentsServer(t, nil, http.StatusInternalServerError)
	ghEnv(t, server)
	if _, _, _, err := reviewPlan(newGitHub(), 7, Result{Stop: stopDone}, true, hostile, true); err == nil {
		t.Error("a failing comment fetch must return an error")
	}
}

// TestReviewTableUnlocated renders the findings table for an unlocated
// finding — the inline form (issue #49) whose path and line are empty — and
// pins the whole table, because that cell used to render as `:0` (issue
// #190): a location a fix run and a reader both read, naming no file. The
// word `unlocated` is the explicit fallback, and no `:0` may appear
// anywhere in the table.
func TestReviewTableUnlocated(t *testing.T) {
	answer := "## Findings\n\n" +
		"- [medium] service.go wc -l 1200, split it\n" +
		"- [high] app/x/main.go:42 - the mutex is released twice\n"
	findings, sawSection := parseFindings(answer)
	if !sawSection || len(findings) != 2 {
		t.Fatalf("parse = %d findings, sawSection %t, want 2/true: %+v", len(findings), sawSection, findings)
	}
	// The first finding carries no location at all; the second does.
	if findings[0].Path != "" || findings[0].Line != 0 {
		t.Fatalf("first finding is not unlocated: %+v", findings[0])
	}
	if findings[1].Path != "app/x/main.go" || findings[1].Line != 42 {
		t.Fatalf("second finding lost its location: %+v", findings[1])
	}

	table := reviewTable(findings)
	want := "| severity | location | finding |\n|---|---|---|\n" +
		"| `medium` | unlocated | service.go wc -l 1200, split it |\n" +
		"| `high` | app/x/main.go:42 | the mutex is released twice |\n"
	if table != want {
		t.Errorf("unlocated table =\n%q\nwant\n%q", table, want)
	}
	if strings.Contains(table, ":0") {
		t.Errorf("the table renders :0 for an unlocated finding: %q", table)
	}
	assertReviewBodyClean(t, table)

	// The whole comment — table, command line and footer — carries no `:0`.
	t.Setenv("RESULT_PATH", "")
	t.Setenv("LEDGER_PATH", "")
	t.Setenv("TRAJECTORY_PATH", "")
	blocking := blockingFindings(findings)
	if len(blocking) != 2 {
		t.Fatalf("blocking = %d findings, want 2: %+v", len(blocking), blocking)
	}
	body := reviewFixBody(blocking, findings, 0, "zai-org/glm-5.3-flash", stopDone)
	if !strings.Contains(body, "| `medium` | unlocated |") {
		t.Errorf("the fix comment does not name the unlocated finding: %q", body)
	}
	if strings.Contains(body, ":0") {
		t.Errorf("the fix comment renders :0: %q", body)
	}
	assertReviewBodyClean(t, body)

	// A located finding still renders its own path and line unchanged, and
	// the renderer is what decides: a zero line with a path is not unlocated.
	if got := reviewTable([]Finding{{Severity: stopDone, Path: "a.go", Line: 0}}); !strings.Contains(got, "| a.go:0 |") {
		t.Errorf("a path with a zero line stopped rendering its location: %q", got)
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
	if err := os.WriteFile(filepath.Join(single, "single-models.json"), []byte(`{"limits":{"plan":"big","weekly_credit_usd":20,"run_credit_usd":5,"overage_usd":0},"models":[{"id":"only/model","role":"mid","upstream":"u","efforts":["high"]}]}`), 0o644); err != nil {
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
		"single word `none`",                                  // the clean-review form
		// Issue #189: the reviewer's standing instructions say the PATH
		// harness is the pre-change build, so a harness change is reviewed
		// against a binary built from the diff's tree.
		reviewHarnessPreChangeSentence,
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

	t.Run("an early stop with blocking findings starts a fix run", func(t *testing.T) {
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
		if out != "action=fix\nused=0\nblocking=2\n" {
			t.Errorf("stdout = %q", out)
		}
		data, err := os.ReadFile(comment)
		if err != nil {
			t.Fatalf("comment not written: %v", err)
		}
		if !strings.Contains(string(data), "`deadline`") {
			t.Errorf("fix comment does not name the stop reason: %q", data)
		}
		assertReviewBodyClean(t, string(data))
	})

	t.Run("an early stop with only low findings is report-only", func(t *testing.T) {
		server := commentsServer(t, reportComments(1), http.StatusOK)
		ghEnv(t, server)
		setArgs("7")
		writeReviewResult(t, stopDeadline, "## Findings\n\n- [low] docs/readme.md:3 - typo in the flag name\n")
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

	t.Run("inline findings act on an incomplete review", func(t *testing.T) {
		// Run 39010363221: the review wrote its findings inline, with no
		// findings section, and was read as a review with none.
		server := commentsServer(t, reportComments(1), http.StatusOK)
		ghEnv(t, server)
		setArgs("7")
		writeReviewResult(t, stopIncomplete, "The review is finished. Findings: [high] app/x/main.go:42 - the mutex is released twice; and [medium] the queue never drains when a send fails.")
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
		if !strings.Contains(string(data), "high app/x/main.go:42; medium\n\n") {
			t.Errorf("inline findings missing from the command line: %q", data)
		}
		assertReviewBodyClean(t, string(data))
	})

	t.Run("the run record's final result is a fallback", func(t *testing.T) {
		server := commentsServer(t, reportComments(1), http.StatusOK)
		ghEnv(t, server)
		setArgs("7")
		// The envelope's answer is empty; the review's text lives in the
		// root task's end record, as it did on run 39010363221.
		writeReviewResult(t, stopIncomplete, "")
		writeTrajectory(t, `{"kind":"step","step":1,"command":"go test ./...","observation":"ok"}
{"kind":"end","step":0,"steps":1,"result":"The review found two problems.\n\n[high] app/x/main.go:42 - the mutex is released twice\n[low] docs/readme.md:3 - typo"}
`)
		reviewLedger(t)
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
		if !strings.Contains(string(data), "| app/x/main.go:42 |") {
			t.Errorf("run-record finding missing from the table: %q", data)
		}
		assertReviewBodyClean(t, string(data))
	})

	t.Run("the fallback also answers a section with no bullets", func(t *testing.T) {
		server := commentsServer(t, reportComments(1), http.StatusOK)
		ghEnv(t, server)
		setArgs("7")
		writeReviewResult(t, stopIncomplete, "## Findings\n\nnone\n")
		writeTrajectory(t, `{"kind":"end","result":"[high] app/x/main.go:42 - the mutex is released twice\n"}
`)
		reviewLedger(t)
		t.Setenv("REVIEW_COMMENT_PATH", comment)
		out, err := captureStdout(t, runReviewReport)
		if err != nil {
			t.Fatal(err)
		}
		if out != "action=fix\nused=0\nblocking=1\n" {
			t.Errorf("stdout = %q", out)
		}
	})

	t.Run("the run record beside the result is found without TRAJECTORY_PATH", func(t *testing.T) {
		server := commentsServer(t, reportComments(1), http.StatusOK)
		ghEnv(t, server)
		setArgs("7")
		resultPath := writeReviewResult(t, stopIncomplete, "")
		record := `{"kind":"end","result":"[high] app/x/main.go:42 - the mutex is released twice\n"}
`
		if err := os.WriteFile(filepath.Join(filepath.Dir(resultPath), trajectoryFileName), []byte(record), 0o644); err != nil {
			t.Fatal(err)
		}
		reviewLedger(t)
		t.Setenv("REVIEW_COMMENT_PATH", comment)
		out, err := captureStdout(t, runReviewReport)
		if err != nil {
			t.Fatal(err)
		}
		if out != "action=fix\nused=0\nblocking=1\n" {
			t.Errorf("stdout = %q", out)
		}
	})

	t.Run("a hostile run record is read as data", func(t *testing.T) {
		server := commentsServer(t, reportComments(1), http.StatusOK)
		ghEnv(t, server)
		setArgs("7")
		writeReviewResult(t, stopIncomplete, "")
		writeTrajectory(t, `not json at all
{"kind":"end"}
{"kind":"end","result":42}
["kind","end"]
{"kind":"step","result":"[high] step.go:1 - not an ending"}
{"kind":"begin","result":"[high] begin.go:1 - not an ending either"}
{"kind":"end","result":"prose, then findings.\n\n- [high] app/x/main.go:42 - | pipes | and <!-- html comments --> and @alice\n"}
{"kind":"end"}trailing garbage on the same line
`)
		reviewLedger(t)
		t.Setenv("REVIEW_COMMENT_PATH", comment)
		out, err := captureStdout(t, runReviewReport)
		if err != nil {
			t.Fatal(err)
		}
		// Only the real end line's findings act; the step and begin lines
		// never do, and the malformed lines are skipped, not fatal.
		if out != "action=fix\nused=0\nblocking=1\n" {
			t.Errorf("stdout = %q", out)
		}
		data, err := os.ReadFile(comment)
		if err != nil {
			t.Fatalf("comment not written: %v", err)
		}
		assertReviewBodyClean(t, string(data))
	})

	t.Run("a long run record is read from its tail, still capped", func(t *testing.T) {
		server := commentsServer(t, reportComments(1), http.StatusOK)
		ghEnv(t, server)
		setArgs("7")
		writeReviewResult(t, stopIncomplete, "")
		// The end record is the record's last line (issue #111): the
		// 36-step review of run 39062136141 pushed it past a read capped
		// from the head, and the report threw the review away. Padding
		// before the ending no longer hides it — but the read stays
		// capped at maxTrajectoryBytes.
		padding := strings.Repeat("#", maxTrajectoryBytes) + "\n"
		writeTrajectory(t, padding+`{"kind":"end","result":"[high] app/x/main.go:42 - beyond the head, in the tail\n"}`)
		reviewLedger(t)
		t.Setenv("REVIEW_COMMENT_PATH", comment)
		out, err := captureStdout(t, runReviewReport)
		if err != nil {
			t.Fatal(err)
		}
		if out != "action=fix\nused=0\nblocking=1\n" {
			t.Errorf("tail read: stdout = %q", out)
		}
		data, err := os.ReadFile(comment)
		if err != nil {
			t.Fatalf("comment not written: %v", err)
		}
		assertReviewBodyClean(t, string(data))

		// What follows the end record can still push it out of reach: a
		// record ending in megabytes of garbage is read as its tail, the
		// end record outside the cap is never seen, and the garbage is
		// skipped, not fatal.
		trailing := strings.Repeat("{", 2*maxTrajectoryBytes) + "\n"
		writeTrajectory(t, padding+`{"kind":"end","result":"[high] app/x/main.go:42 - out of reach\n"}`+"\n"+trailing)
		out, err = captureStdout(t, runReviewReport)
		if err != nil {
			t.Fatal(err)
		}
		if out != "action=report-only\nused=0\nblocking=0\n" {
			t.Errorf("end record out of reach: stdout = %q", out)
		}
	})

	t.Run("the run that prompted issue #111 is reported as a review", func(t *testing.T) {
		// Run 39062136141: a 36-step review, an empty envelope answer, a
		// closing result holding one FINDINGS paragraph, stop incomplete.
		// The findings must act, not read as "no findings found".
		server := commentsServer(t, reportComments(1), http.StatusOK)
		ghEnv(t, server)
		setArgs("7")
		writeReviewResult(t, stopIncomplete, "")
		var record strings.Builder
		for step := 1; step <= 36; step++ {
			fmt.Fprintf(&record, "{\"kind\":\"step\",\"step\":%d,\"command\":\"go test ./...\",\"observation\":\"ok\"}\n", step)
		}
		record.WriteString("{\"kind\":\"end\",\"step\":0,\"steps\":36,\"result\":\"" +
			"The review is complete. Reported in the answer under ## Findings.\\n\\n" +
			"FINDINGS (3, 2 blocking): [medium] main.go:1155 - the mutex is released twice under the error path; " +
			"[medium] queue.go:20 - the queue never drains when a send fails; " +
			"[low] docs/readme.md:3 - typo in the flag name\"}\n")
		writeTrajectory(t, record.String())
		reviewLedger(t)
		t.Setenv("REVIEW_COMMENT_PATH", comment)
		out, err := captureStdout(t, runReviewReport)
		if err != nil {
			t.Fatal(err)
		}
		if out != "action=fix\nused=0\nblocking=2\n" {
			t.Fatalf("stdout = %q, want a fix run for the 2 blocking findings", out)
		}
		data, err := os.ReadFile(comment)
		if err != nil {
			t.Fatalf("comment not written: %v", err)
		}
		body := string(data)
		for _, want := range []string{
			"| `medium` | main.go:1155 | the mutex is released twice under the error path |",
			"| `medium` | queue.go:20 | the queue never drains when a send fails |",
			"| `low` | docs/readme.md:3 | typo in the flag name |",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("finding table missing %q in %q", want, body)
			}
		}
		assertReviewBodyClean(t, body)
	})

	t.Run("an incomplete run whose review finished with only low findings", func(t *testing.T) {
		// Issue #111: the closing result holds a finished review, so the
		// report lists its findings and says the run ended incomplete
		// after finishing its review — never "no findings found".
		server := commentsServer(t, reportComments(1), http.StatusOK)
		ghEnv(t, server)
		setArgs("7")
		writeReviewResult(t, stopIncomplete, "")
		writeTrajectory(t, `{"kind":"end","result":"FINDINGS (1, 0 blocking): [low] docs/readme.md:3 - typo in the flag name"}`)
		reviewLedger(t)
		t.Setenv("REVIEW_COMMENT_PATH", comment)
		out, err := captureStdout(t, runReviewReport)
		if err != nil {
			t.Fatal(err)
		}
		if out != "action=report-only\nused=0\nblocking=0\n" {
			t.Fatalf("stdout = %q, want report-only", out)
		}
		data, err := os.ReadFile(comment)
		if err != nil {
			t.Fatalf("comment not written: %v", err)
		}
		body := string(data)
		for _, want := range []string{"after finishing its review", "| `low` | docs/readme.md:3 | typo in the flag name |"} {
			if !strings.Contains(body, want) {
				t.Errorf("body missing %q: %q", want, body)
			}
		}
		if strings.Contains(body, "no findings section") {
			t.Errorf("a finished review must not read as one with no findings: %q", body)
		}
		assertReviewBodyClean(t, body)
	})

	t.Run("a long run record ships its tail, end record included", func(t *testing.T) {
		// The run job ships the record beside the result; the copy must
		// keep the record's ending (issue #111), where the end record of
		// a long run lives, not its head.
		home := t.TempDir()
		t.Setenv("CODEAF_HOME", home)
		recordDir := filepath.Join(home, "runs", "42", "tasks", "root")
		if err := os.MkdirAll(recordDir, 0o755); err != nil {
			t.Fatal(err)
		}
		padding := strings.Repeat("#", maxTrajectoryBytes) + "\n"
		record := padding + `{"kind":"end","result":"[high] app/x/main.go:42 - shipped from the tail\n"}` + "\n"
		if err := os.WriteFile(filepath.Join(recordDir, "trajectory.jsonl"), []byte(record), 0o644); err != nil {
			t.Fatal(err)
		}
		resultPath := writeReviewResult(t, stopIncomplete, "")
		t.Setenv("TRAJECTORY_PATH", "")
		if shipped := shipRunRecord(); shipped == "" {
			t.Fatal("shipRunRecord found nothing")
		}
		data, err := os.ReadFile(filepath.Join(filepath.Dir(resultPath), trajectoryFileName))
		if err != nil {
			t.Fatalf("record not shipped: %v", err)
		}
		if got := readClosing(filepath.Join(filepath.Dir(resultPath), trajectoryFileName)); got.Answer == "" {
			t.Errorf("the shipped copy lost the end record: %d bytes, tail starts %q", len(data), data[:40])
		}
	})

	t.Run("a run record with no findings stays advisory", func(t *testing.T) {
		server := commentsServer(t, reportComments(1), http.StatusOK)
		ghEnv(t, server)
		setArgs("7")
		writeReviewResult(t, stopIncomplete, "")
		writeTrajectory(t, `{"kind":"end","result":"The review is clean."}
`)
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
