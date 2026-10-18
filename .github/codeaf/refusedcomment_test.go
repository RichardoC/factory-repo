package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRefusedComment(t *testing.T) {
	// A diff carrying every hostile shape at once: a 5-backtick fence that
	// would close ours, a 3-backtick run, an @mention, and more bytes than
	// the 60 KB cap.
	long := strings.Repeat("context line padding the diff past the cap\n", 1600) // ~69 KB
	diff := "@@ -1,2 +1,3 @@\n" +
		"+```md\n" +
		"+`````\n" +
		"+@attacker was here\n" +
		"+```\n" +
		long
	dir := t.TempDir()
	errFile := filepath.Join(dir, "publish-errors.log")
	reason := "workflow policy: codeaf.yml: triggers (`on`) changed\n" +
		"::error::the workflow change breaks the workflow policy (above) and no maintainer approved it; refusing it\n" +
		"reviewed by @maintainer\n"
	if err := os.WriteFile(errFile, []byte(reason), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PUBLISH_ERRORS_PATH", errFile)
	got := refusedComment(diff, publishReason(os.Getenv("PUBLISH_ERRORS_PATH")))

	// The fence is five backticks, opened once and closed once.
	if strings.Count(got, "`````") != 2 {
		t.Errorf("want exactly one 5-backtick fence (open+close) in body:\n%s", got[:min(len(got), 600)])
	}
	// No run of 3+ backticks survives inside the fence.
	fenceStart := strings.Index(got, "`````") + len("`````")
	fenceEnd := strings.LastIndex(got, "`````")
	inside := got[fenceStart:fenceEnd]
	if i := strings.Index(inside, "```"); i >= 0 {
		t.Errorf("a 3-backtick run survives inside the fence at %d: %q", i, inside[max(0, i-40):min(len(inside), i+40)])
	}
	if !strings.Contains(inside, "'''") {
		t.Error("backtick runs were not replaced with three single-quotes")
	}
	// An @ in the diff stays inside the fence (verbatim render, no escape)
	// and the reason's @ is escaped so no mention survives raw.
	if !strings.Contains(inside, "@attacker") {
		t.Error("the diff content is missing from its fence")
	}
	head := got[:strings.Index(got, "`````")]
	if strings.Contains(head, "@maintainer") {
		t.Error("a raw @mention survives outside the fence")
	}
	if !strings.Contains(head, "@\\u200b") && !strings.Contains(head, "@\u200b") {
		t.Errorf("the reason's @ was not escaped with a zero-width space:\n%s", head)
	}
	if !strings.Contains(head, "workflow policy: codeaf.yml: triggers") {
		t.Errorf("the policy reason is missing from the body:\n%s", head)
	}
	// The body is capped: diff over 60 KB is cut with a visible note.
	if n := strings.Count(long[:60], "context line"); n == 0 {
		t.Error("test fixture lost")
	}
	if !strings.Contains(got, "(diff cut at 60000 bytes)") {
		t.Error("the cut note is missing for a diff over the cap")
	}
	if short := refusedComment("short diff\n", ""); strings.Contains(short, "(diff cut at") {
		t.Error("the cut note appears for a diff under the cap")
	}
	// Nothing from the diff may appear outside the fence.
	outside := got[fenceEnd+len("`````"):]
	if strings.Contains(outside, "context line") || strings.Contains(outside, "@attacker") {
		t.Error("diff content leaked outside the fence")
	}
}
