package main

// Model experiments: the same task run with different seat models, to tell
// what is down to the model from what is down to the harness. An experiment
// is an issue labelled codeaf-experiment, dispatched with a `profile` input
// that names one of the profiles in experiments.json. A profile pins all
// three seats; the run gets no independent review and no continuation, so
// each result is one profile's unaided work.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// experimentLabel marks an issue that runs only through a profile dispatch.
const experimentLabel = "codeaf-experiment"

var profileName = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// Profile is one seat setup: a model id from models.json for each seat.
type Profile struct {
	Worker  string `json:"worker"`
	Planner string `json:"planner"`
	Checker string `json:"checker"`
}

func experimentsPath() string {
	return envOr("CODEAF_EXPERIMENTS", filepath.Join(harnessDir(), "experiments.json"))
}

// experimentProfile validates a profile name (untrusted: it comes from a
// dispatch input) and returns its seats, each resolved to a models.json id.
func experimentProfile(name string) (Profile, error) {
	if !profileName.MatchString(name) {
		return Profile{}, fmt.Errorf("profile %q is not a valid name", name)
	}
	data, err := os.ReadFile(experimentsPath())
	if err != nil {
		return Profile{}, err
	}
	var file struct {
		Profiles map[string]Profile `json:"profiles"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return Profile{}, fmt.Errorf("experiments.json: %w", err)
	}
	p, ok := file.Profiles[name]
	if !ok {
		return Profile{}, fmt.Errorf("no profile %q in experiments.json", name)
	}
	cfg, err := loadConfig(configPath())
	if err != nil {
		return Profile{}, err
	}
	for _, seat := range []*string{&p.Worker, &p.Planner, &p.Checker} {
		m, ok := Resolve(cfg.Models, *seat)
		if !ok {
			return Profile{}, fmt.Errorf("profile %q names %q, which models.json does not allow", name, *seat)
		}
		*seat = m.ID
	}
	return p, nil
}

// laneModels are the models a lane may pick as its worker: every model but
// those kept for experiments.
func laneModels(models []Model) []Model {
	var out []Model
	for _, m := range models {
		if m.Role != "experiment" {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		return models
	}
	return out
}
