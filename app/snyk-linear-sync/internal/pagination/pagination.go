// Package pagination guards Linear and Snyk pagination loops against a
// cursor that never advances. A server response that repeats the same
// cursor with a "more pages" marker (an endCursor with hasNextPage true,
// or a links.next carrying the same starting_after) would otherwise re-fetch
// the same page forever.
package pagination

import "fmt"

// NextCursorOrError returns next when it advances past after, and an error
// naming the loop (label) and page otherwise. A cursor that does not advance
// would loop forever. The run context may carry no deadline, so this is
// guarded explicitly rather than relying on cancellation to break out.
//
// after is the cursor the current page was fetched with ("" on the first
// page), next is the cursor the server offers for the following page. An
// empty next is handled by the caller's own end-of-pages check; it can never
// repeat a non-empty after, so it passes through unchanged.
func NextCursorOrError(after, next, label string, page int) (string, error) {
	if after != "" && next == after {
		return "", fmt.Errorf("%s: pagination cursor did not advance after page %d", label, page)
	}
	return next, nil
}
