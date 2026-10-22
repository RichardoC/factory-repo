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
	"io/fs"
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
//
// A finding with no location — the inline form (issue #49) — has no file to
// name, so its location cell reads `unlocated` instead of the `:0` that a
// path:line of the empty path would otherwise render (issue #190): a fix run
// and a reader both read `:0` as a location, and it names no file.
func reviewTable(findings []Finding) string {
	var b strings.Builder
	b.WriteString("| severity | location | finding |\n|---|---|---|\n")
	for _, f := range findings {
		fmt.Fprintf(&b, "| %s | %s | %s |\n",
			tokenCell(f.Severity, 200), reviewLocation(f), escapeMD(f.Text, 200, true))
	}
	return b.String()
}

// reviewLocation renders one finding's location cell: `path:line` when the
// finding carries a location, and the fixed word `unlocated` when it does
// not, so no cell ever shows a location that names no file (issue #190).
// The path is escaped and capped like every other cell, so a hostile path
// cannot break out of the table or the comment.
//
// A finding is unlocated when its path is empty: the parser gives the
// inline form (issue #49) no path and no line, and every located finding it
// builds has both, so the line alone never decides.
func reviewLocation(f Finding) string {
	// The path decides, not the line: every finding the parser builds
	// without a location carries no path, so a line alone names no file.
	// Asking for a zero line too would let any caller that filled the line
	// in render a bare ":0" — the cell this issue exists to remove.
	if f.Path == "" {
		return "unlocated"
	}
	// A located cell is `path:line`; a zero line never renders, because a
	// line number counts from one. Two shapes reach here with line zero:
	// the straight-through path a test or a future caller may build, and
	// the location-shaped text the parser drops the number from — `:0`
	// itself parses as the path ":" (findingText accepts a `:` separator)
	// and `0:1` as the path "0" — both of which named no file either, so
	// they read `unlocated` rather than as a location (issue #190).
	// A path that is only separators and digits (":", "0") names no file
	// and reads `unlocated`; a real path with no line keeps its path, so a
	// file-level finding still says which file.
	if f.Line <= 0 {
		if strings.Trim(f.Path, ":0123456789") == "" {
			return "unlocated"
		}
		return escapeMD(f.Path, 200, true)
	}
	return escapeMD(f.Path, 200, true) + ":" + strconv.Itoa(f.Line)
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

// capDiffFloorBytes is the smallest body budget a file's section keeps when
// the cap forces allocation: every file in the diff contributes some
// content, even a many-file patch larger than the whole cap (issue #231).
const capDiffFloorBytes = 1024

// capDiffMaxNoteFiles bounds how many clipped file names the truncation
// note lists; past that the note says how many more there are, so a diff
// with thousands of files cannot grow the note without bound.
const capDiffMaxNoteFiles = 30

// diffSection is one file's slice of a unified diff: its `diff --git`
// header line (newline included) and the body up to the next header.
type diffSection struct {
	header string // the `diff --git a/x b/x` line, newline included
	body   string // everything up to the next `diff --git` line
	name   string // the file's path, from the header's b/ side
}

// splitDiffSections splits a unified diff at every `diff --git ` line. A
// diff with no such line yields no sections; text before the first header
// is kept as its own section with no name, so a leading run of context is
// never dropped silently either.
func splitDiffSections(diff string) []diffSection {
	start := 0
	if !strings.HasPrefix(diff, "diff --git ") {
		i := strings.Index(diff, "\ndiff --git ")
		if i < 0 {
			return nil
		}
		start = i + 1
	}
	var secs []diffSection
	for start < len(diff) {
		end := strings.Index(diff[start:], "\ndiff --git ")
		if end < 0 {
			end = len(diff)
		} else {
			end += start
		}
		sec := diff[start:end]
		hl := strings.IndexByte(sec, '\n')
		var s diffSection
		if hl < 0 {
			s.header = sec
		} else {
			s.header, s.body = sec[:hl+1], sec[hl+1:]
		}
		if strings.HasPrefix(s.header, "diff --git ") {
			// The header line reads `diff --git a/old b/new`; the b/ side
			// names the file. Quoted paths (spaces in the name) keep the
			// whole remainder, which still names the file.
			s.name = strings.TrimSuffix(s.header[len("diff --git "):], "\n")
			if i := strings.LastIndexByte(s.name, ' '); i >= 0 {
				bpath := s.name[i+1:]
				if strings.HasPrefix(bpath, "b/") {
					s.name = bpath[len("b/"):]
				}
			}
		}
		secs = append(secs, s)
		start = end + 1
	}
	return secs
}

// allocDiffShares fills share[i] with the body bytes section i may keep
// within budget: every file keeps a floor of content (scaled down when
// even the floors do not fit), a file whose whole body fits is kept whole,
// and the leftovers are redistributed to the files still short, largest
// first.
func allocDiffShares(secs []diffSection, budget int, share []int) {
	for i := range share {
		share[i] = 0
	}
	n := len(secs)
	if n == 0 {
		return
	}
	floor := capDiffFloorBytes
	if floor*n > budget {
		floor = budget / n
	}
	remaining := budget
	for i, s := range secs {
		k := min(len(s.body), floor)
		if k > remaining {
			k = max(remaining, 0)
		}
		share[i] = k
		remaining -= k
	}
	// Redistribution: the files still short split what is left equally,
	// largest first, until the budget runs out.
	short := make([]int, 0, n)
	for i, s := range secs {
		if share[i] < len(s.body) {
			short = append(short, i)
		}
	}
	sort.Slice(short, func(a, b int) bool { return secs[short[a]].body > secs[short[b]].body })
	for len(short) > 0 && remaining > 0 {
		per := remaining / len(short)
		if per == 0 {
			// Crumbs only: one byte to each of the largest files, in
			// order, until the budget is gone.
			for _, i := range short {
				if remaining == 0 {
					break
				}
				share[i]++
				remaining--
			}
			break
		}
		next := make([]int, 0, len(short))
		for _, i := range short {
			add := min(len(secs[i].body)-share[i], per)
			share[i] += add
			remaining -= add
			if share[i] < len(secs[i].body) {
				next = append(next, i)
			}
		}
		short = next
	}
}

// diffTruncationNote is what the capped diff ends with: the byte count the
// note always carried, plus the clipped files and the instruction to read
// them on the checked-out PR head branch (the maintainer's reduction of
// issue #231 — the review run has the checkout, so named files can be read
// even though the brief cannot carry them).
func diffTruncationNote(max int, clipped []string) string {
	note := fmt.Sprintf("(diff truncated at %d bytes)\n", max)
	if len(clipped) == 0 {
		return note
	}
	named := clipped
	extra := 0
	if len(named) > capDiffMaxNoteFiles {
		extra = len(named) - capDiffMaxNoteFiles
		named = named[:capDiffMaxNoteFiles]
	}
	note = fmt.Sprintf("(diff truncated at %d bytes; not all of every file is shown — read the full content of %s on the checked-out PR head branch",
		max, strings.Join(named, ", "))
	if extra > 0 {
		note += fmt.Sprintf(" and %d more", extra)
	}
	return note + ")\n"
}

// capDiff caps the diff embedded in the review brief. The cap is file-aware
// (issue #231): the diff is split into per-file sections, every `diff --git`
// header line is always kept, and the byte budget (the note included) is
// allocated across the files so each one contributes some content, the
// leftovers going to the largest files first. Sections whose body is cut
// short are named in the note. A diff too large even for its headers, or
// with no file headers at all, falls back to a plain prefix cap. The result
// never splits a UTF-8 rune and never exceeds max bytes.
func capDiff(diff string, max int) string {
	if len(diff) <= max {
		return diff
	}
	secs := splitDiffSections(diff)
	headerBytes := 0
	for _, s := range secs {
		headerBytes += len(s.header)
	}
	if len(secs) == 0 || headerBytes >= max {
		// No per-file sections to preserve — cap the text itself.
		return capDiffPrefix(diff, max)
	}
	share := make([]int, len(secs))
	contentBudget := max - headerBytes
	for round := 0; round < 16; round++ {
		allocDiffShares(secs, contentBudget, share)
		var b strings.Builder
		var clipped []string
		for i, s := range secs {
			cut := share[i]
			if cut > len(s.body) {
				cut = len(s.body) // a file short of its share is kept whole
			}
			// Back off to a rune boundary, never splitting one; a cut at
			// the end of the body has nothing to back off from.
			for cut > 0 && cut < len(s.body) && !utf8.RuneStart(s.body[cut]) {
				cut-- // never split a rune
			}
			b.WriteString(s.header)
			if cut < len(s.body) {
				b.WriteString(s.body[:cut])
				if s.name != "" {
					clipped = append(clipped, s.name)
				}
			} else {
				b.WriteString(s.body)
			}
		}
		out := b.String()
		note := diffTruncationNote(max, clipped)
		if len(out) > 0 && out[len(out)-1] != '\n' {
			out += "\n" // a clipped body may end mid-line
		}
		out += note
		if len(out) <= max {
			return out
		}
		// The note names more clipped files than this round's budget left
		// room for: shrink the content budget and re-allocate.
		contentBudget -= len(out) - max
		if contentBudget < 0 {
			break
		}
	}
	// Degenerate shapes (headers plus note past the cap): a bounded prefix
	// keeps the total within max, as before issue #231.
	return capDiffPrefix(diff, max)
}

// capDiffPrefix is the pre-#231 plain cap: a byte prefix with a note.
func capDiffPrefix(diff string, max int) string {
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

// reviewHarnessPreChangeSentence is the evidence rule review-instructions.md
// carries (issue #189): the `harness` on PATH predates the run, so a review
// of a .github/codeaf/ diff rebuilds from the diff's tree.
const reviewHarnessPreChangeSentence = "the `harness` binary on `PATH` was built from the default\nbranch before this run started"

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
			// Issue #139: the app rules travel with the issue, exactly as
			// runBrief embeds them for the worker.
			if rules := appRulesSections(gh, issueAreas(closes)); rules != "" {
				parts = append(parts, rules)
			}
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
	// The envelope's answer can be empty while the review's text lives in
	// the run record (issue #49), which the run job shipped beside this
	// result in every mode (issue #134); the record also tells whether the
	// run finished in the store, promoting an incomplete stop to done. Both
	// are read as untrusted data.
	c := readClosing(trajectoryPath())
	findings, sawSection := parseFindings(effectiveAnswer(result, c))
	if len(findings) == 0 {
		// The answer carried no findings (issue #49) — it may be empty, or
		// the review may have written them inline where the envelope never
		// looks. Fall back to the root task's final result in the run
		// record, which the run job shipped beside this result and which is
		// read as untrusted data.
		if answer := c.Answer; answer != "" {
			if tf, tsec := parseFindings(answer); len(tf) > 0 {
				findings, sawSection = tf, sawSection || tsec
			}
		}
	}
	result.Stop = effectiveStop(result, c)
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
	// filepath.Glob below stays silent on an unreadable directory: it
	// answers no matches for a permission error, so a runs dir the harness
	// cannot read — the agent user still owned CODEAF_HOME before the
	// workflow took the tree back (issue #62) — would ship no record and
	// say nothing. Check the directory directly so that failure is loud.
	runs := filepath.Join(home, "runs")
	if _, err := os.ReadDir(runs); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Printf("warning: %s is unreadable, the run record cannot ship: %v", runs, err)
	}
	records, err := filepath.Glob(filepath.Join(runs, "*", "tasks", "root", "trajectory.jsonl"))
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
