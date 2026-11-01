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
	"time"
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
// means zero continuations used. Only the review path counts this way
// (review.go): a review run posts no run report of its own, so its fix
// continuations are labelled from the reports left by earlier PR-mode runs.
// The PR-mode continuation count (continuePlan) does not subtract one: it
// runs before the stopping run has posted its own report, so the plain
// report count already equals the continuations used (issue #219).
func continuationsUsed(reportComments int) int {
	if reportComments < 1 {
		return 0
	}
	return reportComments - 1
}

// continuationCapReached reports whether the cap on automatic continuations
// is used up.
func continuationCapReached(used int) bool { return used >= maxContinuations }

// failedReportHeadings open the reports of a run whose patch never landed
// (issue #219): main.go writes the outcome as "### CodeAF run — <outcome>",
// and codeaf.yml's report step sets that outcome to "publishing failed" when
// the publish job failed and to "run job failed" when the run job itself did.
// queue.go's strandedByFailedRun treats both as the same stranded state
// (issue #215), so both must be excluded here too. Each entry is an exact
// prefix, never a search inside the body, because a comment is untrusted
// data; the strings are fixed literals this package writes itself, so a
// comment matches only by reproducing one exactly, and only bot comments
// reach the checks that use them (failedRunReport's callers).
var failedReportHeadings = []string{
	reportMarker + "\n### CodeAF run — publishing failed\n",
	reportMarker + "\n### CodeAF run — run job failed\n",
}

// countReports counts the comments on a pull request that carry a CodeAF
// run report (issue #219, sharpening the count issue #31 started with).
// Ordinary comments, including the continue and cap comments this file
// posts, carry no report marker and are not counted. Two comments that
// carry the marker are still not reports: one by anyone but the workflow
// bot (a forged marker on a public repository can neither inflate nor
// suppress the count), and one whose report says the run did not land —
// publishing failed, the run job failed, a fixed no-result row in place
// of a stop / ok row, or a "no changes" report that ended not ok (issue
// #415). All four shapes are what failedRunReport knows (queue.go), so it
// is the one predicate here too: a report whose patch never landed must
// not spend a continuation slot that a later run's report can only
// undercount.
func countReports(comments []issueComment) int {
	n := 0
	for _, c := range comments {
		if c.User.Login != planReportBotLogin || !strings.Contains(c.Body, reportMarker) {
			continue
		}
		if failedRunReport(c.Body) {
			continue
		}
		n++
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
		// A run that finished in the store (issue #134) reports an
		// incomplete envelope stop; its run record, shipped beside the
		// result, tells the real ending. Read as untrusted data: the
		// promotion only marks the PR ready — the review and publish
		// checks still run on the work.
		stop = effectiveStop(result, readClosing(trajectoryPath()))
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
// up, and "" for everything that needs no comment: an early stop that must
// not be continued, or a missing result. A run that finished (stop "done")
// is not automatically "" either — a pull request born conflicted is
// continued once, or handed over with a cap comment (continueConflicted).
// The returned body is built only from fixed sentences, the sanitized stop
// word and counters — never from remote text — so it is safe to post as a
// comment.
//
// A comment listing that stops at getList's page cap (issue #416) cannot be
// counted: a thread longer than maxListPages pages is left uncounted and
// treated as the cap case — the hand-over comment, no continuation started —
// rather than returned as an error. Before, the raw page-cap error failed the
// continue-plan step, which the workflow runs continue-on-error, so a long
// thread silently ended the continuation chain; failing safe keeps a person
// in the loop. Every other getList failure is still an error.
func continuePlan(gh github, pr int, stop string) (action string, body string, used int, err error) {
	stop = sanitizeStop(stop)
	if !earlyStop(stop) || !continuesAfter(stop) {
		// Issue #137: a run that finished (stop "done") on a pull request
		// born conflicted — publish started its branch from an old base
		// and the 3-way apply failed — still needs one continuation,
		// through the base-merge path (merge.go, issue #57) that a
		// codeaf-continue-needed run takes.
		if stop == stopDone {
			return continueConflicted(gh, pr)
		}
		return "", "", 0, nil
	}
	var comments []issueComment
	if err := gh.getList(fmt.Sprintf("/issues/%d/comments?per_page=100", pr), &comments); err != nil {
		if isPageCapError(err) {
			// Issue #416: the listing is truncated, not broken. A thread
			// that long has spent the continuations, so hand over exactly
			// as the cap case does; used is maxContinuations because no
			// smaller count can be proved from the pages read.
			return "cap", capBody(stop), maxContinuations, nil
		}
		return "", "", 0, err
	}
	// The count is the continuations already run (issue #219): continue-plan
	// runs before this run's own report is posted, so every report comment
	// counted here is a run that came before, and the next continuation is
	// labelled used+1.
	used = countReports(comments)
	if continuationCapReached(used) {
		return "cap", capBody(stop), used, nil
	}
	return "continue", continueBody(stop, used), used, nil
}

// ---------------------------------------------------------------- the PR born conflicted

// Mergeable states of a pull request that this file distinguishes (issue
// #137). GitHub computes mergeable_state asynchronously: "unknown" means
// the answer is not ready yet, and "dirty" means the branch conflicts with
// its base.
const (
	mergeableDirty   = "dirty"
	mergeableUnknown = "unknown"
)

// conflictBody is the comment that starts the continuation of a pull
// request born conflicted: fixed text only, never remote text, with the
// trailing newline the other comment bodies carry.
const conflictBody = "The branch conflicts with its base branch; a continuation brings the base in and resolves the conflicts\n"

// continueSleep is injectable so tests of the mergeable_state retries do
// not actually sleep.
var continueSleep = time.Sleep

// mergeableStateBudget bounds the whole mergeable_state probe (the review
// of this change): the retry schedule alone is 15 seconds of sleeps, and
// each fetch may stall for the API client's full 30-second timeout, so an
// unbounded probe could stretch the continue-plan step into minutes. Fast
// answers never touch the budget — the 3-retries-5-seconds-apart schedule
// of issue #137 completes in about 15 seconds — and a probe that runs out
// of budget returns the last state it read, which the caller treats as no
// conflict. Injected as a var so tests can shrink it.
var mergeableStateBudget = 20 * time.Second

// mergeableState fetches a pull request's mergeable_state (issue #137). While
// GitHub is still computing it the fetch is retried up to 3 times, 5
// seconds apart; past that — or past mergeableStateBudget — the last state
// is returned and the caller does nothing with it.
func (g github) mergeableState(pr int) (string, error) {
	const unknownRetries = 3
	const retryWait = 5 * time.Second
	deadline := time.Now().Add(mergeableStateBudget)
	for attempt := 0; ; attempt++ {
		var p struct {
			MergeableState string `json:"mergeable_state"`
		}
		if err := g.get(fmt.Sprintf("/pulls/%d", pr), &p); err != nil {
			return "", err
		}
		if p.MergeableState != mergeableUnknown || attempt >= unknownRetries {
			return p.MergeableState, nil
		}
		// GitHub has answered "unknown" every time: one more fetch would
		// wait, and must still start inside the budget.
		if !time.Now().Before(deadline) {
			return p.MergeableState, nil
		}
		continueSleep(retryWait)
	}
}

// continueConflicted plans the continuation of a pull request born
// conflicted: its run finished (stop "done"), so the early-stop path does
// not apply, but the branch conflicts with its base and only a PR-mode run
// resolves that. A fetch error is returned, not swallowed (the review of
// this change): a failing /pulls call would otherwise print action=none and
// silently leave a conflicted pull request unresolved, while a returned
// error fails the continue-plan step — the workflow runs it
// continue-on-error, so publishing still goes on and the failure is
// visible. A state other than dirty does nothing. The continuation cap
// already used up does not drop the pull request silently either: like the
// early-stop path, it hands over with a "cap" comment, worded for a run
// that finished, not one that stopped early.
//
// A comment listing that stops at getList's page cap (issue #416) is the
// same cap case as in continuePlan: the count is unreadable, so the pull
// request is handed over with the cap comment instead of failing the step —
// but only once the mergeable_state probe says it really conflicts, since a
// clean pull request needs no comment at all. Any other listing failure is
// still returned.
func continueConflicted(gh github, pr int) (action string, body string, used int, err error) {
	var comments []issueComment
	capped := false
	if err := gh.getList(fmt.Sprintf("/issues/%d/comments?per_page=100", pr), &comments); err != nil {
		if !isPageCapError(err) {
			return "", "", 0, err
		}
		capped = true
	}
	// Same semantics as continuePlan (issue #219): the plain report count,
	// not one less — this run's own report is not posted yet either. A
	// capped listing cannot be counted, so it is taken as the cap reached,
	// like the early-stop path above.
	if capped {
		used = maxContinuations
	} else {
		used = countReports(comments)
	}
	state, err := gh.mergeableState(pr)
	if err != nil {
		return "", "", 0, err
	}
	if state != mergeableDirty {
		return "", "", 0, nil
	}
	if capped || continuationCapReached(used) {
		return "cap", conflictCapBody(), used, nil
	}
	return "continue", conflictBody, used, nil
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

// conflictCapBody is the cap comment for a pull request born conflicted,
// whose run finished (stop "done"): the early-stop cap body above says the
// run "stopped early again", which reads wrong here, so this says what
// actually holds. Fixed text only, like every body this file posts.
func conflictCapBody() string {
	return fmt.Sprintf("The continuation cap (%d per issue) is reached, and the pull request still conflicts with its base branch. "+
		"The pull request stays a draft for a person to review. "+
		"A maintainer can continue it by hand with a %s comment.\n", maxContinuations, command)
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
		// A run that finished in the store (issue #134) needs no
		// continuation; its run record, shipped beside the result, tells
		// the real ending and is read as untrusted data.
		stop = effectiveStop(result, readClosing(trajectoryPath()))
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
// first so "closed" is tried before "close", and "fixes"/"fixed" before
// "fix".
var closeKeywords = []string{"resolved", "resolves", "closed", "closes", "resolve", "fixed", "fixes", "close", "fix"}

// linkedIssue returns the issue number the pull request body says it closes:
// the first "#N" preceded by a close keyword (Closes/Close/Closed/Fix/
// Fixes/Fixed/Resolve/Resolves/Resolved, case-insensitive), 0 when there is
// none. The
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
