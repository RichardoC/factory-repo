package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildJobq compiles the jobq command into a temp dir and returns its path.
// The e2e tests drive that binary, not run(), so the argument parsing, the
// config/state files and the printed output are all the real ones.
func buildJobq(t *testing.T) string {
	t.Helper()
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("go tool not available: %v", err)
	}
	bin := filepath.Join(t.TempDir(), "jobq")
	cmd := exec.Command(goTool, "build", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// jobq runs the built binary and returns its combined output.
func jobq(t *testing.T, bin string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("jobq %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// writeJSON writes body to dir/name and returns the path.
func writeJSON(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A real `jobq dispatch` starts a digest run when the backlog is below
// digest.min_backlog and a slot is left over after the queued issues.
func TestE2EDispatchStartsDigest(t *testing.T) {
	bin := buildJobq(t)
	dir := t.TempDir()
	cfg := writeJSON(t, dir, "config.json", `{"queue":{"max_parallel":3},"digest":{"min_backlog":2}}`)

	// One queued issue, below the minimum of 2: issue then digest.
	st := writeJSON(t, dir, "state.json", `{"queued":[4],"in_flight":0,"digest_in_flight":false}`)
	got := jobq(t, bin, "dispatch", cfg, st)
	if got != "{\"issue\":\"4\"}\n{\"digest\":\"true\"}\n" {
		t.Fatalf("dispatch output = %q, want the issue then the digest line", got)
	}

	// An empty queue is still below the minimum: exactly one digest run.
	st = writeJSON(t, dir, "state.json", `{"queued":[],"in_flight":0}`)
	got = jobq(t, bin, "dispatch", cfg, st)
	if got != "{\"digest\":\"true\"}\n" {
		t.Fatalf("dispatch (empty queue) output = %q, want one digest line", got)
	}
}

// A real `jobq dispatch` starts no digest when the backlog is at the minimum,
// or when a digest is already in flight. With no queued issue a digest is the
// only run that needs the remaining slot, so in_flight below max_parallel
// still starts one.
func TestE2EDispatchNoDigest(t *testing.T) {
	bin := buildJobq(t)
	dir := t.TempDir()
	cfg := writeJSON(t, dir, "config.json", `{"queue":{"max_parallel":4},"digest":{"min_backlog":2}}`)
	for _, tc := range []struct {
		name, state, want string
	}{
		{"at minimum", `{"queued":[1,2],"in_flight":0}`, "{\"issue\":\"1\"}\n{\"issue\":\"2\"}\n"},
		{"in flight", `{"queued":[],"in_flight":4,"digest_in_flight":false}`, `{"digest":"true"}` + "\n"},
		{"digest in flight", `{"queued":[],"digest_in_flight":true}`, ""},
	} {
		st := writeJSON(t, dir, "state.json", tc.state)
		if got := jobq(t, bin, "dispatch", cfg, st); got != tc.want {
			t.Fatalf("%s: dispatch output = %q, want %q", tc.name, got, tc.want)
		}
	}

	// With max_parallel 1 the queued issue takes the only slot: no digest.
	one := writeJSON(t, dir, "config-one.json", `{"queue":{"max_parallel":1}}`)
	st := writeJSON(t, dir, "state.json", `{"queued":[9]}`)
	if got := jobq(t, bin, "dispatch", one, st); got != "{\"issue\":\"9\"}\n" {
		t.Fatalf("dispatch (one slot) output = %q, want only the issue", got)
	}

	// The default min_backlog of 2 applies when the digest section is absent.
	st = writeJSON(t, dir, "state.json", `{"queued":[7]}`)
	def := writeJSON(t, dir, "config-default.json", `{"queue":{"max_parallel":3}}`)
	if got := jobq(t, bin, "dispatch", def, st); got != "{\"issue\":\"7\"}\n{\"digest\":\"true\"}\n" {
		t.Fatalf("dispatch (default min_backlog) output = %q, want issue then digest", got)
	}
}

// A real `jobq decide` on a digest event of {"digest":"true"} prints run=true
// and mode=digest with no number= line; combined with a real dispatch, the
// digest line is the event that decides to a digest run and performs as one.
func TestE2EDecideDigest(t *testing.T) {
	bin := buildJobq(t)
	dir := t.TempDir()
	ev := writeJSON(t, dir, "event.json", `{"inputs":{"digest":"true"}}`)
	got := jobq(t, bin, "decide", ev)
	if !strings.Contains(got, "run=true\n") {
		t.Fatalf("decide output = %q, want run=true", got)
	}
	if !strings.Contains(got, "mode=digest\n") {
		t.Fatalf("decide output = %q, want mode=digest", got)
	}
	if strings.Contains(got, "number=") {
		t.Fatalf("decide output = %q, want no number= line", got)
	}

	// The digest inputs a real dispatch emits decide to a digest run too: take
	// the last line of a dispatch and feed its "inputs" to decide.
	cfg := writeJSON(t, dir, "config.json", `{"queue":{"max_parallel":3}}`)
	st := writeJSON(t, dir, "state.json", `{"queued":[4]}`)
	lines := strings.Split(strings.TrimSuffix(jobq(t, bin, "dispatch", cfg, st), "\n"), "\n")
	if len(lines) != 2 || lines[1] != `{"digest":"true"}` {
		t.Fatalf("dispatch lines = %q, want one digest line last", lines)
	}
	ev = writeJSON(t, dir, "event.json", `{"inputs":`+lines[1]+`}`)
	got = jobq(t, bin, "decide", ev)
	if !strings.Contains(got, "run=true\n") || !strings.Contains(got, "mode=digest\n") || strings.Contains(got, "number=") {
		t.Fatalf("decide(dispatch digest) output = %q, want run=true, mode=digest, no number=", got)
	}
}

// Issue and review runs still print their mode and number through the CLI.
func TestE2EDecideIssueAndReview(t *testing.T) {
	bin := buildJobq(t)
	dir := t.TempDir()
	ev := writeJSON(t, dir, "event.json", `{"inputs":{"issue":"7"}}`)
	if got := jobq(t, bin, "decide", ev); !strings.Contains(got, "mode=issue\nnumber=7\n") {
		t.Fatalf("decide(issue) output = %q, want mode=issue and number=7", got)
	}
	ev = writeJSON(t, dir, "event.json", `{"inputs":{"review":"12"}}`)
	if got := jobq(t, bin, "decide", ev); !strings.Contains(got, "mode=review\nnumber=12\n") {
		t.Fatalf("decide(review) output = %q, want mode=review and number=12", got)
	}
	ev = writeJSON(t, dir, "event.json", `{"inputs":{"digest":"true","issue":"7"}}`)
	if got := jobq(t, bin, "decide", ev); !strings.Contains(got, "run=false\n") || strings.Contains(got, "mode=") {
		t.Fatalf("decide(digest+issue) output = %q, want a skip", got)
	}
}

// The binary rejects a negative digest.min_backlog.
func TestE2ENegativeMinBacklog(t *testing.T) {
	bin := buildJobq(t)
	dir := t.TempDir()
	cfg := writeJSON(t, dir, "config.json", `{"digest":{"min_backlog":-1}}`)
	st := writeJSON(t, dir, "state.json", `{}`)
	out, err := exec.Command(bin, "dispatch", cfg, st).CombinedOutput()
	if err == nil {
		t.Fatalf("dispatch accepted a negative digest.min_backlog: %s", out)
	}
	if !strings.Contains(string(out), "digest.min_backlog must not be negative") {
		t.Fatalf("dispatch error = %q, want the min_backlog error", out)
	}
}
