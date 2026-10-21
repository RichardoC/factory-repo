package main

import "testing"

func TestExecuteDigest(t *testing.T) {
	got, err := Execute(Decision{Run: true, Mode: "digest"})
	if err != nil || got != "wrote digest" {
		t.Fatalf("Execute = %q, %v", got, err)
	}
}

func TestExecuteDigestIgnoresNumber(t *testing.T) {
	// Decide leaves Number at 0 for a digest run; any Number must not matter.
	got, err := Execute(Decision{Run: true, Mode: "digest", Number: 9})
	if err != nil || got != "wrote digest" {
		t.Fatalf("Execute = %q, %v", got, err)
	}
}

func TestExecuteDigestNotARun(t *testing.T) {
	if got, err := Execute(Decision{Run: false, Mode: "digest", Reason: "digest must be true"}); err == nil {
		t.Fatalf("Execute = %q, want an error", got)
	}
}

func TestExecuteUnknownMode(t *testing.T) {
	if got, err := Execute(Decision{Run: true, Mode: "nonsense"}); err == nil {
		t.Fatalf("Execute = %q, want an error", got)
	}
}
