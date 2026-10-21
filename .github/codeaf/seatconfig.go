package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	if len(os.Args) != 3 && len(os.Args) != 4 {
		return errors.New("usage: harness seat-config LANE [PROFILE]")
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
	lanes := laneModels(cfg.Models)
	worker := lanes[lane%len(lanes)].ID
	config["models.tiers.worker"] = worker
	// A model experiment (experiments.go) pins all three seats on purpose;
	// that path keeps its seats. The name is validated again here; an
	// unknown or malformed profile fails the run rather than quietly
	// running the default seats.
	if len(os.Args) == 4 && os.Args[3] != "" {
		p, err := experimentProfile(os.Args[3])
		if err != nil {
			return err
		}
		config["models.tiers.worker"] = p.Worker
		config["models.tiers.mastermind"] = p.Planner
		config["models.tiers.high"] = p.Checker
	} else {
		// Issue #181: the planner and checker seats pinned by
		// codeaf-config.json may not run on the lane's worker model, so a
		// run never has worker == planner or all three seats on one model
		// (AGENTS.md, Models and concurrency).
		seat := func(key string) (string, error) {
			id, _ := config[key].(string)
			return seatOffWorker(cfg.Models, id, worker)
		}
		// The mastermind and the high seat may share a model, as the
		// committed config does; each only has to differ from the worker.
		for _, key := range []string{"models.tiers.mastermind", "models.tiers.high"} {
			id, err := seat(key)
			if err != nil {
				return err
			}
			config[key] = id
		}
	}
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

// seatOffWorker keeps a planner or checker seat off the worker's model
// (issue #181): the lane rotation can put the worker seat on the model that
// codeaf-config.json pins those seats to, and a run whose three seats share
// one model has no separate check on it. A seat that already differs from
// the worker stands. One that matches moves to the cheapest model a lane
// may use — laneModels ordered by the API list prices the spend caps are
// enforced at (with the committed files that is zai-org/glm-5.3-flash) —
// and when no other model exists the run fails, exactly as reviewerModel
// (review.go) fails for a reviewer on the worker model.
func seatOffWorker(models []Model, seatID, workerID string) (string, error) {
	if seatID == "" || seatID != workerID {
		return seatID, nil
	}
	ordered := append([]Model(nil), laneModels(models)...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].APIInput+ordered[i].APIOutput < ordered[j].APIInput+ordered[j].APIOutput
	})
	for _, m := range ordered {
		if m.ID != workerID {
			return m.ID, nil
		}
	}
	return "", fmt.Errorf("no allowed model differs from the worker model %q; the planner and checker seats must not run on the worker model", workerID)
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
