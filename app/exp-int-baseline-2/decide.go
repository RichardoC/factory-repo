package main

import "strconv"

// Event is a dispatch: the inputs the dispatcher (or a person) sent.
type Event struct {
	Inputs map[string]string `json:"inputs"`
}

// Decision is what the gate makes of an event.
type Decision struct {
	Run    bool
	Mode   string // "issue" or "review"
	Number int
	Reason string
}

func skip(reason string) Decision { return Decision{Reason: reason} }

func number(s string) (int, bool) {
	if s == "" || len(s) > 9 {
		return 0, false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	return n, err == nil && n > 0
}

// Decide turns an event into a run, or a skip with a reason. Exactly one of
// the `issue` and `review` inputs must be a positive number, or the `digest`
// input must be exactly "true" and alone.
func Decide(ev Event) Decision {
	issue, review, digest := ev.Inputs["issue"], ev.Inputs["review"], ev.Inputs["digest"]
	switch {
	case digest != "" && (issue != "" || review != ""):
		return skip("digest is exclusive with issue and review")
	case digest != "":
		if digest != "true" {
			return skip("digest must be true")
		}
		return Decision{Run: true, Mode: "digest", Reason: "digest dispatch"}
	case issue != "" && review != "":
		return skip("issue and review are exclusive")
	case issue != "":
		n, ok := number(issue)
		if !ok {
			return skip("issue must be a positive number")
		}
		return Decision{Run: true, Mode: "issue", Number: n, Reason: "issue dispatch"}
	case review != "":
		n, ok := number(review)
		if !ok {
			return skip("review must be a positive number")
		}
		return Decision{Run: true, Mode: "review", Number: n, Reason: "review dispatch"}
	}
	return skip("dispatch needs a numeric issue or review input")
}
