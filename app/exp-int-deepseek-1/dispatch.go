package main

import "strconv"

// State is what the dispatcher knows at one tick.
type State struct {
	Queued         []int // queued issue numbers, oldest first
	InFlight       int   // runs in progress
	DigestInFlight bool  `json:"digest_in_flight"` // a digest run is already going
}

// Dispatch returns the inputs of every run to start this tick: queued issues,
// oldest first, while there are free slots, then at most one digest run on any
// slot still free.
func Dispatch(cfg Config, st State) []map[string]string {
	var out []map[string]string
	// free is the slots left; starting a queued issue takes one, and the count
	// that remains after the loop is what the digest may use.
	free := cfg.Queue.MaxParallel - st.InFlight
	queued := 0
	for _, n := range st.Queued {
		if free <= 0 {
			break
		}
		out = append(out, map[string]string{"issue": strconv.Itoa(n)})
		free--
		queued++
	}
	// The digest uses spare capacity: only with a slot still free after every
	// queued issue has been started, a backlog below the threshold, and no
	// digest already going. A backlog the slots did not drain leaves none.
	if queued == len(st.Queued) && free > 0 &&
		len(st.Queued) < cfg.Digest.MinBacklog && !st.DigestInFlight {
		out = append(out, map[string]string{"digest": "true"})
	}
	return out
}
