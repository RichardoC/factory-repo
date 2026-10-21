package main

// harness gh-retry (issue #146): runs one GitHub CLI call through bounded
// retries for transient upstream failures. Run 39072735041 (#128) lost its
// pull request to a single `HTTP 504` from the pull-request-create call:
// the branch was pushed, but a one-off gateway timeout made publish report
// "could not open the pull request". The publish job now runs its
// `gh pr create` through `harness gh-retry --`, and this command owns the
// retry policy. A 504 there usually means the pull request was created, so
// the retry gets a 422 "already exists" (not retried); the publish job then
// looks the pull request up instead of failing. The policy:
//
//   - up to ghRetryAttempts runs of the command (one attempt plus at most
//     ghRetryAttempts-1 retries), waiting ghRetryBackoffs between them;
//   - only transient failures are retried: a 5xx status anywhere in the
//     command's output, or a transport error (the request never got an HTTP
//     response). A lone HTTP 4xx is never retried: a 4xx is GitHub's
//     answer, not a hiccup on the way to it. A genuine 5xx still wins over
//     a stray 4xx in the same output: the 504 that failed the call is what
//     is retried, not judged by an unrelated "HTTP 422" earlier in it;
//   - the command's stdout and stderr are forwarded exactly once, from its
//     final run only, so a command-substitution capturing the subcommand's
//     stdout gets the URL and nothing the failed attempts printed;
//   - the exit code is the command's own (127 when it could not even run,
//     which is not retried), so the workflow's error handling is unchanged.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// ghRetryAttempts is how many times the command is run in total. Three
// failures exhaust the allowance: that is what the backoff pair below paces.
const ghRetryAttempts = 3

// ghRetryBackoffs is the wait before each retry: 5 s before the second run,
// 15 s before the third. gh-retry never sleeps after the last attempt.
// runGhRetry passes the production values; tests pass zero waits.
var ghRetryBackoffs = []time.Duration{5 * time.Second, 15 * time.Second}

// ghRetryHTTPStatus finds GitHub's "HTTP <code>" text in a command's output,
// as the CLI prints it, e.g. "HTTP 504: 504 Gateway Timeout (url)".
var ghRetryHTTPStatus = regexp.MustCompile(`HTTP ([0-9]{3})`)

// ghRetryNetworkError matches the transport errors a GitHub call surfaces
// when a request never got an HTTP response at all, in the wordings both
// Go's net stack and curl-style CLIs print: name resolution (Go's "no such
// host", curl's "could not resolve host", the resolver's "temporary failure
// in name resolution"), dialing and routing, timeouts mid-request, TLS, and
// cut connections.
var ghRetryNetworkError = regexp.MustCompile(`(?i)(` + strings.Join([]string{
	`connection refused`,
	`connection reset`,
	`connection timed out`,
	`connection aborted`,
	`could not resolve host`,
	`dial tcp`,
	`network is down`,
	`network is unreachable`,
	`no route to host`,
	`no such host`,
	`temporary failure in name resolution`,
	`server misbehaving`,
	`i/o timeout`,
	`Client\.Timeout`,
	`Client\.timeout exceeded`,
	`context deadline exceeded`,
	`request timed out`,
	`TLS handshake`,
	`unexpected EOF`,
	`proxy error`,
}, `|`) + `)`)

// ghRetryExitCodeError carries the wrapped command's exit code back to
// main, which exits with it (the dispatch in main.go): gh-retry's exit code
// is the wrapped command's own, not the harness's usual 1, and publish's
// command-substitution checks and `|| true` handling both depend on that.
// Its message is never printed: the command's output was already forwarded
// by ghRetryMain, and log.Fatal's "harness: " line would only confuse a
// caller parsing the output.
type ghRetryExitCodeError struct{ code int }

func (e ghRetryExitCodeError) Error() string {
	return fmt.Sprintf("gh-retry: exit code %d", e.code)
}

// runGhRetry is the subcommand entry point ("harness gh-retry -- gh ...").
// Success returns nil -- main's usual path -- and every other exit code is
// handed back as a ghRetryExitCodeError for main to exit with.
func runGhRetry() error {
	code := ghRetryMain(os.Args[2:], os.Stdout, os.Stderr, ghRetryBackoffs)
	if code == 0 {
		return nil
	}
	return ghRetryExitCodeError{code}
}

// ghRetryMain is runGhRetry's body, split out so tests can drive it against
// a fake gh on PATH without exiting the test process: it returns the exit
// code the subcommand should exit with, and writes the wrapped command's
// final-run stdout and stderr (and its own retry notes, on stderr only) to
// the given writers.
func ghRetryMain(args []string, stdout, stderr io.Writer, backoffs []time.Duration) int {
	if len(args) < 2 || args[0] != "--" {
		fmt.Fprintln(stderr, "usage: harness gh-retry -- <command> [args...]")
		return 2
	}
	wrapped := args[1:]
	for attempt := 1; ; attempt++ {
		var outBuf, errBuf strings.Builder
		cmd := exec.Command(wrapped[0], wrapped[1:]...)
		cmd.Stdout, cmd.Stderr = &outBuf, &errBuf
		err := cmd.Run()
		if err == nil {
			writeBoth(stdout, outBuf.String(), stderr, errBuf.String())
			return 0
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			// The command never ran (not on PATH, not executable): not
			// transient, and there is no exit code to preserve.
			writeBoth(stdout, outBuf.String(), stderr, errBuf.String())
			fmt.Fprintf(stderr, "harness: gh-retry: %v\n", err)
			return 127
		}
		code := exitErr.ExitCode()
		combined := outBuf.String() + errBuf.String()
		if attempt >= ghRetryAttempts || !ghRetryTransient(combined) {
			writeBoth(stdout, outBuf.String(), stderr, errBuf.String())
			return code
		}
		wait := time.Duration(0)
		if attempt <= len(backoffs) {
			wait = backoffs[attempt-1]
		}
		fmt.Fprintf(stderr, "harness: gh-retry: attempt %d of %d failed with a transient error; retrying in %s\n",
			attempt, ghRetryAttempts, wait)
		time.Sleep(wait)
	}
}

// ghRetryTransient reports whether a failed command's combined output shows
// a transient failure worth retrying: an HTTP 5xx, or a transport error when
// no HTTP status was printed. The 5xx that failed the call is retried even
// when an unrelated "HTTP 4xx" appears earlier in the same output (a stray
// substring must not veto a genuine gateway timeout), while an output whose
// only status is a 4xx -- or any other non-5xx -- is GitHub's answer, and
// asking again cannot change it.
func ghRetryTransient(output string) bool {
	matches := ghRetryHTTPStatus.FindAllStringSubmatch(output, -1)
	if len(matches) == 0 {
		return ghRetryNetworkError.MatchString(output)
	}
	for _, match := range matches {
		if strings.HasPrefix(match[1], "5") {
			return true
		}
	}
	return false
}

// writeBoth forwards one run's captured output to its streams, exactly once.
func writeBoth(stdout io.Writer, out string, stderr io.Writer, errOut string) {
	if out != "" {
		_, _ = io.WriteString(stdout, out)
	}
	if errOut != "" {
		_, _ = io.WriteString(stderr, errOut)
	}
}
