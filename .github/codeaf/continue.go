// continue.go supports runs that stop before finishing (issue #31): a run
// that ends early on a pull request is continued automatically, up to a cap,
// and a run that ends with stop "done" marks the pull request ready for
// review. Everything here is standard library only.
package main

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The stop words CodeAF can report, verified in Agent-Field/codeaf
// envelope.go at the pin recorded in .github/codeaf/codeaf-ref
// (6e73ac7, main, 2026-10-15).
const (
	stopDone       = "done"
	stopError      = "error"
	stopIncomplete = "incomplete"
	stopBudget     = "budget"
	stopTurnCap    = "turn-cap"
	stopDeadline   = "deadline"
	stopPrice      = "price"
	stopQuestion   = "question"
	stopUnchecked  = "unchecked"

	// stopUnknown stands in for a stop reason that is missing, unparseable
	// or not one of the words above. It contains no letters, so it can
	// never collide with a real stop word.
	stopUnknown = "(unknown)"
)

// knownStops is every word CodeAF's envelope reports as stop.
var knownStops = map[string]bool{
	stopDone:       true,
	stopError:      true,
	stopIncomplete: true,
	stopBudget:     true,
	stopTurnCap:    true,
	stopDeadline:   true,
	stopPrice:      true,
	stopQuestion:   true,
	stopUnchecked:  true,
}

// sanitizeStop maps a stop reason from CodeAF's result onto a word that is
// safe to print, append to $GITHUB_OUTPUT and embed in a comment: a known
// word passes through unchanged, and anything else — empty, unknown,
// hostile — becomes stopUnknown. A known word is lowercase letters and
// hyphens only, so no newline, quote or shell character can pass through.
func sanitizeStop(word string) string {
	if knownStops[word] {
		return word
	}
	return stopUnknown
}

// continuesAfter reports whether a run that stopped with this reason should
// be continued automatically (issue #31): "deadline" and "incomplete" are
// named there, and budget, turn-cap and price are the same family — a limit
// the run's own configuration set, not a verdict on the work. done needs no
// continuation, and error, question and unchecked need a person's input
// before more machine time is spent on them.
func continuesAfter(stop string) bool {
	switch stop {
	case stopDeadline, stopIncomplete, stopBudget, stopTurnCap, stopPrice:
		return true
	}
	return false
}

// readyForReview reports whether a run that ended with this stop reason has
// finished its work: only "done" marks the pull request ready for review
// (issue #31). earlyStop is its negation, named for how the report reads.
func readyForReview(stop string) bool { return stop == stopDone }

func earlyStop(stop string) bool { return !readyForReview(stop) }

// maxContinuations caps how many times runs may continue each other on one
// pull request (issue #31), so a task that can never finish cannot loop and
// spend.
const maxContinuations = 2

// continuationsUsed counts how many continuations a pull request has already
// had, given how many run-report comments it carries: the first report is
// the run that opened the pull request, not a continuation, so one report
// means zero continuations used.
func continuationsUsed(reportComments int) int {
	if reportComments < 1 {
		return 0
	}
	return reportComments - 1
}

// continuationCapReached reports whether the cap on automatic continuations
// is used up.
func continuationCapReached(used int) bool { return used >= maxContinuations }

// countReports counts the comments on a pull request that carry a CodeAF
// run report. Ordinary comments, including the continue and cap comments
// this file posts, carry no report marker and are not counted.
func countReports(comments []issueComment) int {
	n := 0
	for _, c := range comments {
		if strings.Contains(c.Body, reportMarker) {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------- pr-mode

// prMode implements `harness pr-mode`: it reads the run's result and prints
// the draft/ready transition as step outputs. Exactly three lines are
// printed, safe to append to $GITHUB_OUTPUT and to read in bash: a missing
// or unparseable result is an early stop whose reason is unknown, so a run
// that died before writing one still keeps the pull request a draft.
func prMode() error {
	stop := stopUnknown
	if result, _, err := readResult(); err == nil {
		stop = sanitizeStop(result.Stop)
	}
	early := earlyStop(stop)
	fmt.Printf("early=%t\nready=%t\nstop=%s\n", early, readyForReview(stop), stop)

	path := os.Getenv("PR_BODY_EXTRA_PATH")
	if path == "" {
		return nil // no path configured: skip the file, never fail the workflow
	}
	if !early {
		// A finished run needs no early-stop note; drop a stale one.
		// Best effort: a missing file is the normal case, and cleanup
		// must not fail the workflow.
		_ = os.Remove(path)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	body := fmt.Sprintf("This CodeAF run stopped early (stop reason: `%s`); the work on this branch is unverified and may be incomplete. Review it as work in progress.\n", stop)
	return os.WriteFile(path, []byte(body), 0o644)
}

// ---------------------------------------------------------------- the plan

// continuePlan decides what a pull request needs after a run stopped early.
// The action is "continue" while the cap allows it, "cap" once it is used
// up, and "" for everything that needs no comment: a finished run, an early
// stop that must not be continued, or a missing result. The returned body is
// built only from fixed sentences, the sanitized stop word and counters —
// never from remote text — so it is safe to post as a comment.
func continuePlan(gh github, pr int, stop string) (action string, body string, used int, err error) {
	stop = sanitizeStop(stop)
	if !earlyStop(stop) || !continuesAfter(stop) {
		return "", "", 0, nil
	}
	var comments []issueComment
	if err := gh.get(fmt.Sprintf("/issues/%d/comments?per_page=100", pr), &comments); err != nil {
		return "", "", 0, err
	}
	used = continuationsUsed(countReports(comments))
	if continuationCapReached(used) {
		return "cap", capBody(stop), used, nil
	}
	return "continue", continueBody(stop, used), used, nil
}

// continueBody is the comment that starts the next run: its first line is
// the command the workflow acts on, the rest is fixed text naming which
// continuation this is.
func continueBody(stop string, used int) string {
	return fmt.Sprintf("/codeaf continue: %s\n\n"+
		"This run continues the previous one on this branch; the brief includes the previous report and what the issue still requires. "+
		"Continuation %d of %d.\n", stop, used+1, maxContinuations)
}

// capBody is the comment posted when the continuation cap is reached: the
// pull request stays a draft for a person, who can still continue it by hand.
func capBody(stop string) string {
	return fmt.Sprintf("The continuation cap (%d per issue) is reached, and this run stopped early again (stop reason: `%s`). "+
		"The pull request stays a draft for a person to review. "+
		"A maintainer can continue it by hand with a %s comment.\n", maxContinuations, stop, command)
}

// runContinuePlan implements `harness continue-plan PR`. It prints exactly
// one output line, action=continue|cap|none, and for a non-none action
// writes the comment body to CONTINUE_COMMENT_PATH for the workflow to post.
func runContinuePlan() error {
	if len(os.Args) < 3 {
		return errors.New("usage: harness continue-plan <PR>")
	}
	pr, ok := dispatchIssue(os.Args[2])
	if !ok {
		return fmt.Errorf("continue-plan: PR number %q is not a positive number of digits", os.Args[2])
	}
	stop := stopUnknown
	if result, _, err := readResult(); err == nil {
		stop = sanitizeStop(result.Stop)
	}
	action, body, _, err := continuePlan(newGitHub(), pr, stop)
	if err != nil {
		return err
	}
	if action == "" {
		fmt.Print("action=none\n")
		return nil
	}
	fmt.Printf("action=%s\n", action)
	path := os.Getenv("CONTINUE_COMMENT_PATH")
	if path == "" {
		return errors.New("CONTINUE_COMMENT_PATH is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(body), 0o644)
}

// ---------------------------------------------------------------- the brief

// closeKeywords are the phrases GitHub links a closing issue on, longest
// first so "closed" is tried before "close".
var closeKeywords = []string{"resolves", "closed", "closes", "resolve", "fixed", "fixes", "close"}

// linkedIssue returns the issue number the pull request body says it closes:
// the first "#N" preceded by a close keyword (Closes/Close/Closed/Fixes/
// Fixed/Resolve/Resolves, case-insensitive), 0 when there is none. The
// continuation brief uses it to say what the issue still requires. N is read
// as digits with a sane bound; an oversized or malformed reference yields 0.
func linkedIssue(body string) int {
	for _, line := range strings.Split(body, "\n") {
		lower := asciiLower(line)
		for i := 0; i < len(lower); i++ {
			if i > 0 && isRefByte(lower[i-1]) {
				continue // keyword inside a word
			}
			matched := ""
			for _, kw := range closeKeywords {
				if strings.HasPrefix(lower[i:], kw) {
					matched = kw
					break
				}
			}
			if matched == "" {
				continue
			}
			if n, ok := issueRefAfter(lower[i+len(matched):]); ok {
				return n
			}
			i += len(matched) - 1
		}
	}
	return 0
}

// issueRefAfter parses "#N" after a close keyword, skipping spaces and one
// colon: N is digits followed by a word boundary, positive and at most
// math.MaxInt32. ok is false for anything else.
func issueRefAfter(after string) (int, bool) {
	j := 0
	for j < len(after) && (after[j] == ' ' || after[j] == ':' || after[j] == '\t') {
		j++
	}
	if j >= len(after) || after[j] != '#' {
		return 0, false
	}
	j++
	start := j
	for j < len(after) && after[j] >= '0' && after[j] <= '9' {
		j++
	}
	if j == start || (j < len(after) && isRefByte(after[j])) {
		return 0, false
	}
	n, err := strconv.Atoi(after[start:j])
	if err != nil || n <= 0 || n > math.MaxInt32 {
		return 0, false
	}
	return n, true
}
