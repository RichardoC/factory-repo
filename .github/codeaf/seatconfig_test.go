package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seatConfigStdout runs `harness seat-config LANE` against the fixture files
// the caller pointed HARNESS_DIR and PROXY_MODELS at, and returns what it
// printed on stdout.
func seatConfigStdout(t *testing.T, args ...string) (string, error) {
	t.Helper()
	oldArgs := os.Args
	os.Args = append([]string{"harness", "seat-config"}, args...)
	defer func() { os.Args = oldArgs }()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStdout := os.Stdout
	os.Stdout = w
	out := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(r)
		out <- string(data)
	}()
	err = runSeatConfig()
	os.Stdout = oldStdout
	w.Close()
	return <-out, err
}

// seatConfigFixture writes a codeaf-config.json and a models.json with two
// limited models of different max_concurrent, and points the command at
// them. The expected task.parallel is the smaller per-run slice, 4/2 = 2.
func seatConfigFixture(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	cfg := `{
  "api_key": "via-local-proxy",
  "models.tiers.worker": "zai-org/glm-5.3-flash",
  "models.tiers.mastermind": "deepseek-ai/deepseek-v4.1-flash",
  "task.parallel": 5,
  "daily_budget_usd": 20,
  "telemetry": false
}`
	if err := os.WriteFile(filepath.Join(dir, "codeaf-config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	models := `{
  "limits": {"plan": "big", "weekly_credit_usd": 20, "run_credit_usd": 5},
  "queue": {"max_parallel_runs": 2},
  "models": [
    {"id": "model-a", "upstream": "hf:a", "max_concurrent": 4, "efforts": ["high"],
     "input": 0.1, "output": 0.25, "cache_read": 0.02, "api_input": 0.15, "api_cached": 0.04, "api_output": 0.5},
    {"id": "model-b", "upstream": "hf:b", "max_concurrent": 6, "efforts": ["high"],
     "input": 0.1, "output": 0.25, "cache_read": 0.02, "api_input": 0.15, "api_cached": 0.04, "api_output": 0.5}
  ]
}`
	if err := os.WriteFile(filepath.Join(dir, "models.json"), []byte(models), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARNESS_DIR", dir)
	t.Setenv("PROXY_MODELS", filepath.Join(dir, "models.json"))
}

func TestSeatConfig(t *testing.T) {
	seatConfigFixture(t)
	for _, c := range []struct {
		lane   string
		worker string
	}{
		{"0", "model-a"},
		{"1", "model-b"},
		{"2", "model-a"}, // wraps by the model count
		{"5", "model-b"},
		{"abc", "model-a"}, // a bad lane is lane 0
		{"", "model-a"},    // an empty lane is lane 0
		{"-1", "model-a"},
		{"1x", "model-a"},
	} {
		t.Run("lane "+c.lane, func(t *testing.T) {
			out, err := seatConfigStdout(t, c.lane)
			if err != nil {
				t.Fatal(err)
			}
			var config map[string]any
			if err := json.Unmarshal([]byte(out), &config); err != nil {
				t.Fatalf("seat-config printed %q: %v", out, err)
			}
			worker, _ := config["models.tiers.worker"].(string)
			if worker != c.worker {
				t.Fatalf("worker = %q; want %q", worker, c.worker)
			}
			// task.parallel is the smallest per-run slice: min(4/2, 6/2) = 2.
			if parallel, ok := config["task.parallel"].(float64); !ok || parallel != 2 {
				t.Fatalf("task.parallel = %v; want 2", config["task.parallel"])
			}
			// Everything else passes through unchanged.
			if config["api_key"] != "via-local-proxy" || config["models.tiers.mastermind"] != "deepseek-ai/deepseek-v4.1-flash" ||
				config["daily_budget_usd"] != float64(20) || config["telemetry"] != false {
				t.Fatalf("the rest of the config changed: %v", config)
			}
		})
	}
}

// TestSeatConfigCommittedConfig: against the committed models.json and
// codeaf-config.json, lane 0 keeps the default worker seat, lane 1 picks the
// next model, and task.parallel drops to the per-run slice.
func TestSeatConfigCommittedConfig(t *testing.T) {
	t.Setenv("HARNESS_DIR", ".")
	t.Setenv("PROXY_MODELS", "models.json")
	models := laneModels(testModels(t))
	for _, c := range []struct {
		lane   string
		worker string
	}{
		{"0", models[0].ID},
		{"1", models[1%len(models)].ID},
		{"9", models[9%len(models)].ID},
		{"bad", models[0].ID},
		{"", models[0].ID},
	} {
		out, err := seatConfigStdout(t, c.lane)
		if err != nil {
			t.Fatal(err)
		}
		var config map[string]any
		if err := json.Unmarshal([]byte(out), &config); err != nil {
			t.Fatalf("seat-config printed %q: %v", out, err)
		}
		if worker, _ := config["models.tiers.worker"].(string); worker != c.worker {
			t.Fatalf("lane %q: worker = %q; want %q", c.lane, worker, c.worker)
		}
		// task.parallel is the per-run slice, whatever the committed files
		// say: the smallest max_concurrent sliced by max_parallel_runs.
		cfg, err := loadConfig("models.json")
		if err != nil {
			t.Fatal(err)
		}
		want := minPerRunSlots(cfg.Models, cfg.Queue.MaxParallelRuns)
		if parallel, ok := config["task.parallel"].(float64); !ok || int(parallel) != want {
			t.Fatalf("lane %q: task.parallel = %v; want %d, the per-run slice at max_parallel_runs %d",
				c.lane, config["task.parallel"], want, cfg.Queue.MaxParallelRuns)
		}
		if _, ok := config["models.crew.task_cap"]; !ok {
			t.Fatalf("lane %q: the config lost keys: %v", c.lane, config)
		}
	}
}

// cheapestOther is the test's own idea of the cheapest model a lane may
// pick that is not skipID: laneModels ordered by the API list prices the
// spend caps are enforced at, in models.json order on a tie. With the
// committed models.json the cheapest lane model is zai-org/glm-5.3-flash.
func cheapestOther(models []Model, skipID string) string {
	var best string
	var bestCost float64
	for _, m := range laneModels(models) {
		if m.ID == skipID {
			continue
		}
		cost := m.APIInput + m.APIOutput
		if best == "" || cost < bestCost {
			best, bestCost = m.ID, cost
		}
	}
	return best
}

// TestSeatConfigDistinctSeats (issue #181): against the committed files,
// every lane's printed worker differs from both the planner and the checker
// seat, all three seats are models.json ids, and a lane whose worker is the
// model the config pins those seats to (lane 1, the deepseek lane) rotates
// them onto the cheapest other lane model. The planner and the checker may
// share a model (issue #222): only the worker has to differ from each.
func TestSeatConfigDistinctSeats(t *testing.T) {
	t.Setenv("HARNESS_DIR", ".")
	t.Setenv("PROXY_MODELS", "models.json")
	cfg, err := loadConfig("models.json")
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{}
	for _, m := range cfg.Models {
		allowed[m.ID] = true
	}
	for lane := 0; lane < cfg.Queue.MaxParallelRuns; lane++ {
		t.Run(fmt.Sprint(lane), func(t *testing.T) {
			out, err := seatConfigStdout(t, fmt.Sprint(lane))
			if err != nil {
				t.Fatal(err)
			}
			var config map[string]any
			if err := json.Unmarshal([]byte(out), &config); err != nil {
				t.Fatalf("seat-config printed %q: %v", out, err)
			}
			seats := map[string]string{}
			for _, key := range []string{"models.tiers.worker", "models.tiers.mastermind", "models.tiers.high"} {
				id, _ := config[key].(string)
				if !allowed[id] {
					t.Errorf("%s = %q, which models.json does not allow", key, id)
				}
				seats[key] = id
			}
			worker, mastermind, high := seats["models.tiers.worker"], seats["models.tiers.mastermind"], seats["models.tiers.high"]
			if worker == mastermind {
				t.Errorf("worker %q and mastermind %q are the same model", worker, mastermind)
			}
			if worker == high {
				t.Errorf("worker %q and high %q are the same model", worker, high)
			}
			if worker == mastermind && worker == high {
				t.Errorf("all three seats are %q", worker)
			}
			// The lane whose worker is the model the config pins the
			// planner and checker to rotates both seats onto the cheapest
			// other lane model. With the committed files that is lane 1
			// (worker deepseek-ai/deepseek-v4.1-flash, seats
			// zai-org/glm-5.3-flash).
			lanes := laneModels(cfg.Models)
			if want := lanes[lane%len(lanes)].ID; worker != want {
				t.Errorf("worker = %q; want the lane model %q", worker, want)
			}
			if mastermind == worker || high == worker {
				want := cheapestOther(cfg.Models, worker)
				if mastermind == worker && mastermind != want {
					t.Errorf("mastermind = %q; want the cheapest other lane model %q", mastermind, want)
				}
				if high == worker && high != want {
					t.Errorf("high = %q; want the cheapest other lane model %q", high, want)
				}
			}
		})
	}
}

// TestSeatConfigLaneOneRotatesPins (issue #181): the committed
// codeaf-config.json pins both the planner and the checker to the model the
// committed rotation gives lane 1, so that lane must print them on the
// cheapest other lane model, zai-org/glm-5.3-flash with the committed
// files, and lane 0, whose worker model differs, keeps the pinned seats.
func TestSeatConfigLaneOneRotatesPins(t *testing.T) {
	t.Setenv("HARNESS_DIR", ".")
	t.Setenv("PROXY_MODELS", "models.json")
	cfg, err := loadConfig("models.json")
	if err != nil {
		t.Fatal(err)
	}
	configData, err := os.ReadFile("codeaf-config.json")
	if err != nil {
		t.Fatal(err)
	}
	var committed map[string]any
	if err := json.Unmarshal(configData, &committed); err != nil {
		t.Fatal(err)
	}
	pinned, _ := committed["models.tiers.mastermind"].(string)
	pinnedHigh, _ := committed["models.tiers.high"].(string)
	lanes := laneModels(cfg.Models)
	want := cheapestOther(cfg.Models, lanes[1%len(lanes)].ID)
	for lane := 0; lane < cfg.Queue.MaxParallelRuns; lane++ {
		c := struct {
			lane                     int
			keepMastermind, keepHigh bool
		}{lane, lanes[lane%len(lanes)].ID != pinned, lanes[lane%len(lanes)].ID != pinnedHigh}
		t.Run(fmt.Sprint(lane), func(t *testing.T) {
			out, err := seatConfigStdout(t, fmt.Sprint(lane))
			if err != nil {
				t.Fatal(err)
			}
			var config map[string]any
			if err := json.Unmarshal([]byte(out), &config); err != nil {
				t.Fatalf("seat-config printed %q: %v", out, err)
			}
			if worker, _ := config["models.tiers.worker"].(string); worker != lanes[lane%len(lanes)].ID {
				t.Fatalf("worker = %q; want %q", worker, lanes[lane%len(lanes)].ID)
			}
			mastermind, _ := config["models.tiers.mastermind"].(string)
			high, _ := config["models.tiers.high"].(string)
			if c.keepMastermind && mastermind != pinned {
				t.Errorf("mastermind = %q; want the pinned %q", mastermind, pinned)
			}
			if c.keepHigh && high != pinnedHigh {
				t.Errorf("high = %q; want the pinned %q", high, pinnedHigh)
			}
			if !c.keepMastermind && mastermind != want {
				t.Errorf("mastermind = %q; want the rotated %q", mastermind, want)
			}
			if !c.keepHigh && high != want {
				t.Errorf("high = %q; want the rotated %q", high, want)
			}
		})
	}
}

// TestSeatConfigRotatesCollidingSeats: with a fixture whose config pins
// both seats to model-b, lane 1's worker (model-b) moves them to model-a,
// the only other lane model, while lane 0 keeps the pins; a model pool of
// one model, with the pins on it, fails the run rather than sharing a seat
// with the worker.
func TestSeatConfigRotatesCollidingSeats(t *testing.T) {
	dir := t.TempDir()
	cfg := `{
  "api_key": "via-local-proxy",
  "models.tiers.worker": "model-a",
  "models.tiers.mastermind": "model-b",
  "models.tiers.high": "model-b",
  "task.parallel": 5,
  "telemetry": false
}`
	if err := os.WriteFile(filepath.Join(dir, "codeaf-config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	models := `{
  "limits": {"plan": "big", "weekly_credit_usd": 20, "run_credit_usd": 5},
  "queue": {"max_parallel_runs": 2},
  "models": [
    {"id": "model-a", "upstream": "hf:a", "max_concurrent": 4, "efforts": ["high"]},
    {"id": "model-b", "upstream": "hf:b", "max_concurrent": 6, "efforts": ["high"]}
  ]
}`
	if err := os.WriteFile(filepath.Join(dir, "models.json"), []byte(models), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARNESS_DIR", dir)
	t.Setenv("PROXY_MODELS", filepath.Join(dir, "models.json"))

	out, err := seatConfigStdout(t, "0")
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal([]byte(out), &config); err != nil {
		t.Fatalf("seat-config printed %q: %v", out, err)
	}
	if id, _ := config["models.tiers.mastermind"].(string); id != "model-b" {
		t.Errorf("lane 0: mastermind = %q; want the pinned model-b", id)
	}
	if id, _ := config["models.tiers.high"].(string); id != "model-b" {
		t.Errorf("lane 0: high = %q; want the pinned model-b", id)
	}

	out, err = seatConfigStdout(t, "1")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(out), &config); err != nil {
		t.Fatalf("seat-config printed %q: %v", out, err)
	}
	if id, _ := config["models.tiers.worker"].(string); id != "model-b" {
		t.Errorf("lane 1: worker = %q; want model-b", id)
	}
	for _, key := range []string{"models.tiers.mastermind", "models.tiers.high"} {
		if id, _ := config[key].(string); id != "model-a" {
			t.Errorf("lane 1: %s = %q; want the rotated model-a", key, id)
		}
	}
}

// TestSeatConfigNoOtherModel: a lane pool of one model, with both seats
// pinned to it, fails the run: the planner and checker may not share the
// worker's model, and there is nothing else to rotate onto.
func TestSeatConfigNoOtherModel(t *testing.T) {
	dir := t.TempDir()
	cfg := `{"models.tiers.worker": "only", "models.tiers.mastermind": "only", "models.tiers.high": "only"}`
	if err := os.WriteFile(filepath.Join(dir, "codeaf-config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	models := `{
  "limits": {"plan": "big", "weekly_credit_usd": 20, "run_credit_usd": 5},
  "queue": {"max_parallel_runs": 2},
  "models": [{"id": "only", "upstream": "hf:o", "max_concurrent": 4, "efforts": ["high"]}]
}`
	if err := os.WriteFile(filepath.Join(dir, "models.json"), []byte(models), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARNESS_DIR", dir)
	t.Setenv("PROXY_MODELS", filepath.Join(dir, "models.json"))

	if _, err := seatConfigStdout(t, "0"); err == nil {
		t.Fatal("seat-config lane 0 kept both seats on the only model")
	}
	// The experiment-profile path still pins all three seats by design
	// (issue #60) and is not held to the rotation.
	if err := os.WriteFile(filepath.Join(dir, "experiments.json"), []byte(`{
  "profiles": {"p": {"worker": "only", "planner": "only", "checker": "only"}}
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := seatConfigStdout(t, "0", "p")
	if err != nil {
		t.Fatalf("seat-config with profile p failed: %v", err)
	}
	var config map[string]any
	if err := json.Unmarshal([]byte(out), &config); err != nil {
		t.Fatalf("seat-config printed %q: %v", out, err)
	}
	for _, key := range []string{"models.tiers.worker", "models.tiers.mastermind", "models.tiers.high"} {
		if id, _ := config[key].(string); id != "only" {
			t.Errorf("profile p: %s = %q; want only", key, id)
		}
	}
}

// TestSeatConfigErrors: unreadable or unparsable files exit non-zero.
func TestSeatConfigErrors(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HARNESS_DIR", dir)
	t.Setenv("PROXY_MODELS", filepath.Join(dir, "models.json"))

	if _, err := seatConfigStdout(t, "1"); err == nil {
		t.Fatal("seat-config accepted a missing models.json")
	}
	if err := os.WriteFile(filepath.Join(dir, "models.json"), []byte(`{"limits":{"plan":"big","weekly_credit_usd":20,"run_credit_usd":5},"models":[{"id":"m","upstream":"hf:m","efforts":["high"]}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := seatConfigStdout(t, "1"); err == nil {
		t.Fatal("seat-config accepted a missing codeaf-config.json")
	}
	if err := os.WriteFile(filepath.Join(dir, "codeaf-config.json"), []byte(`nope`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := seatConfigStdout(t, "1"); err == nil {
		t.Fatal("seat-config accepted an unparsable codeaf-config.json")
	}

	oldArgs := os.Args
	os.Args = []string{"harness", "seat-config"}
	defer func() { os.Args = oldArgs }()
	if err := runSeatConfig(); err == nil {
		t.Fatal("seat-config accepted a missing LANE argument")
	}
}

// configInvariants loads the four committed config files from dir (never
// constants) and returns every invariant between them, so a config change
// that would break a run fails here instead of in a workflow (issue #133).
// The errors carry the file and the key, not just the numbers.
func configInvariants(dir string) []error {
	var errs []error
	errf := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	cfg, err := loadConfig(filepath.Join(dir, "models.json"))
	if err != nil {
		return []error{err}
	}
	ids := map[string]bool{}
	for _, m := range cfg.Models {
		ids[m.ID] = true
		if len(m.Efforts) == 0 {
			errf("models.json: model %q lists no efforts", m.ID)
		}
	}

	// codeaf-config.json: the seats are models.json ids, the worker and the
	// mastermind are different models, and task.parallel stays within the
	// smallest per-run slice any limited model gives at max_parallel_runs.
	cc, err := os.ReadFile(filepath.Join(dir, "codeaf-config.json"))
	if err != nil {
		return append(errs, err)
	}
	var config map[string]any
	if err := json.Unmarshal(cc, &config); err != nil {
		return append(errs, fmt.Errorf("codeaf-config.json: %w", err))
	}
	seat := func(key string) Model {
		id, _ := config[key].(string)
		m, ok := Resolve(cfg.Models, id)
		if !ok {
			errf("codeaf-config.json: %s = %q, which models.json does not allow", key, id)
			return Model{}
		}
		return m
	}
	worker := seat("models.tiers.worker")
	mastermind := seat("models.tiers.mastermind")
	seat("models.tiers.high")
	if worker.ID != "" && mastermind.ID != "" && worker.ID == mastermind.ID {
		errf("codeaf-config.json: the worker is the same model as the mastermind (%s)", worker.ID)
	}
	if parallel, _ := config["task.parallel"].(float64); int(parallel) > minPerRunSlots(cfg.Models, cfg.Queue.MaxParallelRuns) {
		errf("codeaf-config.json: task.parallel %d is past the per-run slice %d (max_parallel_runs %d)",
			int(parallel), minPerRunSlots(cfg.Models, cfg.Queue.MaxParallelRuns), cfg.Queue.MaxParallelRuns)
	}

	// experiments.json: every profile names only models.json ids.
	ex, err := os.ReadFile(filepath.Join(dir, "experiments.json"))
	if err != nil {
		return append(errs, err)
	}
	var experiments struct {
		Profiles map[string]Profile `json:"profiles"`
	}
	if err := json.Unmarshal(ex, &experiments); err != nil {
		return append(errs, fmt.Errorf("experiments.json: %w", err))
	}
	for name, p := range experiments.Profiles {
		for _, seat := range []string{p.Worker, p.Planner, p.Checker} {
			if _, ok := Resolve(cfg.Models, seat); !ok {
				errf("experiments.json: profile %q names %q, which models.json does not allow", name, seat)
			}
		}
	}

	// panel.json: its catalog entries are models.json ids too.
	panel, err := os.ReadFile(filepath.Join(dir, "panel.json"))
	if err != nil {
		return append(errs, err)
	}
	var panelCfg struct {
		Models []struct {
			Slug string `json:"slug"`
		} `json:"models"`
	}
	if err := json.Unmarshal(panel, &panelCfg); err != nil {
		return append(errs, fmt.Errorf("panel.json: %w", err))
	}
	for _, m := range panelCfg.Models {
		if !ids[m.Slug] {
			errf("panel.json: model %q, which models.json does not allow", m.Slug)
		}
	}

	// The lane pool must give every lane a worker seat, distinct-or-rotating:
	// either at least max_parallel_runs models, one seat per lane, or fewer
	// that rotate (seat-config picks pool[lane % len(pool)]); and at least
	// one pool model must have role base or mid, the roles concurrent runs
	// share — a pool with none would put every lane on a seat meant for
	// one-off experiments.
	pool := laneModels(cfg.Models)
	baseMid := 0
	for _, m := range pool {
		if m.Role == "base" || m.Role == "mid" {
			baseMid++
		}
	}
	switch {
	case len(pool) == 0:
		errf("models.json: laneModels is empty; no lane gets a worker seat")
	case len(pool) >= cfg.Queue.MaxParallelRuns:
		// Distinct: every lane gets its own model. The base-or-mid clause
		// below still applies.
	case baseMid < 1:
		errf("models.json: laneModels has %d models for %d lanes and rotates; none of them has role base or mid, which concurrent runs share",
			len(pool), cfg.Queue.MaxParallelRuns)
	}
	if len(pool) > 0 && baseMid < 1 {
		errf("models.json: no lane model has role base or mid")
	}

	// limits and queue: the per-run cap fits inside the week's, and at
	// least one run may go at once.
	if cfg.Limits.RunCreditUSD > cfg.Limits.WeeklyCreditUSD {
		errf("models.json: run_credit_usd %v is past weekly_credit_usd %v",
			cfg.Limits.RunCreditUSD, cfg.Limits.WeeklyCreditUSD)
	}
	if cfg.Queue.MaxParallelRuns < 1 {
		errf("models.json: queue.max_parallel_runs %d is below 1", cfg.Queue.MaxParallelRuns)
	}
	return errs
}

// TestConfigInvariants: the committed config files agree with each other,
// and a config that breaks an invariant fails the test. The second subtest
// runs the checks against a temp copy with task.parallel past the per-run
// slice, so the failure path itself stays tested as the config drifts.
func TestConfigInvariants(t *testing.T) {
	t.Run("committed config", func(t *testing.T) {
		for _, err := range configInvariants(".") {
			t.Error(err)
		}
	})

	t.Run("task.parallel past the per-run slice fails", func(t *testing.T) {
		dir := t.TempDir()
		for _, name := range []string{"models.json", "codeaf-config.json", "experiments.json", "panel.json"} {
			data, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		path := filepath.Join(dir, "codeaf-config.json")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		broken := strings.Replace(string(data), `"task.parallel": 2`, `"task.parallel": 9`, 1)
		if broken == string(data) {
			t.Fatal("codeaf-config.json no longer sets task.parallel 2; update this test")
		}
		if err := os.WriteFile(path, []byte(broken), 0o644); err != nil {
			t.Fatal(err)
		}
		errs := configInvariants(dir)
		if len(errs) == 0 {
			t.Fatal("task.parallel 9 passed the invariants")
		}
		if !strings.Contains(errs[0].Error(), "task.parallel") {
			t.Fatalf("errs[0] = %v; want one naming task.parallel", errs[0])
		}
	})
}
