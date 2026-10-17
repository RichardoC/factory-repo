package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// runSeatConfig implements `harness seat-config LANE` (issue #60): it prints
// the CodeAF config object for the run the queue dispatched on LANE, with
// exactly two values set from models.json. The worker seat becomes the
// lane's model, models[lane mod len(models)], so concurrent runs do not
// compete for the same worker; task.parallel becomes the smallest per-run
// slice any limited model gives (perRunSlots), so the concurrent runs'
// workers never sum past a model's account-wide max_concurrent. The lane is
// untrusted input, validated here a second time after the gate: digits only,
// anything else is lane 0. The issue and the environment are never read —
// the seat choice comes from models.json alone.
func runSeatConfig() error {
	if len(os.Args) != 3 {
		return errors.New("usage: harness seat-config LANE")
	}
	lane := 0
	if digitsOnly(os.Args[2]) {
		if n, err := strconv.Atoi(os.Args[2]); err == nil {
			lane = n
		}
	}
	cfg, err := loadConfig(configPath())
	if err != nil {
		return err
	}
	path := filepath.Join(harnessDir(), "codeaf-config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	config["models.tiers.worker"] = cfg.Models[lane%len(cfg.Models)].ID
	if parallel := minPerRunSlots(cfg.Models, cfg.Queue.MaxParallelRuns); parallel > 0 {
		config["task.parallel"] = parallel
	}
	out, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(out))
	return nil
}

// minPerRunSlots is the smallest slice of any model with a max_concurrent —
// the task.parallel that keeps every model's concurrent workers, summed over
// all runs that may go at once, within its account-wide limit. With no
// limited model it is 0, and the config's own task.parallel stands.
func minPerRunSlots(models []Model, maxParallelRuns int) int {
	best := 0
	for _, m := range models {
		if m.MaxConcurrent > 0 {
			if s := perRunSlots(m, maxParallelRuns); best == 0 || s < best {
				best = s
			}
		}
	}
	return best
}
