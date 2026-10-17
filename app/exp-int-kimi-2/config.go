package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// Queue is how the dispatcher fills run slots.
type Queue struct {
	MaxParallel int `json:"max_parallel"` // runs at once; default 2
}

// Digest is when the dispatcher spends spare capacity on a digest run.
type Digest struct {
	MinBacklog int `json:"min_backlog"` // start a digest run only below this queue length; default 2
}

// Config is jobq.json.
type Config struct {
	MaxRetries int    `json:"max_retries"` // default 3
	Queue      Queue  `json:"queue"`
	Digest     Digest `json:"digest"`
}

// loadConfig reads path and fills defaults. A negative value is an error.
func loadConfig(path string) (Config, error) {
	var cfg Config
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	return withDefaults(cfg)
}

func withDefaults(cfg Config) (Config, error) {
	switch {
	case cfg.MaxRetries == 0:
		cfg.MaxRetries = 3
	case cfg.MaxRetries < 0:
		return cfg, fmt.Errorf("max_retries must not be negative")
	}
	switch {
	case cfg.Queue.MaxParallel == 0:
		cfg.Queue.MaxParallel = 2
	case cfg.Queue.MaxParallel < 0:
		return cfg, fmt.Errorf("queue.max_parallel must not be negative")
	}
	switch {
	case cfg.Digest.MinBacklog == 0:
		cfg.Digest.MinBacklog = 2
	case cfg.Digest.MinBacklog < 0:
		return cfg, fmt.Errorf("digest.min_backlog must not be negative")
	}
	return cfg, nil
}
