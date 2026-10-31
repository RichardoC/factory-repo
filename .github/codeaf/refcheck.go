package main

// The weekly upstream-pin check (issue #35): a scheduled job compares the
// pin in .github/codeaf/codeaf-ref with upstream main and must decide
// between doing nothing, opening one queue-labelled issue asking CodeAF to
// move the pin, updating the issue it opened before, or closing it when
// upstream is back at the pin. This file is the decision; the workflow
// feeds it three files and acts on the one JSON object ref-check prints.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"
)

// ---------------------------------------------------------------- ref-check

const (
	// refPath is the pinned upstream commit: one full SHA, validated
	// before the workflow clones anything.
	refPath = ".github/codeaf/codeaf-ref"

	// upstreamRepo is the upstream the pin tracks.
	upstreamRepo = "Agent-Field/codeaf"

	// refUpdateLabel marks the one open issue the check owns; queuedLabel
	// puts it in the queue a `codeaf` run picks from.
	refUpdateLabel = "codeaf-ref-update"
	queuedLabel    = "codeaf-queued"

	// refIssueTitle titles the created and updated issues.
	refIssueTitle = "CodeAF: upstream has moved past the pin"

	// refHeadMarker starts the first body line, "Upstream head: <head>":
	// a later run matches that line against the new head to tell an issue
	// that already reports this head from one still on an older one.
	refHeadMarker = "Upstream head: "

	// refMaxCommits caps the commit list; when commits were left out the
	// body ends with "- ... and N more".
	refMaxCommits = 20

	// refCommitMaxLen caps one commit's summary, ~100 characters.
	refCommitMaxLen = 100
)

// validRef is a full lowercase commit SHA: what the pin file must hold.
var validRef = regexp.MustCompile(`^[0-9a-f]{40}$`)

// compareCommit is one commit of the compare API's commits array.
type compareCommit struct {
	SHA    string `json:"sha"`
	Commit struct {
		Message string `json:"message"`
	} `json:"commit"`
}

// compare is the bounded part of the compare API response that ref-check
// reads; unknown or missing extra fields are ignored. commits is ordered
// oldest first, so its last entry is the tip of upstream main — unless the
// API capped the array (see truncated), in which case that last entry is
// the newest of the listed commits, not upstream's tip.
type compare struct {
	Status  string          `json:"status"`
	AheadBy int             `json:"ahead_by"`
	Commits []compareCommit `json:"commits"`
}

// truncated reports whether the compare API capped the commits array. The
// API lists at most 250 commits per compare (issue #324), so when upstream
// is further ahead than that the array stops short of the tip while
// ahead_by still carries the true count.
func (c compare) truncated() bool {
	return len(c.Commits) > 0 && len(c.Commits) < c.AheadBy
}

// cappedHead is the head marker a capped compare records instead of a sha:
// it names the listing the decision was made from, "capped (250 of 300
// commits listed)". The last listed sha is not upstream's tip then, and
// recording it would make the next run match it again and never update the
// issue; this marker moves as soon as ahead_by does, and it is not a sha a
// later run can mistake for the tip.
func cappedHead(listed, ahead int) string {
	return fmt.Sprintf("capped (%d of %d commits listed)", listed, ahead)
}

// head returns the upstream head the decision keys off: the sha of the last
// commit since the pin, or the pin itself when the compare lists none,
// which is what an identical comparison looks like. A capped listing has no
// tip in it, so it returns cappedHead's marker instead of a sha. A compare
// whose listing is complete must carry a well-formed sha for its tip: the
// head goes into the issue marker line, and a corrupt one could never be
// matched again.
func (c compare) head(pin string) (string, error) {
	if len(c.Commits) == 0 {
		return pin, nil
	}
	if c.truncated() {
		return cappedHead(len(c.Commits), c.AheadBy), nil
	}
	head := c.Commits[len(c.Commits)-1].SHA
	if !validRef.MatchString(head) {
		return "", fmt.Errorf("compare: tip sha %q is not a 40-character lowercase commit SHA", head)
	}
	return head, nil
}

// decision is what ref-check prints, exactly these keys: what the weekly
// workflow should do and, for create and update, the title, body and
// labels of the issue to write.
type decision struct {
	Action      string   `json:"action"`
	Reason      string   `json:"reason"`
	Pin         string   `json:"pin"`
	Head        string   `json:"head"`
	IssueNumber int      `json:"issue_number"` // 0 unless the action is update or close
	Title       string   `json:"title"`
	Body        string   `json:"body"`
	Labels      []string `json:"labels"`
}

// refIssue returns the first open issue labelled codeaf-ref-update, or nil:
// the check owns one issue at a time.
func refIssue(issues []item) *item {
	for i := range issues {
		if issues[i].labelled(refUpdateLabel) {
			return &issues[i]
		}
	}
	return nil
}

// issueAtHead reports whether the issue body already records head in its
// marker line, "Upstream head: <head>". An issue that does is left alone:
// updating it again would only spam comments.
func issueAtHead(it item, head string) bool {
	marker := refHeadMarker + head
	for _, line := range strings.Split(it.Body, "\n") {
		if strings.TrimSpace(line) == marker {
			return true
		}
	}
	return false
}

// refCommitLine renders one entry of the commit list: the 7-character
// short sha, then the first line of the message, trimmed and capped at
// refCommitMaxLen characters.
func refCommitLine(c compareCommit) string {
	sha := c.SHA
	if len(sha) > 7 {
		sha = sha[:7]
	}
	msg := strings.TrimSpace(strings.SplitN(c.Commit.Message, "\n", 2)[0])
	if utf8.RuneCountInString(msg) > refCommitMaxLen {
		msg = string([]rune(msg)[:refCommitMaxLen-3]) + "..."
	}
	// Upstream's text: escaped, so it can't @-mention anyone or add markup.
	return fmt.Sprintf("- `%s` %s", sha, escapeMD(msg, refCommitMaxLen, true))
}

// refIssueBody is the issue body of a create and an update. The first line
// is the marker "Upstream head: <head>", then the pin, the newest
// refMaxCommits commits since the pin, and what CodeAF should do about
// them. When the compare API capped its listing the body says so, because
// the head is then a capped-listing marker and the oldest commits since the
// pin are not in the response at all.
func refIssueBody(pin, head string, c compare) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s%s\n", refHeadMarker, head)
	fmt.Fprintf(&b, "Pin: %s\n", pin)
	b.WriteString("\n## Commits since the pin\n\n")
	shown := len(c.Commits)
	if shown > refMaxCommits {
		shown = refMaxCommits
	}
	// The newest commits are the ones the reader needs: slice from the end.
	for _, cm := range c.Commits[len(c.Commits)-shown:] {
		b.WriteString(refCommitLine(cm) + "\n")
	}
	if left := len(c.Commits) - shown; left > 0 {
		fmt.Fprintf(&b, "- ... and %d more\n", left)
	}
	if c.truncated() {
		fmt.Fprintf(&b, "\nThe compare API lists at most 250 commits and this compare lists %d of the %d commits since the pin, so the list above stops short of upstream's tip and this issue does not record one. The next run replaces this body when the listing moves.\n", len(c.Commits), c.AheadBy)
	}
	// The acceptance sections the queue's gate reads (issue #136): the
	// body is generated, so the sections are written here rather than
	// demanded of whoever opens the issue.
	fmt.Fprintf(&b, `
## Done when

- The workaround report names every disabled workaround that is no longer needed, with the upstream commit that makes each unnecessary, and the pin moves only after a test run of this repo's CodeAF workflow on a small task succeeds.

## End-to-end check

A codeaf-ref-update run on this issue completes with the module checks green and the pin file at the new head.

## What CodeAF should do

1. Read the upstream commits since the pin in %s.
2. Report which of this repo's documented workarounds they make unnecessary — currently the disabled escalation panel / CODEAF_MODELS workaround documented in the Run CodeAF step of .github/workflows/codeaf.yml (the router treats tool-call-only replies as empty, and its pinned rungs never escalate: internal/router/router.go).
3. Bump %s to the new head and remove only the workarounds proven unnecessary, and only if a test run of this repo's CodeAF workflow on a small task succeeds.
4. Note that changes to .github/workflows/ outside .github/codeaf/ need a maintainer's approval.
`, upstreamRepo, refPath)
	return b.String()
}

// RefCheck decides what the weekly upstream-pin check should do. pin is the
// validated pin from refPath, c the bounded compare response, issues the
// repo's open issues. The identical branch — head == pin, status
// "identical", or nothing ahead — closes the open ref-update issue when
// there is one and does nothing otherwise. The moved branch — commits past
// the pin, or status "diverged" — creates the issue when there is none,
// leaves alone one already at this head, and updates one still on an older
// head. The reason is one line, for the workflow log. A compare the API
// capped (issue #324) has no tip to record: its head is a capped-listing
// marker, which moves as soon as ahead_by does, so a later run never
// mistakes a stale capped listing for upstream's tip.
func RefCheck(pin string, c compare, issues []item) (decision, error) {
	head, err := c.head(pin)
	if err != nil {
		return decision{}, err
	}
	labels := []string{refUpdateLabel, queuedLabel}
	identical := head == pin || c.Status == "identical" ||
		(c.AheadBy == 0 && c.Status != "diverged")
	if identical {
		if it := refIssue(issues); it != nil {
			return decision{
				Action:      "close",
				Reason:      "upstream is at the pin; closing the open ref-update issue",
				Pin:         pin,
				Head:        head,
				IssueNumber: it.Number,
				Labels:      []string{},
			}, nil
		}
		return decision{Action: "none", Reason: "upstream is at the pin", Pin: pin, Head: head, Labels: []string{}}, nil
	}
	reason := fmt.Sprintf("upstream main is %d commits past the pin", c.AheadBy)
	if c.Status == "diverged" {
		reason = fmt.Sprintf("upstream main has diverged from the pin (%d commits ahead)", c.AheadBy)
	}
	if c.truncated() {
		// The head is a listing marker rather than a sha; say why.
		reason += fmt.Sprintf(", and the compare capped its listing at %d commits", len(c.Commits))
	}
	body := refIssueBody(pin, head, c)
	it := refIssue(issues)
	switch {
	case it == nil:
		return decision{Action: "create", Reason: reason, Pin: pin, Head: head, Title: refIssueTitle, Body: body, Labels: labels}, nil
	case issueAtHead(*it, head):
		return decision{
			Action: "none",
			Reason: fmt.Sprintf("#%d already reports this upstream head", it.Number),
			Pin:    pin, Head: head, Labels: []string{},
		}, nil
	default:
		return decision{Action: "update", Reason: reason, Pin: pin, Head: head, IssueNumber: it.Number, Title: refIssueTitle, Body: body, Labels: labels}, nil
	}
}

// readPin reads the pin file: surrounding whitespace is trimmed, and what
// remains must be a 40-character lowercase commit SHA.
func readPin(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	pin := strings.TrimSpace(string(data))
	if !validRef.MatchString(pin) {
		return "", fmt.Errorf("%s: %q is not a 40-character lowercase commit SHA", path, pin)
	}
	return pin, nil
}

// readCompare reads the compare API response of upstreamRepo
// <pin>...main, as `gh api repos/Agent-Field/codeaf/compare/$PIN...main`
// prints it. Only the bounded fields the decision reads are kept.
func readCompare(path string) (compare, error) {
	var c compare
	data, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// runRefCheck implements
// `harness ref-check <pin-file> <compare-json> <open-issues-json>`: it
// prints one JSON object — the decision — on stdout and exits 0. Any error
// returns before anything is printed; main turns it into a stderr message
// and a non-zero exit. The weekly job acts on the decision: create, update
// or close the ref-update issue, or nothing.
func runRefCheck() error {
	if len(os.Args) != 5 {
		return errors.New("usage: harness ref-check <pin-file> <compare-json> <open-issues-json>")
	}
	pin, err := readPin(os.Args[2])
	if err != nil {
		return err
	}
	c, err := readCompare(os.Args[3])
	if err != nil {
		return err
	}
	issues, err := readItems(os.Args[4])
	if err != nil {
		return err
	}
	d, err := RefCheck(pin, c, issues)
	if err != nil {
		return err
	}
	out, err := json.Marshal(d)
	if err != nil {
		return err
	}
	fmt.Println(string(out))
	return nil
}
