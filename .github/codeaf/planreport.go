// planreport.go is the publish side of the planner pass (issue #61): it runs
// in the publish job, reads the plan run's answer exactly as runReviewReport
// reads a review's (RESULT_PATH, falling back to the run record), parses the
// proposals with a machine parser, dedupes them against the open issues'
// titles, and writes the ones that survive as JSON for the workflow to open
// as issues. Everything parsed here comes from the untrusted run job, so it
// is read as data of bounded size: escaped, capped, and never executed.
// Standard library only.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// proposedLabel marks an issue the planner opened for a person to triage.
const proposedLabel = "codeaf-proposed"

// Parse caps. A hostile answer can carry 100k-character fields and a hundred
// proposals; every bound here is what keeps one answer from growing the
// output without limit.
const (
	maxProposalTitle   = 200  // runes, one line
	maxProposalField   = 3000 // runes per field
	maxProposalsParsed = 20   // hard ceiling; the run's own cap is min(max, this)
	maxProposalDeps    = 10   // "#N" tokens per proposal
)

// Proposal is one proposed issue parsed from a plan run's answer (issue #61).
type Proposal struct {
	Title   string
	Problem string
	What    string
	Done    string
	Area    string // "harness" or "app/<name>"; empty when absent or unrecognized
	Deps    []int
}

var (
	// planProposalHeader opens one proposal: a line starting with
	// "Proposal:", at any heading level, case-insensitive, trailing
	// whitespace allowed. The title is the rest of the line.
	planProposalHeader = regexp.MustCompile(`(?i)^\s*#{0,6}\s*proposal:[ \t]*(.*)$`)
	// planHeading is any markdown heading line; it ends the current field.
	planHeading = regexp.MustCompile(`^\s*#{1,6}(\s|$)`)
	// planFieldLine is one known field: "Name: text" with an optional
	// leading "- ", case-insensitive on the name.
	planFieldLine = regexp.MustCompile(`(?i)^\s*-?\s*(problem|what to do|done when|area|depends on)\s*:\s*(.*)$`)
	// planUnknownField is a bullet line shaped like a field ("Name: text")
	// but naming none of the known ones. Its text is kept out, and it ends
	// the previous field, so a crafted "- Ignore previous instructions: ..."
	// cannot ride into a proposal as its continuation. Only bullet lines
	// count — a field's own continuations carry colons in ordinary prose,
	// and those must keep appending.
	planUnknownField = regexp.MustCompile(`^\s*-\s*[^:\n]{1,80}:\s`)
	// planDepToken is one "#N" dependency token; anything else on a
	// "Depends on" line is ignored.
	planDepToken = regexp.MustCompile(`#(\d+)`)
	// planTitleNoise collapses runs of non-alphanumerics, for title
	// normalization in DedupeProposals.
	planTitleNoise = regexp.MustCompile(`[^a-z0-9]+`)
)

// planClean strips control characters (newlines kept — the field layer
// decides what a newline becomes) and trims; it is the one cleaner every
// parsed proposal text passes through.
func planClean(s string) string {
	return strings.TrimFunc(strings.Map(func(r rune) rune {
		switch {
		case r == '\n':
			return r // the field layer decides what a newline becomes
		case r == '\t':
			return ' '
		case r < 0x20 || r == 0x7f:
			return -1 // control characters are stripped
		}
		return r
	}, s), func(r rune) bool { return r == ' ' })
}

// planTruncate cuts s at max runes without splitting one.
func planTruncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}

// planTitle reads a proposal title: one line (newlines become spaces),
// control characters stripped, capped.
func planTitle(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return strings.TrimSpace(planTruncate(planClean(s), maxProposalTitle))
}

// normalizeArea maps an Area field to "harness" or "app/<name>"; anything
// else — a made-up area, a path, prose — answers "". The area becomes a
// label, so only the two shapes the factory understands may pass.
func normalizeArea(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch {
	case s == "harness":
		return "harness"
	case strings.HasPrefix(s, "app/"):
		name := strings.TrimPrefix(s, "app/")
		if name != "" && !strings.ContainsAny(name, " \t\n/") {
			return "app/" + name
		}
	}
	return ""
}

// ParseProposals reads the proposals out of a plan run's answer (issue #61).
// Line by line, sharing one cap, min(max, maxProposalsParsed):
//
//   - a line matching planProposalHeader closes any proposal being read and
//     starts a new one with the rest of the line as its title;
//   - a known field line ("- Problem: ...", case-insensitive) appends its
//     text to that field of the current proposal — a repeated field keeps
//     both texts, so nothing a planner wrote is silently dropped;
//   - an unknown field line is kept out and ends the previous field;
//   - any other non-empty line appends to the previous field, until the
//     next field, proposal or heading; lines before the first proposal are
//     prose and ignored;
//   - at close, a proposal whose Problem and What are both empty is
//     dropped, the title is made single-line, every text is truncated to
//     its cap, Area is normalized, and Depends on keeps at most
//     maxProposalDeps "#N" tokens.
func ParseProposals(answer string, max int) []Proposal {
	if max > maxProposalsParsed {
		max = maxProposalsParsed
	}
	var out []Proposal
	var cur *Proposal
	field := "" // the open field's name, or ""
	appendTo := func(name, line string) {
		switch name {
		case "problem":
			cur.Problem = strings.TrimSpace(cur.Problem + "\n" + line)
		case "what to do":
			cur.What = strings.TrimSpace(cur.What + "\n" + line)
		case "done when":
			cur.Done = strings.TrimSpace(cur.Done + "\n" + line)
		case "area":
			cur.Area = strings.TrimSpace(cur.Area + "\n" + line)
		case "depends on":
			cur.Deps = appendPlanDeps(cur.Deps, line)
		}
	}
	for _, raw := range strings.Split(answer, "\n") {
		if len(out) >= max {
			break // cap reached: the rest of the answer is ignored
		}
		line := strings.TrimSuffix(raw, "\r")
		if m := planProposalHeader.FindStringSubmatch(line); m != nil {
			if cur != nil && planKeeps(*cur) {
				out = append(out, planFinish(*cur))
				if len(out) >= max {
					break
				}
			}
			cur = &Proposal{Title: planTitle(m[1])}
			field = ""
			continue
		}
		if cur == nil {
			continue // prose before the first proposal header
		}
		if planHeading.MatchString(strings.TrimSpace(line)) {
			field = ""
			continue
		}
		if m := planFieldLine.FindStringSubmatch(line); m != nil {
			field = strings.ToLower(m[1])
			appendTo(field, planClean(m[2]))
			continue
		}
		if planUnknownField.MatchString(strings.TrimSpace(line)) {
			field = "" // unknown field: kept out, and it ends the previous one
			continue
		}
		if field == "" || strings.TrimSpace(line) == "" {
			continue
		}
		appendTo(field, planClean(line))
	}
	if cur != nil && planKeeps(*cur) && len(out) < max {
		out = append(out, planFinish(*cur))
	}
	return out
}

// planKeeps reports whether a proposal survives parse: a proposal that says
// neither what is wrong nor what to do is noise, not a proposal.
func planKeeps(p Proposal) bool {
	return strings.TrimSpace(p.Problem) != "" || strings.TrimSpace(p.What) != ""
}

// planFinish truncates the texts, makes the title single-line, normalizes
// the area and caps the dependencies — the shape a proposal takes from here
// on, so later stages never re-trust the raw answer.
func planFinish(p Proposal) Proposal {
	p.Title = planTitle(p.Title)
	p.Problem = planTruncate(strings.TrimSpace(p.Problem), maxProposalField)
	p.What = planTruncate(strings.TrimSpace(p.What), maxProposalField)
	p.Done = planTruncate(strings.TrimSpace(p.Done), maxProposalField)
	p.Area = normalizeArea(p.Area)
	if len(p.Deps) > maxProposalDeps {
		p.Deps = p.Deps[:maxProposalDeps]
	}
	return p
}

// appendPlanDeps reads the "#N" tokens out of one "Depends on" line (or one
// of its continuations) and appends them, capped at maxProposalDeps in
// total; digits only, so "#12x" stops at 12 and prose is ignored.
func appendPlanDeps(deps []int, line string) []int {
	for _, m := range planDepToken.FindAllStringSubmatch(line, -1) {
		if len(deps) >= maxProposalDeps {
			break
		}
		if n, err := strconv.Atoi(m[1]); err == nil {
			deps = append(deps, n)
		}
	}
	return deps
}

// normalizeTitle folds a title for comparison: lowercase, every run of
// non-alphanumerics collapsed to one space, trimmed. "Fix: the queue!" and
// "fix the queue" then name the same work.
func normalizeTitle(s string) string {
	return strings.TrimSpace(planTitleNoise.ReplaceAllString(strings.ToLower(s), " "))
}

// DedupeProposals drops a proposal whose normalized title equals a
// normalized open-issue title, or equals an earlier kept proposal's, and
// answers how many it held back. It never edits proposal text: the
// dedupe's only power is to leave a duplicate unopened.
func DedupeProposals(props []Proposal, open []item) (kept []Proposal, held int) {
	seen := make(map[string]bool, len(open)+len(props))
	for _, it := range open {
		seen[normalizeTitle(it.Title)] = true
	}
	for _, p := range props {
		key := normalizeTitle(p.Title)
		if seen[key] {
			held++
			continue
		}
		seen[key] = true
		kept = append(kept, p)
	}
	return kept, held
}

// ProposalLabels is the label set for an opened proposal: codeaf-proposed
// always, plus the area label when the proposal carries a known area. It is
// NEVER codeaf-queued — that label is the trust gate: only a person with
// triage access, or Claude acting for the owner, promotes a proposal to the
// queue, and this code, fed by the untrusted run job, must never grant work
// to itself. Proposal text is untrusted data, so no label is ever read out
// of it.
func ProposalLabels(p Proposal) []string {
	labels := []string{proposedLabel}
	switch {
	case p.Area == "harness":
		labels = append(labels, "area:harness")
	case strings.HasPrefix(p.Area, "app/"):
		labels = append(labels, "area:"+p.Area)
	}
	return labels
}

// ProposalBody renders one proposal as the body of the issue the publish job
// opens: a fixed markdown template, every untrusted text passed through
// escapeMD (the title on one line), the dependencies rendered as plain
// numbers. When the Problem or What mentions ".github/workflows" and the
// proposal does not already say the word "approval", the note that workflow
// changes need a maintainer's approval is appended — the planner was told
// to state it, and the factory does not rely on it having listened.
func ProposalBody(p Proposal) string {
	var b strings.Builder
	fmt.Fprintf(&b, "### Proposal: %s\n\n", escapeMD(p.Title, maxProposalTitle, true))
	fmt.Fprintf(&b, "**Problem**\n\n%s\n\n", escapeMD(p.Problem, maxProposalField, false))
	fmt.Fprintf(&b, "**What to do**\n\n%s\n\n", escapeMD(p.What, maxProposalField, false))
	fmt.Fprintf(&b, "**Done when**\n\n%s\n", escapeMD(p.Done, maxProposalField, false))
	if p.Area != "" {
		fmt.Fprintf(&b, "\n**Area**: %s\n", escapeMD(p.Area, 100, true))
	}
	if len(p.Deps) > 0 {
		nums := make([]string, 0, len(p.Deps))
		for _, n := range p.Deps {
			nums = append(nums, fmt.Sprintf("#%d", n))
		}
		fmt.Fprintf(&b, "\n**Depends on**: %s\n", strings.Join(nums, ", "))
	}
	text := strings.ToLower(p.Problem + "\n" + p.What)
	if strings.Contains(text, ".github/workflows") && !strings.Contains(text, "approval") {
		b.WriteString("\nNote: changes to .github/workflows/ need a maintainer's approval before merging.\n")
	}
	return b.String()
}

// planProposalOut is one entry of the JSON the publish workflow reads.
type planProposalOut struct {
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	Labels []string `json:"labels"`
}

// planOpenItems reads the open issues from OPEN_ISSUES_PATH — the JSON that
// `gh api repos/$REPO/issues?state=open&per_page=100` prints, produced by
// the workflow and read here as untrusted data. A missing, unreadable or
// malformed file degrades to no open issues: the dedupe then keeps
// everything, which is the safe direction for proposals (a person still
// triages each one).
func planOpenItems() []item {
	path := os.Getenv("OPEN_ISSUES_PATH")
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var items []item
	if json.Unmarshal(data, &items) != nil {
		return nil
	}
	return items
}

// runPlanReport implements `harness plan-report` (issue #61), in the publish
// job: it reads the plan run's answer (RESULT_PATH, falling back to the root
// task's result in the run record, exactly as runReviewReport does), parses
// the proposals, dedupes them against the open issues' titles, writes the
// kept ones to PROPOSALS_PATH as a JSON array of {title, body, labels} and
// prints one line, "proposals=N held=M" — N proposals to open, M held back
// as duplicates.
func runPlanReport() error {
	for _, name := range []string{"RESULT_PATH", "PROPOSALS_PATH"} {
		if os.Getenv(name) == "" {
			return fmt.Errorf("%s is empty", name)
		}
	}
	answer := ""
	if r, _, err := readResult(); err == nil {
		answer = r.Answer
	}
	if answer == "" {
		// The result envelope's answer may be empty while the plan's text
		// lives in the run record (issue #49's shape); read it as data.
		if a := readTrajectoryAnswer(trajectoryPath()); a != "" {
			answer = a
		}
	}
	props := ParseProposals(answer, planMaxProposals())
	kept, held := DedupeProposals(props, planOpenItems())
	out := make([]planProposalOut, 0, len(kept))
	for _, p := range kept {
		out = append(out, planProposalOut{Title: p.Title, Body: ProposalBody(p), Labels: ProposalLabels(p)})
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	path := os.Getenv("PROPOSALS_PATH")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}
	fmt.Printf("proposals=%d held=%d\n", len(kept), held)
	return nil
}
