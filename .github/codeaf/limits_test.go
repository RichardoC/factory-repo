package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// loadConfig validates the Big-plan limits: the plan must be named, the two
// credit caps must be positive, and the overage cap must not be negative (0
// is the default: stop rather than pay past the weekly credit).
func TestLoadConfigLimits(t *testing.T) {
	dir := t.TempDir()
	write := func(t *testing.T, limits string) string {
		t.Helper()
		path := filepath.Join(dir, "models.json")
		body := fmt.Sprintf(`{"limits":%s,"models":[{"id":"m","upstream":"hf:m","efforts":["high"],
			"input":0.1,"output":0.25,"cache_read":0.02,"api_input":0.15,"api_cached":0.04,"api_output":0.5}]}`, limits)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	for _, tc := range []struct {
		name   string
		limits string
		want   string
	}{
		{"missing plan", `{"weekly_credit_usd":20,"run_credit_usd":5,"overage_usd":0}`, "limits.plan"},
		{"zero run credit", `{"plan":"big","weekly_credit_usd":20,"run_credit_usd":0,"overage_usd":0}`, "run_credit_usd"},
		{"zero weekly credit", `{"plan":"big","weekly_credit_usd":0,"run_credit_usd":5,"overage_usd":0}`, "weekly_credit_usd"},
		{"negative overage", `{"plan":"big","weekly_credit_usd":20,"run_credit_usd":5,"overage_usd":-1}`, "overage_usd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadConfig(write(t, tc.limits))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("loadConfig = %v, want an error naming %s", err, tc.want)
			}
		})
	}
	cfg, err := loadConfig(write(t, `{"plan":"big","weekly_credit_usd":20,"run_credit_usd":5,"overage_usd":0}`))
	if err != nil {
		t.Fatalf("valid limits rejected: %v", err)
	}
	if cfg.Limits.Plan != "big" || cfg.Limits.WeeklyCreditUSD != 20 || cfg.Limits.RunCreditUSD != 5 || cfg.Limits.OverageUSD != 0 {
		t.Fatalf("limits = %+v", cfg.Limits)
	}
	// Overage 0 is valid, and is the default when the field is missing.
	cfg, err = loadConfig(write(t, `{"plan":"big","weekly_credit_usd":20,"run_credit_usd":5}`))
	if err != nil || cfg.Limits.OverageUSD != 0 {
		t.Fatalf("overage default = %+v, %v", cfg.Limits, err)
	}
}

// overCap enforces, in order: the per-run credit cap, then the weekly credit
// cap — refused outright when overage is 0, allowed past it only up to the
// API-price overage cap.
func TestOverCapWeeklyAndOverage(t *testing.T) {
	newProxy := func() *proxy {
		// The run cap stays out of the way ($50): these cases test the weekly
		// credit and the overage caps; the run cap has its own case below.
		return &proxy{runCap: 50, weekCap: 20, weekSpent: 10}
	}
	// Under both caps: allowed.
	if got := newProxy().overCap(); got != "" {
		t.Errorf("under the caps: %q", got)
	}
	// Per-run credit cap.
	p := newProxy()
	p.runSpent = 50
	if got := p.overCap(); !strings.Contains(got, "per-run credit cap $50.00") {
		t.Errorf("run cap: %q", got)
	}
	// Weekly credit cap, overage disabled (the default): refused outright,
	// and the refusal names the cap and the week's spend.
	p = newProxy()
	p.runSpent = 10
	if got := p.overCap(); !strings.Contains(got, "weekly credit cap $20.00") || !strings.Contains(got, "overage disabled, refusing") {
		t.Errorf("weekly cap with overage 0: %q", got)
	}
	// Weekly credit cap with overage allowed: the call goes through, the
	// proxy marks itself as overage, and only an overage already at the cap
	// refuses (the whole call counts, so the check runs before the call).
	p = newProxy()
	p.overageCap = 0.001
	p.runSpent = 10
	if got := p.overCap(); got != "" {
		t.Errorf("overage allowed: %q", got)
	}
	if !p.overage {
		t.Error("overage not marked after the weekly credit was exhausted")
	}
	// No API-priced spend past the credit yet: still allowed.
	if got := p.overCap(); got != "" {
		t.Errorf("overage refused at $0 spent: %q", got)
	}
	p.overageApi = 0.001
	if got := p.overCap(); !strings.Contains(got, "API overage cap $0.00") || !strings.Contains(got, "$0.0010 of API-priced usage") {
		t.Errorf("overage cap: %q", got)
	}
}

// record prices every call twice — credit for the ledger, API price for the
// overage account — and counts a call against the overage cap only once the
// weekly credit is exhausted. Old ledger lines without api_cost_usd parse
// back as 0.
func TestRecordOverageAccounting(t *testing.T) {
	m := mustResolve(t, testModels(t), "zai-org/glm-5.3-flash")
	usage := Usage{PromptTokens: 1000, CompletionTokens: 200}
	usage.PromptTokensDetails = &struct {
		CachedTokens int `json:"cached_tokens"`
	}{CachedTokens: 400}
	// The relay sets usage.Cost before record runs; record reads it for the
	// credit and prices the API cost itself.
	usage.Cost = CostOf(m, usage) // credit $0.000118, API $0.000206
	// The weekly credit runs out during the second call: the third is the
	// first recorded as overage.
	p := &proxy{ledger: filepath.Join(t.TempDir(), "l.jsonl"), runCap: 5, weekCap: 0.0002, overageCap: 0.0002}
<<<<<<< 551588be9ae03bf01220e9e892c4fa0bb2676e2f
	p.record(m, &usage, false, time.Now(), 0, 0, charCounts{}, 0)
	if p.overage || p.overageApi != 0 {
		t.Fatalf("first call counted as overage: overage=%v api=$%v", p.overage, p.overageApi)
	}
	p.record(m, &usage, false, time.Now(), 0, 0, charCounts{}, 0)
	if !p.overage || p.overageApi != 0 {
		t.Fatalf("crossing call: overage=%v api=$%v, want the credit to run out with no overage yet", p.overage, p.overageApi)
	}
	p.record(m, &usage, false, time.Now(), 0, 0, charCounts{}, 0)
=======
	p.record(m, &usage, false, time.Now(), 0, charCounts{}, 0, 0)
	if p.overage || p.overageApi != 0 {
		t.Fatalf("first call counted as overage: overage=%v api=$%v", p.overage, p.overageApi)
	}
	p.record(m, &usage, false, time.Now(), 0, charCounts{}, 0, 0)
	if !p.overage || p.overageApi != 0 {
		t.Fatalf("crossing call: overage=%v api=$%v, want the credit to run out with no overage yet", p.overage, p.overageApi)
	}
	p.record(m, &usage, false, time.Now(), 0, charCounts{}, 0, 0)
>>>>>>> ef15a7a752be8b88bd302f5bd39de5c8e4341321
	if p.overageApi != 0.000206 {
		t.Fatalf("overageApi = $%v, want $0.000206", p.overageApi)
	}
	// The cap is checked before the next call: $0.000206 spent past the
	// credit >= the $0.0002 overage cap.
	if got := p.overCap(); !strings.Contains(got, "API overage cap") {
		t.Fatalf("overCap = %q, want the API overage refusal", got)
	}
	lines, err := readLedger(p.ledger)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 {
		t.Fatalf("%d ledger lines", len(lines))
	}
	for i, line := range lines {
		if line.CostUSD != 0.000118 || line.APICostUSD != 0.000206 {
			t.Errorf("line %d: credit $%v api $%v", i, line.CostUSD, line.APICostUSD)
		}
	}
	// An old line, written before the field existed, parses back as 0.
	var old LedgerLine
	if err := json.Unmarshal([]byte(`{"ts":"2026-10-01T00:00:00Z","model":"m","cost_usd":0.5,"stream":false,"usage_seen":true}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.CostUSD != 0.5 || old.APICostUSD != 0 {
		t.Fatalf("old line = %+v", old)
	}
}

// healthz keeps run_spent_usd (credit now) and reports the week's credit and
// the API-priced overage beside it.
func TestHealthzWeeklyAndOverage(t *testing.T) {
	p := &proxy{weekSpent: 1, overageCap: 0.001}
	p.runSpent = 2
	p.overageApi = 0.5
	server := httptest.NewServer(p)
	defer server.Close()
	response, err := http.Get(server.URL + "/v1/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var health struct {
		OK              bool     `json:"ok"`
		RunSpentUSD     *float64 `json:"run_spent_usd"`
		WeekSpentCredit *float64 `json:"week_spent_credit_usd"`
		OverageAPIUSD   *float64 `json:"overage_api_usd"`
	}
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	if !health.OK || health.RunSpentUSD == nil || *health.RunSpentUSD != 2 ||
		health.WeekSpentCredit == nil || *health.WeekSpentCredit != 3 ||
		health.OverageAPIUSD == nil || *health.OverageAPIUSD != 0.5 {
		t.Fatalf("healthz = %+v", health)
	}
}

// writeQueueModels writes a models.json with one model of the given
// max_concurrent and an optional queue object; an empty queue omits the key.
func writeQueueModels(t *testing.T, queue string, maxConcurrent int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "models.json")
	q := ""
	if queue != "" {
		q = fmt.Sprintf(`,"queue":%s`, queue)
	}
	body := fmt.Sprintf(`{"limits":{"plan":"big","weekly_credit_usd":20,"run_credit_usd":5}%s,
		"models":[{"id":"m","upstream":"hf:m","efforts":["high"],"max_concurrent":%d,
		"input":0.1,"output":0.25,"cache_read":0.02,"api_input":0.15,"api_cached":0.04,"api_output":0.5}]}`,
		q, maxConcurrent)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLoadConfigQueue covers queue.max_parallel_runs: absent or 0 defaults
// to 2, a negative value is refused, and a value whose per-run slots would
// sum past a model's account-wide max_concurrent is refused.
func TestLoadConfigQueue(t *testing.T) {
	for _, queue := range []string{"", `{"max_parallel_runs":2}`, `{"max_parallel_runs":0}`} {
		cfg, err := loadConfig(writeQueueModels(t, queue, 2))
		if err != nil || cfg.Queue.MaxParallelRuns != 2 {
			t.Fatalf("queue %q: max_parallel_runs = %d, %v; want 2", queue, cfg.Queue.MaxParallelRuns, err)
		}
	}
	// An explicit value is kept, and exactly at the model's limit fits.
	cfg, err := loadConfig(writeQueueModels(t, `{"max_parallel_runs":2}`, 2))
	if err != nil || cfg.Queue.MaxParallelRuns != 2 {
		t.Fatalf("queue 2: %d, %v", cfg.Queue.MaxParallelRuns, err)
	}
	// Negative is refused.
	if _, err := loadConfig(writeQueueModels(t, `{"max_parallel_runs":-1}`, 2)); err == nil || !strings.Contains(err.Error(), "must not be negative") {
		t.Fatalf("negative queue: %v", err)
	}
	// Past a model's max_concurrent the per-run slots would sum past the
	// account limit, and the config is refused.
	if _, err := loadConfig(writeQueueModels(t, `{"max_parallel_runs":3}`, 2)); err == nil || !strings.Contains(err.Error(), "sum past the account limit") {
		t.Fatalf("overflowing queue: %v", err)
	}
	// The committed models.json parses and is accepted as committed.
	if _, err := loadConfig("models.json"); err != nil {
		t.Fatalf("committed models.json: %v", err)
	}
}

// TestPerRunSlotsSweep sweeps max_concurrent 1..4 against max_parallel_runs
// 1..4: the per-run slots always number at least 1, and whenever they would
// sum past the model's account-wide max_concurrent, loadConfig refuses the
// config instead.
func TestPerRunSlotsSweep(t *testing.T) {
	for mc := 1; mc <= 4; mc++ {
		for mpr := 1; mpr <= 4; mpr++ {
			slots := perRunSlots(Model{ID: "m", MaxConcurrent: mc}, mpr)
			if slots < 1 {
				t.Errorf("perRunSlots(max_concurrent %d, max_parallel_runs %d) = %d; want at least 1", mc, mpr, slots)
				continue
			}
			_, err := loadConfig(writeQueueModels(t, fmt.Sprintf(`{"max_parallel_runs":%d}`, mpr), mc))
			if sum := slots * mpr; sum <= mc {
				if err != nil {
					t.Errorf("max_concurrent %d, max_parallel_runs %d: %d slots x %d = %d fits, but loadConfig refused: %v", mc, mpr, slots, mpr, sum, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "sum past the account limit") {
				t.Errorf("max_concurrent %d, max_parallel_runs %d: %d slots x %d = %d > %d, but loadConfig accepted", mc, mpr, slots, mpr, sum, mc)
			}
		}
	}
}
