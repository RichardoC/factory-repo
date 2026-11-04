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
// after is the cursor the current page was fetched with, next is the cursor
// the server offers for the following page, and page is the 1-based number
// of the page just fetched. Only the first page is exempt, because it was
// fetched before any cursor existed (or from a caller's starting cursor).
// From page 2 on, a next equal to after is stuck even when both are empty:
// a server that answers "more pages" with an empty cursor would otherwise
// re-fetch the first page forever.
func NextCursorOrError(after, next, label string, page int) (string, error) {
	if page > 1 && next == after {
		return "", fmt.Errorf("%s: pagination cursor did not advance after page %d", label, page)
	}
	return next, nil
}
