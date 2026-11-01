package main

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReportSurvivesTornLedger (issue #392): the ledger is appended one bare
// write per line while the run job has a hard wall clock, so a proxy killed
// mid-write leaves a partial final line -- exactly the timed-out run whose
// figures matter most. readLedger must skip the torn line and warn once that
// the figures are a lower bound, so `harness report` exits 0 and still
// renders the valid calls, instead of failing and deleting the report comment.
func TestReportSurvivesTornLedger(t *testing.T) {
	t.Setenv("HARNESS_DIR", ".")
	useFixture(t, fixtureIssue)
	ledger := filepath.Join(t.TempDir(), "ledger.jsonl")
	// One full call line, then the torn tail the kill left behind: mid-JSON,
	// no trailing newline.
	text := `{"kind":"proxy_start"}
{"model":"zai-org/glm-5.3-flash","cost_usd":0.25}
{"model":"zai-org/gl`
	if err := os.WriteFile(ledger, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEDGER_PATH", ledger)

	lines, err := readLedger(ledger)
	if err != nil {
		t.Fatalf("readLedger: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("readLedger returned %d line(s), want the 2 decodable ones: %+v", len(lines), lines)
	}

	var logs strings.Builder
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)
	out, err := captureStdout(t, runReport)
	if err != nil {
		t.Fatalf("runReport: %v", err)
	}
	if got := strings.Count(logs.String(), "lower bound"); got != 1 {
		t.Errorf("the warning appeared %d time(s), want exactly once: %q", got, logs.String())
	}
	if !strings.Contains(logs.String(), "skipped 1 torn line") {
		t.Errorf("the warning does not name the torn line: %q", logs.String())
	}
	if !strings.Contains(out, "| model |") {
		t.Errorf("the report lost the per-model cost table: %.600q", out)
	}
	if !strings.Contains(out, "$0.2500") {
		t.Errorf("the report does not show the valid line's cost: %.600q", out)
	}
}
