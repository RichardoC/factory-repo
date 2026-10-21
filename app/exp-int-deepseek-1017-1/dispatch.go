package main

import "strconv"

// State is what the dispatcher knows at one tick.
type State struct {
	Queued         []int // queued issue numbers, oldest first
	InFlight       int   // runs in progress
	DigestInFlight bool  `json:"digest_in_flight"` // a digest run is in progress
}

// Dispatch returns the inputs of every run to start this tick: queued issues,
// oldest first, while there are free slots. Then, last, at most one digest run
// ({"digest": "true"}) if a slot is free on top of the ones the queued issues
// take (cfg.Queue.MaxParallel - st.InFlight - issuesStarted > 0), the queued
// backlog is shorter than cfg.Digest.MinBacklog, and no digest run is already in
// flight.
func Dispatch(cfg Config, st State) []map[string]string {
	var out []map[string]string
	free := cfg.Queue.MaxParallel - st.InFlight
	started := 0
	for _, n := range st.Queued {
		if started >= free {
			break
		}
		out = append(out, map[string]string{"issue": strconv.Itoa(n)})
		started++
	}
	if free-started > 0 && len(st.Queued) < cfg.Digest.MinBacklog && !st.DigestInFlight {
		out = append(out, map[string]string{"digest": "true"})
	}
	return out
}
