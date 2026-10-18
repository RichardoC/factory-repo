package main

// harness gh-retry (issue #146): runs one GitHub CLI call through bounded
// retries for transient upstream failures. Run 39072735041 (#128) lost its
// pull request to a single `HTTP 504` from the pull-request-create call:
// the branch was pushed, but a one-off gateway timeout made publish report
// "could not open the pull request". The workflow now prefixes its GitHub
// write calls with `harness gh-retry --`, so the whole change there is a
// one-word prefix and this command owns the retry policy:
//
//   - up to ghRetryAttempts runs of the command (one attempt plus at most
//     ghRetryAttempts-1 retries), waiting ghRetryBackoffs between them;
//   - only transient failures are retried: a 5xx status anywhere in the
//     command's output, or a transport error (the request never got an HTTP
//     response). An HTTP 4xx (or any other explicit status) is never
//     retried: a 4xx is GitHub's answer, not a hiccup on the way to it;
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

// ghRetryNetworkError matches the transport errors GitHub's CLI surfaces
// verbatim from Go's net stack when a request never got an HTTP response at
// all: name resolution, dialing, timeouts mid-request, TLS, cut connections.
var ghRetryNetworkError = regexp.MustCompile(`(?i)(` + strings.Join([]string{
	`connection refused`,
	`connection reset`,
	`connection timed out`,
	`dial tcp`,
	`network is unreachable`,
	`no such host`,
	`i/o timeout`,
	`Client\.Timeout`,
	`Client\.timeout exceeded`,
	`context deadline exceeded`,
	`TLS handshake`,
	`unexpected EOF`,
	`proxy error`,
}, `|`) + `)`)

// runGhRetry is the subcommand entry point ("harness gh-retry -- gh ...").
// It exits itself rather than returning an error, because its exit code is
// the wrapped command's own, not the harness's usual 1: publish's
// command-substitution checks and `|| true` handling both depend on that.
// main never sees the return below.
func runGhRetry() error {
	os.Exit(ghRetryMain(os.Args[2:], os.Stdout, os.Stderr, ghRetryBackoffs))
	return nil
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
// no HTTP status was printed. A 4xx always wins against both: GitHub
// answered, so asking again cannot change the answer.
func ghRetryTransient(output string) bool {
	matches := ghRetryHTTPStatus.FindAllStringSubmatch(output, -1)
	if len(matches) == 0 {
		return ghRetryNetworkError.MatchString(output)
	}
	saw5xx, saw4xx := false, false
	for _, match := range matches {
		if strings.HasPrefix(match[1], "5") {
			saw5xx = true
		}
		if strings.HasPrefix(match[1], "4") {
			saw4xx = true
		}
	}
	return saw5xx && !saw4xx
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
