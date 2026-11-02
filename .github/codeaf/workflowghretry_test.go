package main

// TestWorkflowGhRetryWrapsFailSoftCalls checks a whole class of bug that no
// test caught before (issue #429): a fail-soft GitHub call in the publish or
// queue path that runs as raw `gh`, so one transient GitHub 5xx silences the
// review run, the continuation, the fix run or CI for that PR with nothing
// but a ::warning in the log. The repository already owns the retrier for
// this class of failure -- `harness gh-retry` (issue #146), which retries a
// 5xx or a network error and never a 4xx -- and the pull-request-create call
// is wrapped in it; every other fail-soft label edit and every CI/queue/run
// dispatch in codeaf.yml and codeaf-queue.yml must be wrapped the same way,
// with the existing ::warning kept as the after-retries fallback.
//
// The workflow YAML is read as text, not through a YAML library, in the same
// style as TestWorkflowPermissionsCoverAPIUse: the file is the artifact under
// test, and the shapes that matter (a gh call inside a `run:` block, the
// `gh-retry --` wrapper in front of it) are line-oriented. Backslash
// continuations are joined first (shellLines) and comments stripped
// (stripComment), both reused from the permissions test, so a wrapper on the
// first physical line of a continued command is seen and a comment that
// mentions a call is not scanned as one.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ghRetryCovered matches the gh invocations that must go through gh-retry in
// the two queue-setting workflows: every pr-edit label call (all of them
// label a PR with a codeaf-* label or strip one), and the dispatches that
// start CI or another CodeAF run (`ci.yml`, `codeaf-queue.yml`,
// `codeaf.yml`). The `codeaf-state.yml` dispatch in the handoff job is
// deliberately not in the set: it starts the state merge, not a run, and its
// job holds no harness.
var ghRetryCovered = regexp.MustCompile(`\bgh\s+(?:pr edit|workflow run (codeaf-queue|codeaf|ci).yml)\b`)

// unwrappedGhRetryCalls reports every covered gh invocation in a workflow's
// text that does not run through `gh-retry --`: the text before the
// invocation, with trailing spaces dropped, must end with the wrapper's
// `gh-retry --`.
func unwrappedGhRetryCalls(name, text string) []string {
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		lines = append(lines, stripComment(line))
	}
	var bad []string
	for _, line := range shellLines(strings.Join(lines, "\n")) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		for _, loc := range ghRetryCovered.FindAllStringIndex(line, -1) {
			before := strings.TrimRight(line[:loc[0]], " \t")
			wrapped := strings.HasSuffix(before, "--") &&
				strings.HasSuffix(strings.TrimRight(strings.TrimSuffix(before, "--"), " \t"), "gh-retry")
			if !wrapped {
				bad = append(bad, name+": "+line)
			}
		}
	}
	return bad
}

func TestWorkflowGhRetryWrapsFailSoftCalls(t *testing.T) {
	dir := filepath.Join("..", "workflows")
	for _, name := range []string{"codeaf.yml", "codeaf-queue.yml"} {
		name := name
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatalf("reading %s: %v", name, err)
			}
			for _, v := range unwrappedGhRetryCalls(name, string(data)) {
				t.Errorf("fail-soft gh call not wrapped in gh-retry: %s", v)
			}
		})
	}

	// The test must actually catch the class of bug, not parse its way to a
	// pass: unwrap one call of each shape in a copy of the committed text and
	// the check must fail naming it.
	t.Run("unwrapping a dispatch fails", func(t *testing.T) {
		bare := unwrapOne(t, filepath.Join(dir, "codeaf.yml"),
			`"$HARNESS" gh-retry -- gh workflow run ci.yml`, `gh workflow run ci.yml`)
		if v := unwrappedGhRetryCalls("codeaf.yml", bare); len(v) == 0 {
			t.Fatal("unwrapping the CI dispatch produced no violation")
		}
		bare = unwrapOne(t, filepath.Join(dir, "codeaf-queue.yml"),
			`"$HARNESS" gh-retry -- gh workflow run codeaf.yml --ref "$DEFAULT_BRANCH" -f "review=$N"`,
			`gh workflow run codeaf.yml --ref "$DEFAULT_BRANCH" -f "review=$N"`)
		if v := unwrappedGhRetryCalls("codeaf-queue.yml", bare); len(v) == 0 {
			t.Fatal("unwrapping the queue's review dispatch produced no violation")
		}
	})
	t.Run("unwrapping a label edit fails", func(t *testing.T) {
		bare := unwrapOne(t, filepath.Join(dir, "codeaf.yml"),
			`"$HARNESS" gh-retry -- gh pr edit "$PR_NUMBER" --add-label codeaf-review-needed`,
			`gh pr edit "$PR_NUMBER" --add-label codeaf-review-needed`)
		if v := unwrappedGhRetryCalls("codeaf.yml", bare); len(v) == 0 {
			t.Fatal("unwrapping the review-needed label edit produced no violation")
		}
		bare = unwrapOne(t, filepath.Join(dir, "codeaf-queue.yml"),
			`"$HARNESS" gh-retry -- gh pr edit "$N" --remove-label "$CODEAF_REVIEW_NEEDED_LABEL" || true`,
			`gh pr edit "$N" --remove-label "$CODEAF_REVIEW_NEEDED_LABEL" || true`)
		if v := unwrappedGhRetryCalls("codeaf-queue.yml", bare); len(v) == 0 {
			t.Fatal("unwrapping the queue's label removal produced no violation")
		}
	})
}

// unwrapOne returns a copy of a workflow's text with exactly one `wrapped`
// spelling replaced by its `bare` one, failing the test when the committed
// workflow no longer spells the call that way (the mutation must land, or
// the negative proof proves nothing).
func unwrapOne(t *testing.T, path, wrapped, bare string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	text := string(data)
	if !strings.Contains(text, wrapped) {
		t.Fatalf("%s no longer spells the call as %q; update the negative proof", path, wrapped)
	}
	return strings.Replace(text, wrapped, bare, 1)
}
