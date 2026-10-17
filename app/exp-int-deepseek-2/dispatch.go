package main

import "strconv"

// State is what the dispatcher knows at one tick.
type State struct {
	Queued         []int `json:"queued"`           // queued issue numbers, oldest first
	InFlight       int   `json:"in_flight"`        // runs in progress
	DigestInFlight bool  `json:"digest_in_flight"` // a digest run is already in progress
}

// Dispatch returns the inputs of every run to start this tick: queued issues,
// oldest first, while there are free slots; then, if a slot is still free and
// the issue backlog is below cfg.Digest.MinBacklog and no digest run is
// already in flight, at most one digest run -- which takes a slot like any
// other run.
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
	if free > 0 && len(st.Queued) < cfg.Digest.MinBacklog && !st.DigestInFlight {
		out = append(out, map[string]string{"digest": "true"})
	}
	return out
}
