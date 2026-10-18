package main

// Tests for "harness gh-retry" (issue #146): a pull request was lost to one
// `HTTP 504` with no retry. The acceptance cases drive ghRetryMain (the
// subcommand's body) against a fake gh installed on PATH, exactly what the
// command runs in publish: the fake counts its own invocations, fails the
// first GHRETRY_TEST_FAILS of them with GHRETRY_TEST_ERR on stderr and the
// configured exit code, and succeeds with a pull-request URL on stdout.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeGh installs a gh executable in a scratch directory, puts that
// directory first on PATH and points the fake's counter file at it, and
// returns a function reading how many times the fake has run. t.Setenv
// keeps every piece in the test's scope; the wrapped command inherits the
// environment, so it reaches the fake too.
func fakeGh(t *testing.T) func() int {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"n=$(cat \"$GHRETRY_TEST_COUNT\" 2>/dev/null || echo 0)\n" +
		"n=$((n + 1))\n" +
		"printf '%s\\n' \"$n\" > \"$GHRETRY_TEST_COUNT\"\n" +
		"if [ \"$n\" -le \"${GHRETRY_TEST_FAILS:-0}\" ]; then\n" +
		"  printf '%s\\n' \"${GHRETRY_TEST_ERR:-HTTP 504: 504 Gateway Timeout}\" >&2\n" +
		"  exit \"${GHRETRY_TEST_FAIL_CODE:-1}\"\n" +
		"fi\n" +
		"printf 'https://github.com/RichardoC/factory-repo/pull/129\\n'\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	countFile := filepath.Join(dir, "count")
	t.Setenv("GHRETRY_TEST_COUNT", countFile)
	// Drop any setting leaking from a previous subtest before it matters.
	t.Setenv("GHRETRY_TEST_FAILS", "0")
	t.Setenv("GHRETRY_TEST_ERR", "")
	t.Setenv("GHRETRY_TEST_FAIL_CODE", "1")
	return func() int {
		t.Helper()
		raw, err := os.ReadFile(countFile)
		if err != nil {
			t.Fatalf("read fake gh counter: %v", err)
		}
		n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err != nil {
			t.Fatalf("fake gh counter %q is not a number: %v", raw, err)
		}
		if n == 0 {
			t.Fatalf("fake gh never ran (counter %q)", raw)
		}
		return n
	}
}

// zeroBackoffs runs ghRetryMain without its waits, so the retry tests do not
// really sleep 5 s and 15 s between attempts.
var zeroBackoffs = []time.Duration{}

// TestGhRetry is the acceptance case from issue #146: a fake gh on PATH
// fails twice with exit 1 and prints HTTP 504, then succeeds.
func TestGhRetry(t *testing.T) {
	runs := fakeGh(t)
	t.Setenv("GHRETRY_TEST_FAILS", "2") // two HTTP 504s, then success
	var out, errOut strings.Builder
	code := ghRetryMain([]string{"--", "gh", "pr", "create", "--title", "x"}, &out, &errOut, zeroBackoffs)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	if runs() != 3 {
		t.Errorf("fake gh ran %d times, want 3 (two failures retried)", runs())
	}
	if want := "https://github.com/RichardoC/factory-repo/pull/129\n"; out.String() != want {
		t.Errorf("stdout = %q, want exactly the successful run's %q", out.String(), want)
	}
}

func TestGhRetryDoesNotRetry4xx(t *testing.T) {
	runs := fakeGh(t)
	t.Setenv("GHRETRY_TEST_FAILS", "1") // would succeed on a retry if one happened
	t.Setenv("GHRETRY_TEST_ERR", "gh: HTTP 422: Unprocessable Entity (https://api.github.com/graphql)")
	var out, errOut strings.Builder
	code := ghRetryMain([]string{"--", "gh", "pr", "edit", "12", "--add-label", "codeaf"}, &out, &errOut, zeroBackoffs)
	if code != 1 {
		t.Errorf("exit code = %d, want the fake's 1", code)
	}
	if runs() != 1 {
		t.Errorf("fake gh ran %d times, want 1: HTTP 4xx is never retried", runs())
	}
	if !strings.Contains(errOut.String(), "HTTP 422") {
		t.Errorf("stderr %q lost the fake's output", errOut.String())
	}
}

func TestGhRetryExhaustedReturnsCodeAndOutput(t *testing.T) {
	runs := fakeGh(t)
	t.Setenv("GHRETRY_TEST_FAILS", "3") // all three attempts fail
	var out, errOut strings.Builder
	code := ghRetryMain([]string{"--", "gh", "pr", "comment", "12", "--body", "hi"}, &out, &errOut, zeroBackoffs)
	if code != 1 {
		t.Errorf("exit code = %d, want the fake's 1 after three failed attempts", code)
	}
	if runs() != 3 {
		t.Errorf("fake gh ran %d times, want 3 (the retry allowance)", runs())
	}
	if !strings.Contains(errOut.String(), "HTTP 504: 504 Gateway Timeout") {
		t.Errorf("stderr %q does not carry the fake's output", errOut.String())
	}
	if strings.Contains(out.String(), "pull/129") {
		t.Errorf("stdout %q must not contain the success output of runs that never happened", out.String())
	}
}

func TestGhRetrySucceedsFirstTry(t *testing.T) {
	runs := fakeGh(t)
	var out, errOut strings.Builder
	code := ghRetryMain([]string{"--", "gh", "pr", "view", "12"}, &out, &errOut, zeroBackoffs)
	if code != 0 || runs() != 1 {
		t.Errorf("exit code = %d, runs = %d; want 0 and 1", code, runs())
	}
}

func TestGhRetryNetworkErrorIsRetried(t *testing.T) {
	runs := fakeGh(t)
	t.Setenv("GHRETRY_TEST_FAILS", "1")
	t.Setenv("GHRETRY_TEST_ERR", `Post "https://api.github.com/graphql": dial tcp: lookup api.github.com: no such host`)
	var out, errOut strings.Builder
	code := ghRetryMain([]string{"--", "gh", "label", "create", "x"}, &out, &errOut, zeroBackoffs)
	if code != 0 {
		t.Errorf("exit code = %d, want 0 after retrying a network error (stderr: %s)", code, errOut.String())
	}
	if runs() != 2 {
		t.Errorf("fake gh ran %d times, want 2 (one retry after a network error)", runs())
	}
}

func TestGhRetryUsage(t *testing.T) {
	var out, errOut strings.Builder
	if code := ghRetryMain([]string{"gh", "pr", "view"}, &out, &errOut, zeroBackoffs); code != 2 {
		t.Errorf("exit code = %d, want 2 for a missing -- before the command", code)
	}
}

func TestGhRetryMissingCommandIsNotRetried(t *testing.T) {
	var out, errOut strings.Builder
	code := ghRetryMain([]string{"--", "no-such-binary-definitely-gh", "pr", "view"}, &out, &errOut, zeroBackoffs)
	if code != 127 {
		t.Errorf("exit code = %d, want 127 for a command that never ran", code)
	}
}

func TestGhRetryTransientClassification(t *testing.T) {
	for _, tc := range []struct {
		output string
		want   bool
	}{
		{"gh: HTTP 504: 504 Gateway Timeout (https://api.github.com/graphql)", true},
		{"HTTP 502: bad gateway", true},
		{"HTTP 500: Internal Server Error", true},
		{`Post "https://api.github.com/graphql": dial tcp 1.2.3.4:443: connect: connection refused`, true},
		{`Post "https://api.github.com/graphql": dial tcp: lookup api.github.com: no such host`, true},
		{"gh: read tcp 1.2.3.4:443->5.6.7.8:1: i/o timeout: unexpected EOF: retrying", true},
		{"HTTP 422: Unprocessable Entity", false},
		{"HTTP 404: Not Found (https://api.github.com/repos)", false},
		{"gh: HTTP 403: API rate limit exceeded", false},
		{"", false},
		{"gh: no git remotes found", false},
		// A 4xx anywhere beats everything else: GitHub answered.
		{"HTTP 422 first, then a later HTTP 504 from a fallback", false},
	} {
		if got := ghRetryTransient(tc.output); got != tc.want {
			t.Errorf("ghRetryTransient(%q) = %v, want %v", tc.output, got, tc.want)
		}
	}
}

func TestGhRetryWaitsTheBackoffs(t *testing.T) {
	runs := fakeGh(t)
	t.Setenv("GHRETRY_TEST_FAILS", "2")
	// Short waits carry the shape of the production ones (before the second
	// and third run, none after the last) so the test measures that the
	// backoffs really pace the retries without sleeping 5 s and 15 s.
	backoffs := []time.Duration{10 * time.Millisecond, 25 * time.Millisecond}
	var out, errOut strings.Builder
	start := time.Now()
	code := ghRetryMain([]string{"--", "gh", "pr", "edit", "12", "--add-label", "x"}, &out, &errOut, backoffs)
	elapsed := time.Since(start)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	if runs() != 3 {
		t.Errorf("fake gh ran %d times, want 3", runs())
	}
	if elapsed < 35*time.Millisecond {
		t.Errorf("elapsed %s, want at least the 35 ms of backoff between attempts", elapsed)
	}
}
