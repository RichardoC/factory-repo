package main

import "strconv"

// State is what the dispatcher knows at one tick.
type State struct {
	Queued         []int // queued issue numbers, oldest first
	InFlight       int   // runs in progress
	DigestInFlight bool  `json:"digest_in_flight"` // a digest run is in progress
}

// Dispatch returns the inputs of every run to start this tick: queued issues,
// oldest first, while there are free slots. When the backlog is low it then
// starts at most one digest run in a still-free slot.
func Dispatch(cfg Config, st State) []map[string]string {
	var out []map[string]string
	free := cfg.Queue.MaxParallel - st.InFlight
	for _, n := range st.Queued {
		if free <= 0 {
			return out
		}
		out = append(out, map[string]string{"issue": strconv.Itoa(n)})
		free--
	}
	if free > 0 && len(st.Queued) < cfg.Digest.MinBacklog && !st.DigestInFlight {
		out = append(out, map[string]string{"digest": "true"})
	}
	return out
}
