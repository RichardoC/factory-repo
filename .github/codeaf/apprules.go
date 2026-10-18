// apprules.go embeds per-app rules in the briefs (issue #139): for every
// area of the form app/<name>, the app's AGENTS.md on the default branch
// travels with the brief, so the worker reads the app's own rules without
// hunting a large README. It is read through the API, not the working tree:
// a pr or review run's tree is the PR head, and a PR must not be able to
// write the rules its own review is judged by. The planner, which serves no
// single issue and runs on the default branch, gets every app's file from
// its tree instead. A missing or unreadable file adds nothing — the file is
// documentation, not an installation error. Standard library only.
package main

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// maxAppRulesBytes caps one app's AGENTS.md in the worker's and the review
// run's brief. The planner's per-app cap is smaller (maxPlanAppRulesBytes,
// planbrief.go) because it embeds every app at once; capBrief bounds the
// whole brief besides.
const maxAppRulesBytes = 16384

// appNameRe bounds the app name an area may carry: the same shape the queue
// accepts for a label (queue.go). An area comes from issue labels and body
// text, and a name like ".." or ".github" must not read outside app/.
var appNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// appRulesSection renders one app's section — the heading and the file's
// contents capped at max bytes — or ok=false when app/<name>/AGENTS.md is
// absent on the default branch, unreadable, or not a file.
func appRulesSection(g github, name string, max int) (string, bool) {
	path := "app/" + name + "/AGENTS.md"
	var file struct {
		Type     string `json:"type"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	// No ref: the contents API reads the default branch.
	if err := g.get("/contents/"+path, &file); err != nil || file.Type != "file" || file.Encoding != "base64" {
		return "", false
	}
	data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(file.Content, "\n", ""))
	if err != nil {
		return "", false
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## Rules for app/%s (%s)\n\n", name, path)
	b.WriteString(capAppFile(string(data), max, path))
	return b.String(), true
}

// appRulesSections renders the per-app rule sections for an area set
// (issueAreas), in the set's order. Areas of any other shape — the harness,
// or an app name that cannot be one — add nothing.
func appRulesSections(g github, areas []string) string {
	var b strings.Builder
	for _, area := range areas {
		name, ok := strings.CutPrefix(area, "app/")
		if !ok || !appNameRe.MatchString(name) {
			continue
		}
		if section, ok := appRulesSection(g, name, maxAppRulesBytes); ok {
			b.WriteString(section + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// capAppFile caps an embedded file at max bytes without splitting a UTF-8
// rune, noting the truncation; the note counts against the cap. It mirrors
// capDiff (review.go), with the file's own name in the note.
func capAppFile(text string, max int, name string) string {
	if len(text) <= max {
		return text
	}
	note := fmt.Sprintf("\n(%s truncated at %d bytes)\n", name, max)
	cut := max - len(note)
	if cut < 0 {
		cut = 0
	}
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + note
}
