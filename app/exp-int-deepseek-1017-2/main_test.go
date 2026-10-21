package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runDecide runs `jobq decide` over an EVENT.json written into a temp dir and
// returns its output.
func runDecide(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "EVENT.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run([]string{"decide", path}, &out); err != nil {
		t.Fatalf("run decide: %v", err)
	}
	return out.String()
}

// A digest run prints mode=digest, and no number= line.
func TestRunDecideDigest(t *testing.T) {
	got := runDecide(t, `{"inputs":{"digest":"true"}}`)
	for _, want := range []string{"run=true\n", "mode=digest\n"} {
		if !strings.Contains(got, want) {
			t.Fatalf("decide output = %q, want %q", got, want)
		}
	}
	if strings.Contains(got, "number=") {
		t.Fatalf("decide output = %q, want no number= line", got)
	}
}

// Issue and review runs keep both the mode and number lines.
func TestRunDecideIssueAndReview(t *testing.T) {
	for _, tc := range []struct {
		body, mode string
		n          string
	}{
		{`{"inputs":{"issue":"7"}}`, "issue", "number=7\n"},
		{`{"inputs":{"review":"12"}}`, "review", "number=12\n"},
	} {
		got := runDecide(t, tc.body)
		if !strings.Contains(got, "mode="+tc.mode+"\n") || !strings.Contains(got, tc.n) {
			t.Fatalf("decide(%s) output = %q, want mode=%s and %q", tc.body, got, tc.mode, tc.n)
		}
	}
}

// A digest input that is not exactly "true" is a skip with a reason.
func TestRunDecideDigestNotTrue(t *testing.T) {
	for _, body := range []string{
		`{"inputs":{"digest":"1"}}`,
		`{"inputs":{"digest":"TRUE"}}`,
		`{"inputs":{"digest":"yes"}}`,
	} {
		got := runDecide(t, body)
		if !strings.Contains(got, "run=false\n") || strings.Contains(got, "mode=") {
			t.Fatalf("decide(%s) output = %q, want a skip with a reason", body, got)
		}
		if !strings.Contains(got, "reason=digest must be true\n") {
			t.Fatalf("decide(%s) output = %q, want the digest-must-be-true reason", body, got)
		}
	}
}

// digest together with issue (or review) is refused with a reason, before the
// issue/review handling.
func TestRunDecideDigestWithIssueRefused(t *testing.T) {
	for _, body := range []string{
		`{"inputs":{"digest":"true","issue":"7"}}`,
		`{"inputs":{"digest":"true","review":"7"}}`,
	} {
		got := runDecide(t, body)
		if !strings.Contains(got, "run=false\n") || strings.Contains(got, "mode=") || strings.Contains(got, "number=") {
			t.Fatalf("decide(%s) output = %q, want a skip with a reason", body, got)
		}
		if !strings.Contains(got, "reason=digest is exclusive with issue and review\n") {
			t.Fatalf("decide(%s) output = %q, want the exclusivity reason", body, got)
		}
	}
}
