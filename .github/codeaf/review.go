// review.go supports the independent review pass (issue #32): after publish
// opens or updates a pull request, a separate CodeAF run reviews it in
// read-only mode on a model other than the one that wrote the patch. This
// file parses the reviewer's findings, decides what follows — a fix run, a
// pass label, or a report-only comment — and renders that comment. Everything
// here is standard library only.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// reviewPassedLabel marks a pull request whose independent review found
// nothing blocking, so a person or a merge rule can merge with confidence.
const reviewPassedLabel = "codeaf-review-passed"

// maxReviewFindings caps how many findings are parsed from one review, so a
// hostile answer cannot grow the comment without bound; the rest are ignored.
const maxReviewFindings = 50

// maxReviewDiffBytes caps the diff embedded in the review brief.
const maxReviewDiffBytes = 60000

// Finding is one finding of the independent review, parsed from the
// machine-readable section of the reviewer's answer.
type Finding struct {
	Severity   string // as written: high, medium, low, or an unknown word
	Path       string
	Line       int
	Text       string
	Recognized bool // Severity is one of high, medium or low
}

var (
	// reviewSectionHeader opens the machine-readable section: a markdown
	// header whose first word is "findings", case-insensitive.
	reviewSectionHeader = regexp.MustCompile(`(?i)^#{1,6}\s*findings\b`)
	// markdownHeader is any markdown header line; it closes the section.
	markdownHeader = regexp.MustCompile(`^#{1,6}(\s|$)`)
)

// parseFindings reads the machine-readable findings out of a reviewer's
// answer (issue #32). The contract is a markdown section whose header line
// matches ^#{1,6}\s*findings\b case-insensitively; only bullets inside that
// section — until the next markdown header or the end of the answer — count,
// and sawSection reports whether the header was present at all. A bullet is
// [severity], a path, a line number, a separator and the text; malformed
// bullets are ignored. Text and path are never truncated at parse time; the
// comment renderer escapes and caps them.
func parseFindings(answer string) (findings []Finding, sawSection bool) {
	inSection := false
	for _, raw := range strings.Split(answer, "\n") {
		line := strings.TrimLeft(strings.TrimSuffix(raw, "\r"), " \t")
		switch {
		case reviewSectionHeader.MatchString(line):
			sawSection = true
			inSection = true
			continue
		case markdownHeader.MatchString(line):
			inSection = false
			continue
		}
		if !inSection {
			continue
		}
		f, ok := parseFinding(line)
		if !ok {
			continue
		}
		findings = append(findings, f)
		if len(findings) >= maxReviewFindings {
			break // cap reached: ignore the rest
		}
	}
	return findings, sawSection
}

// parseFinding parses one bullet of the findings section: an optional - or *
// bullet, then [severity], then whitespace, then the path (a run of non-space
// characters up to the colon before the line number), digits, then a
// separator — an em dash, an en dash, a double hyphen, a single hyphen or a
// colon, optionally surrounded by spaces — then the text, which is the rest
// of the line and is kept verbatim. Severity is matched case-insensitively
// against high, medium and low and sets Recognized; any other bracketed word
// still parses as a finding with Recognized=false, so a reviewer writing an
// unknown scale is reported rather than silently dropped.
func parseFinding(line string) (Finding, bool) {
	s := line
	if strings.HasPrefix(s, "- ") || strings.HasPrefix(s, "* ") {
		s = s[2:]
	}
	if !strings.HasPrefix(s, "[") {
		return Finding{}, false
	}
	end := strings.IndexByte(s, ']')
	if end < 2 { // "[x]" is the shortest severity
		return Finding{}, false
	}
	severity := s[1:end]
	s = s[end+1:]
	if s == "" || (s[0] != ' ' && s[0] != '\t') {
		return Finding{}, false // the path needs whitespace after the severity
	}
	s = strings.TrimLeft(s, " \t")
	i := 0
	for i < len(s) && s[i] != ':' && s[i] != ' ' && s[i] != '\t' {
		i++
	}
	if i == 0 || i >= len(s) || s[i] != ':' {
		return Finding{}, false // no path, or no colon before the line number
	}
	path := s[:i]
	s = s[i+1:]
	j := 0
	for j < len(s) && s[j] >= '0' && s[j] <= '9' {
		j++
	}
	if j == 0 {
		return Finding{}, false // no line number
	}
	n, err := strconv.Atoi(s[:j])
	if err != nil || n <= 0 {
		return Finding{}, false
	}
	text, ok := findingText(s[j:])
	if !ok {
		return Finding{}, false
	}
	return Finding{
		Severity:   severity,
		Path:       path,
		Line:       n,
		Text:       text,
		Recognized: asciiLower(severity) == "high" || asciiLower(severity) == "medium" || asciiLower(severity) == "low",
	}, true
}

// findingText reads what follows the line number and returns the text: after
// the digits there must be the separator itself, or whitespace followed by
// it — an em dash, an en dash, a double hyphen, a single hyphen or a colon.
// What remains after the separator is the text.
func findingText(s string) (string, bool) {
	if s == "" {
		return "", false
	}
	if s[0] == ' ' || s[0] == '\t' {
		k := 0
		for k < len(s) && (s[k] == ' ' || s[k] == '\t') {
			k++
		}
		if k == len(s) {
			return "", false // spaces and nothing else
		}
		s = s[k:]
	}
	var text string
	switch {
	case strings.HasPrefix(s, "—"), strings.HasPrefix(s, "–"):
		text = s[len("—"):]
	case strings.HasPrefix(s, "--"):
		text = s[2:]
	case s[0] == '-', s[0] == ':':
		text = s[1:]
	default:
		return "", false // a real separator is required
	}
	if text = strings.TrimLeft(text, " \t"); text == "" {
		return "", false
	}
	return text, true
}

// blockingFindings are the findings that must be fixed before a pull request
// can pass the review: recognized severities high and medium (issue #32).
func blockingFindings(findings []Finding) []Finding {
	var out []Finding
	for _, f := range findings {
		if f.Recognized && (asciiLower(f.Severity) == "high" || asciiLower(f.Severity) == "medium") {
			out = append(out, f)
		}
	}
	return out
}

// hasUnrecognizedSeverity reports whether any finding carries a severity word
// outside high, medium and low: the pass label is withheld until the scale
// the reviewer used is understood.
func hasUnrecognizedSeverity(findings []Finding) bool {
	for _, f := range findings {
		if !f.Recognized {
			return true
		}
	}
	return false
}

// hasLowFindings reports whether any finding is a recognized low.
func hasLowFindings(findings []Finding) bool {
	for _, f := range findings {
		if f.Recognized && asciiLower(f.Severity) == "low" {
			return true
		}
	}
	return false
}

// reviewerModel chooses the model for the reviewer seat (issue #32): the
// review must not share the worker's blind spots, so it never runs on the
// model that wrote the patch. Of the allowed models (models.json), the one
// with role "mid" whose id differs from the worker's is preferred, then the
// first allowed model with a different id. When every allowed model is the
// worker model the choice fails loudly: a review on the same model is
// exactly what issue #32 rules out.
func reviewerModel(cfg Config, workerID string) (Model, error) {
	var other *Model
	for i := range cfg.Models {
		m := cfg.Models[i]
		if m.ID == workerID {
			continue
		}
		if m.Role == "mid" {
			return m, nil
		}
		if other == nil {
			other = &m
		}
	}
	if other != nil {
		return *other, nil
	}
	return Model{}, fmt.Errorf("no allowed model differs from the worker model %q; the reviewer must not run on the worker model", workerID)
}

// ---------------------------------------------------------------- the decision

// reviewPlan decides what follows an independent review (issue #32):
//
//   - no result at all: report-only — nothing can be concluded;
//   - a result that is not stop "done": report-only — an incomplete review
//     never blocks and never labels, and its findings so far are advisory;
//   - a finished review with high or medium findings: a fix run, started as
//     a continuation and counted against the continuation cap (issue #31);
//     at the cap, report-only and a person takes over;
//   - a finished review with an unrecognized severity: report-only, and the
//     pass label is withheld because the scale was not understood;
//   - otherwise: label — a clean pass.
//
// The comment body is built only from fixed sentences, counters and findings
// whose remote-derived strings are escaped and capped; it never carries the
// run-report marker, so it is never mistaken for a run report, and no raw
// answer text reaches it unescaped. A failing comments fetch is an error,
// like continuePlan.
func reviewPlan(gh github, pr int, result Result, haveResult bool, findings []Finding, sawSection bool) (action string, body string, used int, err error) {
	if !haveResult {
		return "report-only", reviewReportBody(
			"The review produced no result: its output is missing or unparseable, so nothing can be concluded from this run. "+
				"No label is added and no fix run starts; a maintainer can re-run the review.",
			nil, result.Model, stopUnknown), 0, nil
	}
	stop := sanitizeStop(result.Stop)
	if !readyForReview(stop) {
		verdict := fmt.Sprintf("The review stopped early (stop reason: `%s`), so this is an incomplete review: "+
			"its findings so far are advisory, nothing is labelled and no fix run starts.", stop)
		if !sawSection {
			verdict += " The reviewer wrote no findings section yet."
		}
		return "report-only", reviewReportBody(verdict, findings, result.Model, stop), 0, nil
	}
	blocking := blockingFindings(findings)
	if len(blocking) > 0 {
		var comments []issueComment
		if err := gh.get(fmt.Sprintf("/issues/%d/comments?per_page=100", pr), &comments); err != nil {
			return "", "", 0, err
		}
		used = continuationsUsed(countReports(comments))
		if !continuationCapReached(used) {
			return "fix", reviewFixBody(blocking, findings, used, result.Model, stop), used, nil
		}
		return "report-only", reviewReportBody(fmt.Sprintf(
			"The automatic fix budget of %d continuations per pull request is spent, so no fix run starts; "+
				"a maintainer can continue by hand with a %s comment. The blocking findings above stay open for a person to act on.",
			maxContinuations, command), findings, result.Model, stop), used, nil
	}
	if hasUnrecognizedSeverity(findings) {
		return "report-only", reviewReportBody(
			"The review found nothing blocking, but the pass label is withheld because at least one severity "+
				"was not recognized (high, medium or low are); a maintainer reads the findings below and decides.",
			findings, result.Model, stop), 0, nil
	}
	verdict := fmt.Sprintf("Nothing blocking was found: the %s label was added, so a person or a merge rule can merge with confidence.",
		reviewPassedLabel)
	if !hasLowFindings(findings) {
		return "label", reviewReportBody(verdict, nil, result.Model, stop), 0, nil
	}
	return "label", reviewReportBody(verdict, findings, result.Model, stop), 0, nil
}

// reviewFixBody is the comment that starts a fix run: its first line is the
// command the workflow acts on, then the findings table and a verdict, then
// the footer.
func reviewFixBody(blocking, findings []Finding, used int, model, stop string) string {
	return reviewFixCommand(blocking) + "\n\n" +
		fmt.Sprintf("A fix run starts now, as continuation %d of %d on this pull request; "+
			"it receives the findings above and the issue the pull request closes, and nothing else to do beyond them.\n\n",
			used+1, maxContinuations) +
		reviewTable(findings) + "\n" + reviewFooter(model, stop)
}

// reviewReportBody is a report-only comment: a verdict paragraph, an optional
// findings table and the footer. It never starts a run and never adds a
// label.
func reviewReportBody(verdict string, findings []Finding, model, stop string) string {
	body := verdict + "\n\n"
	if len(findings) > 0 {
		body += reviewTable(findings) + "\n"
	}
	return body + reviewFooter(model, stop)
}

// reviewFixCommand is the first line of a fix comment: the /codeaf command
// the workflow acts on. It is built only from sanitized severity words and
// safe paths, so a hostile finding can neither inject shell nor break out of
// the command line that the fix run parses again.
func reviewFixCommand(blocking []Finding) string {
	parts := make([]string, 0, len(blocking))
	for _, f := range blocking {
		parts = append(parts, asciiLower(f.Severity)+" "+safeReviewPath(f.Path)+":"+strconv.Itoa(f.Line))
	}
	plural := "s"
	if len(blocking) == 1 {
		plural = ""
	}
	return fmt.Sprintf("%s fix: %d blocking finding%s - %s", command, len(blocking), plural, strings.Join(parts, "; "))
}

// safeReviewPath narrows a finding's path to the characters a repository
// path can contain — letters, digits, slash, dot, underscore, hyphen — and
// caps its length. Anything else, and it is replaced whole: the command line
// is parsed again by the fix run, and a quoted, spaced or metacharacter path
// must never carry injected text through it. The full path still appears in
// the table, escaped.
func safeReviewPath(path string) string {
	const maxPath = 200
	if len(path) > maxPath {
		return "(unsafe path)"
	}
	for i := 0; i < len(path); i++ {
		c := path[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '/' || c == '.' || c == '_' || c == '-'
		if !ok {
			return "(unsafe path)"
		}
	}
	return path
}

// reviewTable renders the findings as a markdown table. Every cell is
// escaped and capped: the severity through tokenCell (a known-shaped word in
// backticks, anything else escaped), the location and the text through
// escapeMD — so a hostile answer cannot inject markdown, mention anyone, or
// carry a raw HTML comment into the posted comment.
func reviewTable(findings []Finding) string {
	var b strings.Builder
	b.WriteString("| severity | location | finding |\n|---|---|---|\n")
	for _, f := range findings {
		fmt.Fprintf(&b, "| %s | %s:%d | %s |\n",
			tokenCell(f.Severity, 200), escapeMD(f.Path, 200, true), f.Line, escapeMD(f.Text, 200, true))
	}
	return b.String()
}

// reviewFooter ends the review comment: the reviewer model, the sanitized
// stop word, this run's metered spend from the proxy ledger, and a link to
// the workflow run with its diagnostics artifact when one was uploaded. The
// ledger is written by the run job and the model id comes from its result,
// so both are treated as untrusted: an unreadable ledger reads as unknown,
// and the model id is escaped before it is rendered.
func reviewFooter(model, stop string) string {
	stop = sanitizeStop(stop) // defensive: callers pass sanitized words already
	var b strings.Builder
	b.WriteString("---\n\nReviewer model: ")
	if model == "" {
		b.WriteString("(unknown)")
	} else {
		b.WriteString("`" + escapeMD(model, 200, true) + "`")
	}
	b.WriteString(" · stop: `" + stop + "`")
	if lines, err := readLedger(os.Getenv("LEDGER_PATH")); err != nil {
		b.WriteString(" · metered spend this run: (unknown)")
	} else {
		b.WriteString(" · metered spend this run: " + money(ledgerTotal(lines)))
	}
	// The link is built from the runner's own variables and a digit-only
	// artifact id, like the run report's link; it is not remote text.
	server := os.Getenv("GITHUB_SERVER_URL")
	if server == "" {
		server = "https://github.com"
	}
	if repo, run := os.Getenv("GITHUB_REPOSITORY"), os.Getenv("GITHUB_RUN_ID"); repo != "" && run != "" {
		link := fmt.Sprintf("%s/%s/actions/runs/%s", server, repo, run)
		if id := diagnosticsArtifact(); digitsOnly(id) {
			link += "/artifacts/" + id
		}
		fmt.Fprintf(&b, " · [Workflow run](%s)", link)
	}
	b.WriteString("\n")
	return b.String()
}

// ---------------------------------------------------------------- commands

// reviewOutputContract restates, at the end of the review brief, the one
// part of the answer the harness parses (issue #32).
const reviewOutputContract = "# The output the harness parses\n\n" +
	"End your answer with a section exactly titled `## Findings`, holding one bullet per finding, " +
	"and write nothing after it:\n\n" +
	"- [high] app/zip/queue.go:42 - the mutex is released twice under the error path, so a second request deadlocks\n\n" +
	"Severity is high, medium or low. When the review is clean, write the section with no bullets under it."

// capDiff caps the diff embedded in the review brief without splitting a
// UTF-8 rune, noting the truncation; the note counts against the cap.
func capDiff(diff string, max int) string {
	if len(diff) <= max {
		return diff
	}
	note := fmt.Sprintf("\n(diff truncated at %d bytes)\n", max)
	cut := max - len(note)
	if cut < 0 {
		cut = 0
	}
	for cut > 0 && !utf8.RuneStart(diff[cut]) {
		cut--
	}
	return diff[:cut] + note
}

// runReviewBrief implements `harness review-brief` (issue #32): the review
// run's brief. It mirrors runBrief's conventions — the budget line, the
// standing instructions from the harness directory, sections joined by a
// blank line, the whole brief capped, printed and written to BRIEF_PATH.
// Env NUMBER is the pull request to review.
func runReviewBrief() error {
	number, err := strconv.Atoi(os.Getenv("NUMBER"))
	if err != nil {
		return fmt.Errorf("NUMBER: %w", err)
	}
	briefPath := os.Getenv("BRIEF_PATH")
	if briefPath == "" {
		return errors.New("BRIEF_PATH is empty")
	}
	gh := newGitHub()
	standing, err := os.ReadFile(filepath.Join(harnessDir(), "review-instructions.md"))
	if err != nil {
		return err
	}
	parts := []string{budgetLine(), string(standing)}

	var pr item
	if err := gh.get(fmt.Sprintf("/pulls/%d", number), &pr); err != nil {
		return err
	}
	parts = append(parts, fmt.Sprintf("# The pull request under review: #%d — %s\n\nHead branch `%s`, base branch `%s`.\n\n%s",
		number, pr.Title, pr.Head.Ref, pr.Base.Ref, pr.Body))
	// What the review judges the diff against is the issue the pull request
	// closes (issue #32), read exactly as runBrief reads it: an API failure
	// degrades to a note and must not fail the brief.
	if n := linkedIssue(pr.Body); n != 0 {
		var closes item
		if err := gh.get(fmt.Sprintf("/issues/%d", n), &closes); err != nil {
			parts = append(parts, fmt.Sprintf("(the issue this PR closes, #%d, is unavailable: %s)\n", n, escapeMD(err.Error(), 200, true)))
		} else {
			parts = append(parts, fmt.Sprintf("## The issue this PR closes (#%d) — %s\n\n%s", n, escapeMD(closes.Title, 200, true), closes.Body))
		}
	}
	diff, err := gh.diff(number)
	if err != nil {
		return err
	}
	parts = append(parts, "# The diff under review\n\n```diff\n"+capDiff(diff, maxReviewDiffBytes)+"\n```")
	parts = append(parts, previousReportSection(gh, number))
	parts = append(parts, reviewOutputContract)

	brief := capBrief(strings.Join(parts, "\n\n")+"\n", maxBriefBytes)
	fmt.Print(brief)
	if err := os.MkdirAll(filepath.Dir(briefPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(briefPath, []byte(brief), 0o644)
}

// runReviewConfig implements `harness review-config` (issue #32): it prints
// the CodeAF config for the review run — codeaf-config.json with the worker
// tier replaced by the reviewer model chosen by reviewerModel — so the
// review never shares the model that wrote the patch. It fails loudly on an
// unreadable config and when no different model exists.
func runReviewConfig() error {
	cfgPath := filepath.Join(harnessDir(), "codeaf-config.json")
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return err
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("%s: %w", cfgPath, err)
	}
	worker, _ := config["models.tiers.worker"].(string)
	if worker == "" {
		return fmt.Errorf("%s: key models.tiers.worker is missing or not a string", cfgPath)
	}
	models, err := loadConfig(configPath())
	if err != nil {
		return err
	}
	reviewer, err := reviewerModel(models, worker)
	if err != nil {
		return err
	}
	config["models.tiers.worker"] = reviewer.ID
	out, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(out))
	return nil
}

// runReviewReport implements `harness review-report PR` (issue #32), in the
// publish job in review mode: it reads the review run's result, parses the
// findings, decides what follows and prints exactly three output lines —
// action=fix|label|report-only, used=<continuations already used>,
// blocking=<blocking findings> — and writes the comment body to
// REVIEW_COMMENT_PATH for the workflow to post.
func runReviewReport() error {
	if len(os.Args) < 3 {
		return errors.New("usage: harness review-report <PR>")
	}
	pr, ok := dispatchIssue(os.Args[2])
	if !ok {
		return fmt.Errorf("review-report: PR number %q is not a positive number of digits", os.Args[2])
	}
	for _, name := range []string{"RESULT_PATH", "LEDGER_PATH", "REVIEW_COMMENT_PATH"} {
		if os.Getenv(name) == "" {
			return fmt.Errorf("%s is empty", name)
		}
	}
	result, haveResult := Result{}, false
	if r, _, err := readResult(); err == nil {
		result, haveResult = r, true
	}
	findings, sawSection := parseFindings(result.Answer)
	action, body, used, err := reviewPlan(newGitHub(), pr, result, haveResult, findings, sawSection)
	if err != nil {
		return err
	}
	fmt.Printf("action=%s\nused=%d\nblocking=%d\n", action, used, len(blockingFindings(findings)))
	path := os.Getenv("REVIEW_COMMENT_PATH")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(body), 0o644)
}
