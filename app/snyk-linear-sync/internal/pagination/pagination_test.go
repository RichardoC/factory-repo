package pagination

import (
	"strings"
	"testing"
)

func TestNextCursorOrErrorReturnsAdvancingCursor(t *testing.T) {
	next, err := NextCursorOrError("cursor-1", "cursor-2", "test loop", 2)
	if err != nil {
		t.Fatalf("NextCursorOrError() error = %v", err)
	}
	if next != "cursor-2" {
		t.Fatalf("NextCursorOrError() = %q, want cursor-2", next)
	}
}

func TestNextCursorOrErrorAllowsFirstPage(t *testing.T) {
	// The first page was fetched with no cursor ("after" empty), so no
	// comparison applies yet; any next cursor is an advance.
	next, err := NextCursorOrError("", "cursor-1", "test loop", 1)
	if err != nil {
		t.Fatalf("NextCursorOrError() error = %v", err)
	}
	if next != "cursor-1" {
		t.Fatalf("NextCursorOrError() = %q, want cursor-1", next)
	}
}

func TestNextCursorOrErrorNamesLoopAndPageWhenStuck(t *testing.T) {
	_, err := NextCursorOrError("cursor-1", "cursor-1", "test loop", 3)
	if err == nil {
		t.Fatal("NextCursorOrError() error = nil, want a did-not-advance error")
	}
	if !strings.Contains(err.Error(), "did not advance") {
		t.Fatalf("error %q does not mention the cursor did not advance", err)
	}
	if !strings.Contains(err.Error(), "test loop") || !strings.Contains(err.Error(), "page 3") {
		t.Fatalf("error %q does not name the loop and page", err)
	}
}
