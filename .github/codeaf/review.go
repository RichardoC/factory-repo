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
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
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
	// chainedFinding opens the next finding after a `;` on the same line
	// (issue #111): optional whitespace, then a recognized severity in
	// brackets followed by the whitespace before its location or text. An
	// unrecognized word after a `;` is prose, not a boundary.
	chainedFinding = regexp.MustCompile(`(?i)^\s{0,16}\[(?:high|medium|low)\][ \t]`)
)

// parseFindings reads the findings out of a reviewer's answer (issues #32
// and #49). Two passes run over every line, sharing one cap:
//
//   - the section pass (issue #32): a markdown section whose header line
//     matches ^#{1,6}\s*findings\b case-insensitively, holding one bullet
//     per finding — [severity], a path, a line number, a separator and the
//     text; malformed bullets are ignored, and sawSection reports whether
//     the header was present at all;
//   - the inline pass (issues #49 and #111): findings written anywhere in
//     the answer as [severity] path:line - text — several to a line,
//     separated by `;` — or as [severity] text with no location, which only
//     counts for a recognized severity; the forms a review that never
//     reached the section still uses (run 39010363221 wrote
//     "Findings: [high] …" into its result and was read as a review with
//     no findings).
//
// The same finding found by both passes is kept once. Findings are capped at
// maxReviewFindings; text and path are never truncated at parse time — the
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
		if inSection {
			if f, ok := parseFinding(line); ok && !duplicateFinding(findings, f) {
				findings = append(findings, f)
			}
		}
		for _, f := range parseInlineFindings(line) {
			if !duplicateFinding(findings, f) {
				findings = append(findings, f)
			}
		}
		if len(findings) >= maxReviewFindings {
			return findings[:maxReviewFindings], sawSection // cap reached: ignore the rest
		}
	}
	return findings, sawSection
}

// inlineSeverityToken is a bracketed single word anywhere in a line, the
// shape every inline finding opens with. The word is short and holds no
// spaces or brackets, so a markdown link, a footnote or a task list cannot
// pass for one.
var inlineSeverityToken = regexp.MustCompile(`\[[A-Za-z0-9_-]{1,24}\]`)

// locationShaped matches text that opens like a path:line location. A token
// followed by such text was meant as a located finding; when the location
// never completes — no separator, no line digits — the line is dropped
// rather than read as a finding whose text is the half-written location.
var locationShaped = regexp.MustCompile(`^\s*\S+:\d+`)

// parseInlineFindings reads the findings written inline on one line (issues
// #49 and #111), in the forms the lenient contract accepts: [severity]
// path:line - text — the shape the section bullets use, with or without a
// bullet — or [severity] text with no location, which only counts when the
// severity word is one of high, medium and low. Several findings may share
// one line, separated by `;` (issue #111): the run that prompted it wrote
// "FINDINGS (3, 2 blocking): [medium] a.go:1 - x; [medium] b.go:2 - y; [low]
// c.go:3 - z" as one paragraph, and each finding must keep its own text.
// Every bracketed token on the line gets its chance.
func parseInlineFindings(line string) []Finding {
	var out []Finding
	for i := 0; i < len(line); {
		m := inlineSeverityToken.FindStringIndex(line[i:])
		if m == nil {
			break // no severity token anywhere in the rest of the line
		}
		token := line[i+m[0]+1 : i+m[1]-1]
		i += m[1] // every path consumes the token, whether it reads or not
		rest := line[i:]
		// A `;` that opens the next recognized severity ends this
		// finding (issue #111): the text before it is this finding's,
		// and the scan resumes after the `;`. An unrecognized word
		// after a `;` is not a boundary, so it stays in the text.
		segment, next := rest, -1
		if k := chainBoundary(rest); k >= 0 {
			segment, next = rest[:k], k+1
		}
		if f, ok := parseFinding("[" + token + "]" + segment); ok {
			out = append(out, f)
			i += next
			continue
		}
		if next >= 0 {
			// The chain still splits when the finding carries no
			// location: its text ends at the `;` too.
			if f, ok := textOnlyFinding(token, segment); ok {
				out = append(out, f)
			}
			i += next
			continue
		}
		if locationShaped.MatchString(rest) {
			continue // a broken location is dropped, never read as text
		}
		if f, ok := textOnlyFinding(token, rest); ok {
			out = append(out, f)
		}
	}
	return out
}

// chainBoundary reports the index in s of the `;` that separates this
// finding from the next one on the same line, or -1 when no such `;`
// follows. A boundary is only possible where a `[` sits within the
// chainedFinding pattern's reach of the `;`, so a line of plain prose
// with semicolons is scanned without a regex match per semicolon.
func chainBoundary(s string) int {
	for k := 0; k < len(s); k++ {
		if s[k] != ';' {
			continue
		}
		after := s[k+1:]
		if r := strings.IndexByte(after, '['); r < 0 || r > 16 {
			continue
		}
		if chainedFinding.MatchString(after) {
			return k
		}
	}
	return -1
}

// textOnlyFinding reads the no-location form: a recognized severity word,
// whitespace, and text that opens with a letter or digit — the guard that
// keeps bracketed notes like [TODO] fix me, a task list or a stray :42 from
// becoming findings.
func textOnlyFinding(token, rest string) (Finding, bool) {
	lower := asciiLower(token)
	if lower != "high" && lower != "medium" && lower != "low" {
		return Finding{}, false
	}
	if rest == "" || (rest[0] != ' ' && rest[0] != '\t') {
		return Finding{}, false // the text needs whitespace after the severity
	}
	text := strings.TrimLeft(rest, " \t")
	if text == "" {
		return Finding{}, false
	}
	if r, _ := utf8.DecodeRuneInString(text); !unicode.IsLetter(r) && !unicode.IsDigit(r) {
		return Finding{}, false
	}
	return Finding{Severity: token, Text: text, Recognized: true}, true
}

// duplicateFinding reports whether the same finding is already held: the
// severity read case-insensitively, and path, line and text exactly. The
// section pass and the inline pass read the same bullets, so the double is
// dropped and the finding is kept once.
func duplicateFinding(findings []Finding, f Finding) bool {
	for _, g := range findings {
		if asciiLower(g.Severity) == asciiLower(f.Severity) && g.Path == f.Path && g.Line == f.Line && g.Text == f.Text {
			return true
		}
	}
	return false
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

// reviewPlan decides what follows an independent review (issues #32 and #49):
//
//   - no result at all and no findings from either source: report-only —
//     nothing can be concluded;
//   - a result that is not stop "done": the review is incomplete, and an
//     incomplete review never labels — but findings parsed from the answer
//     or the run record still act (issue #49): blocking findings start a fix
//     run exactly as in a finished review. When findings exist the report
//     says the run ended early after finishing its review (issue #111);
//     only when neither source carries findings does it stay a plain
//     incomplete review, as it did before;
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
	stop := sanitizeStop(result.Stop)
	blocking := blockingFindings(findings)
	if !haveResult && len(findings) == 0 {
		return "report-only", reviewReportBody(
			"The review produced no result: its output is missing or unparseable, so nothing can be concluded from this run. "+
				"No label is added and no fix run starts; a maintainer can re-run the review.",
			nil, result.Model, stopUnknown), 0, nil
	}
	if !readyForReview(stop) && len(blocking) == 0 {
		// Blocking findings start a fix run even on an incomplete review
		// (issue #49); what is left here is a review whose findings are
		// advisory, or none. Findings in the answer or the closing result
		// mean the review finished its work and only the run's stop fell
		// short (issue #111): the report lists them as the review's
		// findings, never as "no findings found" — but the pass label
		// still waits for a done stop.
		if len(findings) > 0 {
			verdict := fmt.Sprintf("The run stopped with reason `%s` after finishing its review: "+
				"the findings below were parsed from its answer or its closing result. "+
				"The run did not end `done`, so no label is added and no fix run starts.", stop)
			return "report-only", reviewReportBody(verdict, findings, result.Model, stop), 0, nil
		}
		verdict := fmt.Sprintf("The review stopped early (stop reason: `%s`), so this is an incomplete review: "+
			"no findings section and no inline findings were found in the answer or the run record, "+
			"nothing is labelled and no fix run starts.", stop)
		return "report-only", reviewReportBody(verdict, findings, result.Model, stop), 0, nil
	}
	if readyForReview(stop) && len(blocking) == 0 {
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
	// Blocking findings start a fix run, whether the review finished (issue
	// #32) or stopped early (issue #49): an incomplete review with findings
	// is a review with findings, not one with none.
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
// the command line. A finding with no location — the inline form (issue #49)
// — carries its severity word alone.
func reviewFixCommand(blocking []Finding) string {
	parts := make([]string, 0, len(blocking))
	for _, f := range blocking {
		if f.Path == "" && f.Line == 0 {
			parts = append(parts, asciiLower(f.Severity))
			continue
		}
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
	"- [high] app/zip/queue.go:42 - the mutex is released twice under the error path, so a second request deadlocks\n" +
	"- [low] the changelog does not mention the new flag\n\n" +
	"Severity is high, medium or low. A finding without a file and line is still one bullet. " +
	"When the review is clean, end with the section holding the single word `none` and nothing else:\n\n" +
	"## Findings\n\nnone\n"

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
	if len(findings) == 0 {
		// The answer carried no findings (issue #49) — it may be empty, or
		// the review may have written them inline where the envelope never
		// looks. Fall back to the root task's final result in the run
		// record, which the run job shipped beside this result and which is
		// read as untrusted data.
		if answer := readTrajectoryAnswer(trajectoryPath()); answer != "" {
			if tf, tsec := parseFindings(answer); len(tf) > 0 {
				findings, sawSection = tf, sawSection || tsec
			}
		}
	}
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

// ---------------------------------------------------------------- the run record

// maxTrajectoryBytes caps the root task's run record, both when the run job
// ships it and when the report reads it: the record is written by the
// untrusted run job, so it is data of bounded size, never instructions.
const maxTrajectoryBytes = 2 << 20

// trajectoryFileName is the harness's name for the copy of the root task's
// run record that sits beside the result in the output artifact.
const trajectoryFileName = "codeaf-trajectory.jsonl"

// trajectoryPath names the run record the report falls back to (issue #49):
// TRAJECTORY_PATH when set, else the copy the run job's summary made beside
// the result.
func trajectoryPath() string {
	if p := os.Getenv("TRAJECTORY_PATH"); p != "" {
		return p
	}
	return filepath.Join(filepath.Dir(os.Getenv("RESULT_PATH")), trajectoryFileName)
}

// readTrajectoryAnswer reads the root task's final result out of a run
// record (issues #49 and #111): the last `end` line's result — the account
// the worker gave when its loop ended, which is where a review's actual
// text lives when the result envelope's answer is empty. The end line is
// the record's last, so the read is capped from the tail, not the head:
// the 36-step review of run 39062136141 pushed its end record past a
// head-capped read, and the fallback then answered "". The record comes
// from the untrusted run job, so it is read as data: capped, one bounded
// struct per line, malformed lines skipped — a line the cap cuts in half
// is just malformed — and only the result string is taken. A record that
// cannot be read answers "".
func readTrajectoryAnswer(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	data, err := readRecordTail(file, maxTrajectoryBytes)
	if err != nil {
		return ""
	}
	answer := ""
	for _, line := range strings.Split(string(data), "\n") {
		// The line kinds and the end line's result are CodeAF's own record
		// shape (internal/run/trajectory.go at the pin in codeaf-ref).
		var record struct {
			Kind   string `json:"kind"`
			Result string `json:"result"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			continue // hostile or truncated line: skipped, never fatal
		}
		if record.Kind == "end" && record.Result != "" {
			answer = record.Result // the last end line is the final result
		}
	}
	return answer
}

// readRecordTail reads at most max bytes from the end of an open file
// (issue #111): the root task's end record is the last line of its run
// record, so a bounded read must start from the tail or a long run's
// ending is cut off. Files at or under max are read whole; a longer file
// gives its last max bytes, even if the first of them lands mid-line —
// the half line is malformed to the reader and skipped.
func readRecordTail(file *os.File, max int64) ([]byte, error) {
	size, err := file.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	if size <= max {
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		return io.ReadAll(io.LimitReader(file, max))
	}
	if _, err := file.Seek(-max, io.SeekEnd); err != nil {
		return nil, err
	}
	buf := make([]byte, max)
	n, err := io.ReadFull(file, buf)
	if err != nil && err != io.ErrUnexpectedEOF {
		return nil, err
	}
	return buf[:n], nil
}

// shipRunRecord copies the newest root task run record in CODEAF_HOME next to
// the result (issue #49): the review report runs in the publish job, which
// reads only this job's output artifact, so the record must ride it for the
// fallback to reach the report. It runs in the run job, on files the agent
// could have written: only regular files are taken, the copy holds the
// record's tail, capped like the report's read (issue #111) so the end
// record survives the copy of a long run, and the report reads the copy as
// data. Nothing here is fatal — the record is a fallback — so trouble is
// logged and answered with "".
func shipRunRecord() string {
	home, out := os.Getenv("CODEAF_HOME"), filepath.Dir(os.Getenv("RESULT_PATH"))
	if home == "" || out == "" {
		return ""
	}
	records, err := filepath.Glob(filepath.Join(home, "runs", "*", "tasks", "root", "trajectory.jsonl"))
	if err != nil || len(records) == 0 {
		return ""
	}
	sort.Slice(records, func(a, b int) bool { // newest first: the run just finished
		sa, era := os.Stat(records[a])
		sb, erb := os.Stat(records[b])
		if era != nil || erb != nil {
			return era == nil
		}
		return sa.ModTime().After(sb.ModTime())
	})
	for _, record := range records {
		if s, err := os.Lstat(record); err != nil || !s.Mode().IsRegular() {
			continue // a planted symlink or directory is not a record
		}
		file, err := os.Open(record)
		if err != nil {
			log.Printf("warning: %s is unreadable: %v", record, err)
			continue
		}
		data, err := readRecordTail(file, maxTrajectoryBytes)
		file.Close()
		if err != nil {
			log.Printf("warning: %s is unreadable: %v", record, err)
			continue
		}
		if err := os.WriteFile(filepath.Join(out, trajectoryFileName), data, 0o644); err != nil {
			log.Printf("warning: the run record could not be shipped: %v", err)
			return ""
		}
		return record
	}
	return ""
}
