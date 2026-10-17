package main

import "strconv"

// State is what the dispatcher knows at one tick.
type State struct {
	Queued         []int // queued issue numbers, oldest first
	InFlight       int   // runs in progress
	DigestInFlight bool  // true while a digest run is in progress
}

// Dispatch returns the inputs of every run to start this tick: queued issues,
// oldest first, while there are free slots, and then at most one digest run if
// spare capacity is left and the backlog is low.
func Dispatch(cfg Config, st State) []map[string]string {
	var out []map[string]string
	free := cfg.Queue.MaxParallel - st.InFlight
	for _, n := range st.Queued {
		if free <= 0 {
			break
		}
		out = append(out, map[string]string{"issue": strconv.Itoa(n)})
		free--
	}
	// Spare capacity, a backlog below MinBacklog and no digest already running:
	// start one digest run.
	if free > 0 && len(st.Queued) < cfg.Digest.MinBacklog && !st.DigestInFlight {
		out = append(out, map[string]string{"digest": "true"})
	}
	return out
}
