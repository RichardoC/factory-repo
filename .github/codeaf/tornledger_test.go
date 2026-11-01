package main

import (
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
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

// TestStatePackCountsRunCapOnTornLedger keeps the spend handed on
// fail-closed (issue #392 review): the report may show a torn ledger's good
// lines as a lower bound, but state pack's fallback feeds the weekly hold, so
// with the proxy unreachable and a torn line in the ledger it counts the whole
// run cap, as it always has for an unreadable ledger, while a clean ledger
// still counts exactly what it holds.
func TestStatePackCountsRunCapOnTornLedger(t *testing.T) {
	cfg, err := loadConfig("models.json")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	// A port nothing listens on, so meteredRunSpend fails and the ledger is read.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	_ = l.Close()
	t.Setenv("PROXY_PORT", port)
	t.Setenv("GITHUB_RUN_ID", "88")
	t.Setenv("GITHUB_RUN_ATTEMPT", "1")
	t.Setenv("BASE_GENERATION", "0")
	good := `{"ts":"2026-10-16T11:00:00Z","model":"m","cost_usd":0.1}` + "\n"
	for _, c := range []struct {
		name   string
		ledger string
		want   float64
	}{
		{"clean ledger", good, 0.1},
		{"torn last line", good + `{"ts":"2026-10-16T11:0`, cfg.Limits.RunCreditUSD},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ledger.jsonl")
			if err := os.WriteFile(path, []byte(c.ledger), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("LEDGER_PATH", path)
			out := filepath.Join(t.TempDir(), "codeaf-state")
			if err := statePack(t.TempDir(), out, cfg, now); err != nil {
				t.Fatalf("pack: %v", err)
			}
			meta, err := readRunMeta(out)
			if err != nil {
				t.Fatal(err)
			}
			if meta.RunUSD != c.want {
				t.Errorf("RunUSD = %v, want %v", meta.RunUSD, c.want)
			}
		})
	}
}
