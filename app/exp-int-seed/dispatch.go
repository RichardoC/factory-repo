package main

import "strconv"

// State is what the dispatcher knows at one tick.
type State struct {
	Queued   []int // queued issue numbers, oldest first
	InFlight int   // runs in progress
}

// Dispatch returns the inputs of every run to start this tick: queued issues,
// oldest first, while there are free slots.
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
	return out
}
