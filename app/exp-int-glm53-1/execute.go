package main

import (
	"fmt"
	"strconv"
)

// Execute performs a decided run and says what it did.
func Execute(d Decision) (string, error) {
	if !d.Run {
		return "", fmt.Errorf("not a run: %s", d.Reason)
	}
	switch d.Mode {
	case "issue":
		return "worked issue #" + strconv.Itoa(d.Number), nil
	case "review":
		return "reviewed PR #" + strconv.Itoa(d.Number), nil
	case "digest":
		// A digest run summarises recent activity; it belongs to no
		// issue or PR, so there is no number to report.
		return "wrote digest", nil
	}
	return "", fmt.Errorf("unknown mode %q", d.Mode)
}
