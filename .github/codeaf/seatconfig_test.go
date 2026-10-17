package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// seatConfigStdout runs `harness seat-config LANE` against the fixture files
// the caller pointed HARNESS_DIR and PROXY_MODELS at, and returns what it
// printed on stdout.
func seatConfigStdout(t *testing.T, lane string) (string, error) {
	t.Helper()
	oldArgs := os.Args
	os.Args = []string{"harness", "seat-config", lane}
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
	models := testModels(t)
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
		// Every committed model has max_concurrent 2, so the per-run slice
		// at max_parallel_runs 2 is 1.
		if parallel, ok := config["task.parallel"].(float64); !ok || parallel != 1 {
			t.Fatalf("lane %q: task.parallel = %v; want 1", c.lane, config["task.parallel"])
		}
		if _, ok := config["models.crew.task_cap"]; !ok {
			t.Fatalf("lane %q: the config lost keys: %v", c.lane, config)
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
