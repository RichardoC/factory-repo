// closing.go reads how a run ended out of the root task's run record, so a
// run CodeAF finished through its task store (`plandb done`) is not lost:
// such a run's JSON envelope says `stop: incomplete` with an empty answer,
// while the record's final `end` line carries reason "finished in the store"
// and the real answer in `result` (internal/run/bashworker.go:431 at the
// pin in codeaf-ref; issue #134). One mechanism serves every mode: the run
// record is shipped beside the result in runSummary no matter the mode, and
// every report command reads the closing through effectiveAnswer and
// effectiveStop.
package main

import (
	"encoding/json"
	"os"
	"strings"
)

// storeFinishedReason is the end-line reason CodeAF writes when a run ends
// through its task store: the work is finished and the answer is final.
const storeFinishedReason = "finished in the store"

// closing is how a run ended, as its run record tells it: the last `end`
// line's result (the account the worker gave when its loop ended) and
// reason, and whether that reason says the run finished in the store.
type closing struct {
	Answer        string
	Reason        string
	StoreFinished bool
}

// readClosing reads a run record's ending, as review.go used to read it
// (issues #49 and #111): the last `end` line is the record's last line, so
// the read is capped from the tail, not the head. The record comes from the
// untrusted run job, so it is read as data: capped, one bounded struct per
// line, malformed lines skipped — a line the cap cuts in half is just
// malformed — and only the result and reason strings are taken. A record
// that cannot be read answers with the zero value.
func readClosing(path string) closing {
	file, err := os.Open(path)
	if err != nil {
		return closing{}
	}
	defer file.Close()
	data, err := readRecordTail(file, maxTrajectoryBytes)
	if err != nil {
		return closing{}
	}
	var c closing
	for _, line := range strings.Split(string(data), "\n") {
		// The line kinds and the end line's reason and result are CodeAF's
		// own record shape (internal/run/trajectory.go at the pin).
		var record struct {
			Kind   string `json:"kind"`
			Reason string `json:"reason"`
			Result string `json:"result"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			continue // hostile or truncated line: skipped, never fatal
		}
		if record.Kind != "end" {
			continue
		}
		c.Reason = record.Reason // the last end line is the final ending
		c.StoreFinished = record.Reason == storeFinishedReason
		if record.Result != "" {
			c.Answer = record.Result
		}
	}
	return c
}

// effectiveAnswer is the run's real answer: the result envelope's answer
// when it carries one, else the closing result from the run record (the
// store ending leaves the envelope's answer empty).
func effectiveAnswer(result Result, c closing) string {
	if result.Answer != "" {
		return result.Answer
	}
	return c.Answer
}

// effectiveStop is the run's real stop reason. A run that finished in the
// store reports `stop: incomplete` in its envelope only because the store,
// not CodeAF's loop, ended the run: when the record's end line confirms it,
// the stop is promoted from incomplete to done.
//
// The record is written by the agent, so it is untrusted data, and the
// promotion is deliberately narrow: it only turns a draft pull request into
// a ready one and skips an automatic continuation. The independent review
// still runs on the work, and `publish` still applies the patch with every
// check; nothing here marks work verified.
func effectiveStop(result Result, c closing) string {
	stop := sanitizeStop(result.Stop)
	if stop == stopIncomplete && c.StoreFinished {
		return stopDone
	}
	return stop
}
