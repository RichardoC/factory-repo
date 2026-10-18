package main

// harness refused-comment REFUSED
//
// The Markdown body a maintainer sees on the PR when patch-split published
// the patch without its .github/ part: what was not published, the policy's
// reason (from the publish-errors log), and the refused diff itself.
//
// Nothing from the diff may reach the comment outside its code fence — a
// crafted diff could otherwise inject Markdown, HTML or an @mention — so the
// whole diff goes inside one fence of five backticks, after any run of three
// or more backticks in it is replaced with three single-quotes (a fence of
// five can only be closed by five, but the replacement keeps even a fence
// counted in a language line or a reader's copy-paste from ending ours), and
// the diff is capped at 60 KB with a visible note when cut.

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

const (
	maxReasonBytes = 2000  // the escapeMD cap on the policy reason
	maxDiffBytes   = 60000 // 60 KB cap on the refused diff
)

var backtickRun = regexp.MustCompile("`{3,}")

// runRefusedComment writes the Markdown comment body for a refused patch to
// stdout: what was not published, the policy's reason, and the refused diff.
func runRefusedComment() error {
	if len(os.Args) != 3 {
		return fmt.Errorf("usage: harness refused-comment REFUSED")
	}
	data, err := os.ReadFile(os.Args[2])
	if err != nil {
		return fmt.Errorf("reading refused patch: %w", err)
	}
	reason := publishReason(os.Getenv("PUBLISH_ERRORS_PATH"))
	_, err = io.WriteString(os.Stdout, refusedComment(string(data), reason))
	return err
}

// publishReason reads the policy reason out of the publish-errors log (the
// PUBLISH_ERRORS_PATH file): the `workflow policy:` lines the apply step
// collected there, or any ::error:: line if the log holds no policy lines
// (the patch may have been refused by another gate). Read as data, capped.
func publishReason(path string) string {
	if path == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 1<<16))
	if err != nil {
		return ""
	}
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "::error::"))
		if line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) > 20 {
		lines = lines[len(lines)-20:]
	}
	return strings.Join(lines, "\n")
}

// refusedComment builds the comment body. The reason arrives already read;
// it is escaped here so an @ in it cannot start a mention and its markup
// cannot structure the comment. The diff is never escaped — it renders
// verbatim inside the fence — but it is neutralised (backtick runs become
// three single-quotes, so nothing in it can end the fence early) and capped.
func refusedComment(diff, reason string) string {
	var b strings.Builder
	b.WriteString("The workflow part of this patch was **not published**: it changes files under `.github/`, which needs a maintainer's approval. Everything else in the patch was applied and is on this PR.\n")
	if r := escapeMD(reason, maxReasonBytes, false); strings.TrimSpace(r) != "" {
		b.WriteString("\nThe policy reason:\n\n> ")
		b.WriteString(strings.ReplaceAll(r, "\n", "\n> "))
		b.WriteString("\n")
	}
	d := backtickRun.ReplaceAllString(diff, "'''")
	cut := false
	if len(d) > maxDiffBytes {
		d = truncateBytes(d, maxDiffBytes)
		cut = true
	}
	b.WriteString("\nThe refused diff:\n\n")
	b.WriteString(fenceBackticks + "\n")
	b.WriteString(d)
	if len(d) > 0 && !strings.HasSuffix(d, "\n") {
		b.WriteString("\n")
	}
	b.WriteString(fenceBackticks + "\n")
	if cut {
		b.WriteString(fmt.Sprintf("\n_(diff cut at %d bytes)_\n", maxDiffBytes))
	}
	return b.String()
}

// truncateBytes cuts s to at most max bytes without splitting a UTF-8 rune.
func truncateBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := s[:max]
	for len(cut) > 0 && (cut[len(cut)-1]&0xC0) == 0x80 {
		cut = cut[:len(cut)-1]
	}
	if len(cut) > 0 && (cut[len(cut)-1]&0xC0) == 0xC0 {
		cut = cut[:len(cut)-1]
	}
	return cut
}
