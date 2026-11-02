package main

// TestWorkflowPermissionsCoverAPIUse checks a whole class of bug that no test
// caught before (issues #132, #174, #177): a workflow job whose `run:` scripts
// call the GitHub API with a scope the job's `permissions:` block does not
// grant. GitHub returns what the token may read and silently omits the rest --
// the `issues` endpoint leaves pull requests out of its results for a token
// that cannot read them -- so the job does not fail, it just sees less than it
// thinks. Every other test here feeds hand-made JSON, and a developer's own
// token reads PRs, so nothing noticed until the queue stopped picking up PRs
// (#174) and review runs never started.
//
// The workflow YAML is read as text, not through a YAML library: the file is
// the artifact under test, and the shapes that matter (`permissions:` inside a
// job, a gh call inside a `run:` block) are line-oriented. For each job the
// test takes the job's own `permissions:` block, else the workflow's top-level
// one, then walks the job's `run:` scripts for gh calls and checks each one
// against the fixed table below.
//
// The table is deliberately small and explicit. A gh subcommand the table does
// not know is a test failure, not a pass, so a newly added call cannot slip
// past by being unfamiliar.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// apiNeed is one requirement a call places on a job: a permission and the
// access it needs, e.g. {"pull-requests", "read"}.
type apiNeed struct {
	permission string
	access     string // "read" or "write"
}

// name renders the requirement the way the failure message and the issue's
// end-to-end check name it: `pull-requests: write`.
func (n apiNeed) name() string { return n.permission + ": " + n.access }

// ghCallRule is one entry of the table: the shape as the workflows spell it
// ("gh pr edit") and the access it needs.
type ghCallRule struct {
	what string
	need apiNeed
}

// ghWord finds a gh invocation in a run: script. It matches gh at the start
// of the script or after a character that can precede a command: whitespace,
// a pipe, a subshell, a command substitution, &, ; or $( . This is not a
// shell parser -- matching the word gh is enough, because the subcommand
// table decides what to do with it.
var ghWord = regexp.MustCompile(`(?:^|[\s|(&;])gh(?:[\s]|$)`)

// readOnlyCalls are the fixed gh call shapes that only read, so they need
// `<permission>: read` rather than write. A verb the table does not name here
// is treated as a write: writing more than necessary in this table would let
// a real write call pass a read-only job (gh run cancel, gh run rerun,
// gh pr merge all exist and write).
var readOnlyCalls = map[string]bool{
	"gh pr view":      true,
	"gh pr list":      true,
	"gh pr diff":      true,
	"gh pr status":    true,
	"gh pr checks":    true,
	"gh issue view":   true,
	"gh issue list":   true,
	"gh label list":   true,
	"gh run list":     true,
	"gh run view":     true,
	"gh run watch":    true,
	"gh run download": true,
}

// ghCallRules is the fixed table of call shapes the workflows use, and the
// access each needs. Shapes not in the table fail the test, so a new call must
// be added here with its scope.
func ghCallRules() []ghCallRule {
	var rules []ghCallRule
	add := func(what string) {
		// Whether a fixed shape reads or writes comes from readOnlyCalls:
		// a shape the table does not mark as a read is treated as a write,
		// so a new write call cannot pass a read-only job unnoticed.
		access := "write"
		if readOnlyCalls[what] {
			access = "read"
		}
		sub := strings.Fields(what)[0]
		if sub == "gh" {
			sub = strings.Fields(what)[1] // the table spells calls as the workflows do
		}
		rules = append(rules, ghCallRule{what: what, need: apiNeed{permissionForSub(sub), access}})
	}
	// The shapes in the table, sorted so the list reads like documentation.
	// A read shape and a write shape sharing a subcommand are both listed:
	// `gh run list` reads and `gh run rerun` writes.
	for _, what := range []string{
		"gh cache delete",
		"gh cache list",
		"gh issue close",
		"gh issue comment",
		"gh issue create",
		"gh issue edit",
		"gh issue list",
		"gh issue reopen",
		"gh issue view",
		"gh label create",
		"gh label delete",
		"gh label edit",
		"gh label list",
		"gh pr checks",
		"gh pr close",
		"gh pr comment",
		"gh pr create",
		"gh pr diff",
		"gh pr edit",
		"gh pr label",
		"gh pr list",
		"gh pr lock",
		"gh pr merge",
		"gh pr ready",
		"gh pr reopen",
		"gh pr review",
		"gh pr status",
		"gh pr unlock",
		"gh pr view",
		"gh run cancel",
		"gh run download",
		"gh run list",
		"gh run rerun",
		"gh run view",
		"gh run watch",
		"gh workflow disable",
		"gh workflow enable",
		"gh workflow run",
	} {
		add(what)
	}
	// Shorthand subcommands the runs use for a read (`gh auth status`,
	// `gh repo view`, `gh status`): no repository scope, so metadata.
	for _, what := range []string{"gh auth status", "gh repo view", "gh status"} {
		add(what)
	}
	return rules
}

// permissionForSub maps a gh subcommand group to the scope it uses. The
// groups that need no scope beyond the token's implicit metadata read fall
// through to "metadata".
func permissionForSub(sub string) string {
	switch sub {
	case "pr":
		return "pull-requests"
	case "issue", "label":
		return "issues"
	case "run", "workflow", "cache":
		return "actions"
	}
	// auth status, repo view, status: no scope beyond the token's implicit
	// metadata read. (`gh api` never reaches this table: apiNeeds reads its
	// path and method directly.)
	return "metadata"
}

// jobsInWorkflow splits a workflow's text into its top-level jobs, in order.
// A job starts at a `  <name>:` line (exactly two spaces of indent) after the
// `jobs:` line, and ends at the next such line.
func jobsInWorkflow(text string) map[string]string {
	jobs := map[string]string{}
	inJobs := false
	name := ""
	var body []string
	flush := func() {
		if name != "" {
			jobs[name] = strings.Join(body, "\n")
		}
	}
	for _, line := range strings.Split(text, "\n") {
		if !inJobs {
			if strings.HasPrefix(line, "jobs:") {
				inJobs = true
			}
			continue
		}
		trimmed := strings.TrimSpace(line)
		jobStart := strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") &&
			strings.HasSuffix(trimmed, ":") && !strings.HasPrefix(trimmed, "#")
		if jobStart {
			flush()
			name = strings.TrimSuffix(trimmed, ":")
			body = nil
			continue
		}
		if name != "" {
			body = append(body, line)
		}
	}
	flush()
	return jobs
}

// topLevelPermissionsBlock returns the workflow's top-level `permissions:`
// block as text, or "" when the workflow declares none.
func topLevelPermissionsBlock(text string) string {
	return blockAfterLine(text, func(line string) bool { return line == "permissions:" })
}

// jobPermissionsBlock returns the job's own `permissions:` block, or "" when
// the job inherits the workflow's top level. The exact four-space indent is
// what distinguishes the job key from a `permissions:` string or comment
// inside a step's script.
func jobPermissionsBlock(job string) string {
	return blockAfterLine(job, func(line string) bool { return strings.HasPrefix(line, "    permissions:") })
}

// blockAfterLine returns the lines under the first line for which want is
// true, at that line's own indentation level: a deeper-indented line belongs
// to a nested block and is skipped, and the block ends at the first line that
// is outdented.
func blockAfterLine(text string, want func(string) bool) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if !want(strings.TrimRight(line, " \t")) {
			continue
		}
		indent := line[:len(line)-len(strings.TrimLeft(line, " "))]
		var body []string
		for _, next := range lines[i+1:] {
			if strings.TrimSpace(next) == "" || strings.HasPrefix(strings.TrimSpace(next), "#") {
				continue
			}
			if !strings.HasPrefix(next, indent+"  ") || strings.HasPrefix(next, indent+"   ") {
				break
			}
			body = append(body, strings.TrimSpace(next))
		}
		return strings.Join(body, "\n")
	}
	return ""
}

// grantedScopes parses a permissions block into scope -> access: `key: value`
// lines with trailing comments removed, plus the read-all/write-all shorthands
// and the `{}` that grants nothing.
func grantedScopes(block string) map[string]string {
	granted := map[string]string{}
	for _, raw := range strings.Split(block, "\n") {
		line := strings.TrimSpace(raw)
		if at := strings.Index(line, "#"); at >= 0 {
			line = strings.TrimSpace(line[:at])
		}
		if line == "" || line == "{}" {
			continue
		}
		switch line {
		case "read-all":
			granted["*"] = "read"
			continue
		case "write-all":
			granted["*"] = "write"
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		granted[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return granted
}

// shellLines splits a run: script into logical command lines. A backslash
// continuation joins two physical lines into one, so `gh issue edit "$n" \`
// and its `--add-label` are a single command.
func shellLines(script string) []string {
	var lines []string
	var current strings.Builder
	for _, raw := range strings.Split(script, "\n") {
		line := raw
		if strings.HasSuffix(strings.TrimRight(line, " \t"), "\\") {
			line = strings.TrimSuffix(strings.TrimRight(line, " \t"), "\\")
			current.WriteString(line)
			current.WriteString(" ")
			continue
		}
		current.WriteString(line)
		lines = append(lines, current.String())
		current.Reset()
	}
	if current.Len() > 0 {
		lines = append(lines, current.String())
	}
	return lines
}

// stripComment removes a trailing shell comment from a command line, so a
// comment that mentions a command call is not scanned as one. A # inside
// single or double quotes is part of a word; a # at the start of a word is a
// comment.
func stripComment(line string) string {
	inSingle, inDouble := false, false
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '#':
			if inSingle || inDouble {
				continue
			}
			if i == 0 || line[i-1] == ' ' || line[i-1] == '\t' {
				return line[:i]
			}
		}
	}
	return line
}

// chunkSep cuts a command line at the shell operators that separate commands
// -- ; | || && -- so one call's words do not run into the next command's.
var chunkSep = regexp.MustCompile(`\s*(?:\|\||&&|[;|])\s*`)

// splitChunks cuts every logical command line of a script into the individual
// commands the line runs.
func splitChunks(script string) []string {
	var out []string
	for _, line := range shellLines(script) {
		line = stripComment(line)
		for _, chunk := range chunkSep.Split(line, -1) {
			if chunk = strings.TrimSpace(chunk); chunk != "" {
				out = append(out, chunk)
			}
		}
	}
	return out
}

// callEnds cuts a chunk at the first word that starts a new shell construct,
// so the text handed to the table is the call itself, not the rest of a
// script line: a call followed by `|| true`, by `; then`, by a redirect or by
// `&` yields just the call.
func callEnds(chunk string) string {
	var kept []string
	for _, f := range strings.Fields(chunk) {
		switch f {
		case "&&", "||", "|", "&", ";", "then", "else", "fi", "do", "done", "if", "while", "for", "esac", ";;":
			return strings.Join(kept, " ")
		}
		if strings.HasPrefix(f, ">") || strings.HasPrefix(f, "<") || strings.HasPrefix(f, "2>") {
			return strings.Join(kept, " ")
		}
		kept = append(kept, f)
	}
	return strings.Join(kept, " ")
}

// ghCallsIn returns the call in each chunk of a script, as the text after the
// gh word, e.g. `pr edit "$N" --remove-label x`.
func ghCallsIn(script string) []string {
	var calls []string
	for _, chunk := range splitChunks(script) {
		loc := ghWord.FindStringIndex(chunk)
		if loc == nil {
			continue
		}
		// ghWord consumes a leading separator and the gh word; a trailing
		// separator is consumed too, so a line that ends at gh is not a call.
		rest := strings.TrimSpace(chunk[loc[1]:])
		if rest == "" {
			continue
		}
		if call := callEnds(rest); call != "" {
			calls = append(calls, call)
		}
	}
	return calls
}

// callWords splits a call's text into words.
func callWords(call string) []string { return strings.Fields(call) }

// scriptsInJob returns the `run:` bodies of a job's steps as YAML text: the
// body of a `run: |` block, or a one-line `run: cmd`.
func scriptsInJob(job string) []string {
	lines := strings.Split(job, "\n")
	var scripts []string
	for i := 0; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], " \t")
		// A step's run: is a mapping key: at the line's key position, either
		// after the `- ` of a list item or on its own indented line. A run:
		// that appears inside a script body is a shell word, not a key, and
		// is ignored -- its indentation is deeper than a key's.
		indent := line[:len(line)-len(strings.TrimLeft(line, " "))]
		rest := line
		restIndent := indent
		if trimmed := strings.TrimLeft(rest, " "); strings.HasPrefix(trimmed, "- ") {
			rest = strings.TrimPrefix(trimmed, "- ")
			restIndent = indent + "  "
		}
		key := strings.TrimLeft(rest, " ")
		if !strings.HasPrefix(key, "run:") {
			continue
		}
		body := strings.TrimSpace(strings.TrimPrefix(key, "run:"))
		if body != "" && !strings.HasPrefix(body, "|") && !strings.HasPrefix(body, ">") {
			scripts = append(scripts, body)
			continue
		}
		// A block: every line indented further than the key belongs to it.
		var lines2 []string
		for _, next := range lines[i+1:] {
			if strings.TrimSpace(next) == "" {
				lines2 = append(lines2, next)
				continue
			}
			if !strings.HasPrefix(next, restIndent+" ") {
				break
			}
			lines2 = append(lines2, next)
		}
		scripts = append(scripts, strings.Join(lines2, "\n"))
	}
	return scripts
}

// needsFor decides what one call requires, with the calling job's context:
// picksPRs is true when the job picks pull requests out of the issues listing
// (the queue, issues #137 and #174), which needs pull-requests read for that
// listing. ok is false when the call's shape is not in the table, which the
// caller turns into a test failure: a new gh call cannot pass unexamined.
func needsFor(call string, picksPRs bool) (apiNeed, bool) {
	words := callWords(call)
	if len(words) == 0 {
		return apiNeed{}, false
	}
	if words[0] == "api" {
		return apiNeeds(words[1:], picksPRs), true
	}
	if len(words) < 2 {
		return apiNeed{}, false
	}
	what := "gh " + words[0] + " " + words[1]
	for _, rule := range ghCallRules() {
		if rule.what == what {
			return rule.need, true
		}
	}
	return apiNeed{}, false
}

// apiNeeds decides what one gh api call needs: the path names the resource
// (`pulls`, `issues`, `actions`, ...) and the access comes from the method --
// GET (the default, including --paginate) reads, any other method or a data
// flag writes.
//
// picksPRs is the calling job's context. A job that asks the issues endpoint
// for open items and picks pull requests out of them (the queue, issues #137
// and #174) needs pull-requests: read for that listing: GitHub omits pull
// requests from the issues endpoint for a token that cannot read them, which
// is how #174's review never started.
func apiNeeds(args []string, picksPRs bool) apiNeed {
	path := ""
	access := "read"
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "--method" || arg == "-X":
			if i+1 < len(args) && !strings.EqualFold(args[i+1], "GET") {
				access = "write"
			}
			i++
		case arg == "--input" || arg == "-f" || arg == "-F" || arg == "--field" || arg == "--raw-field":
			access = "write"
		case strings.HasPrefix(arg, "--"):
			// --paginate, --jq, --slurp, --cache: reads.
		case path == "":
			path = arg
		}
	}
	// The path arrives inside quotes: repos/$GITHUB_REPOSITORY/issues?...
	path = strings.Trim(path, "'\"")
	// A query string is not a path, and its & is not a shell separator here.
	if at := strings.Index(path, "?"); at >= 0 {
		path = path[:at]
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) >= 3 && parts[0] == "repos" {
		// repos/<owner>/<repo>/<resource>/...: the owner and repository are
		// two segments (`repos/o/r/pulls`) -- unless the script writes
		// $GITHUB_REPOSITORY, which expands to both and so occupies one
		// (`repos/$GITHUB_REPOSITORY/issues`). The resource is the next
		// segment either way.
		resourceAt := 3
		if strings.HasPrefix(parts[1], "$") {
			resourceAt = 2
		}
		if len(parts) <= resourceAt {
			return apiNeed{"metadata", access}
		}
		resource := parts[resourceAt]
		switch resource {
		case "pulls":
			return apiNeed{"pull-requests", access}
		case "issues":
			if picksPRs {
				return apiNeed{"pull-requests", access}
			}
			return apiNeed{"issues", access}
		case "labels", "milestones", "assignees":
			return apiNeed{"issues", access}
		case "actions", "runs":
			return apiNeed{"actions", access}
		case "contents", "git", "commits", "branches", "tags", "releases", "tarball", "zipball", "compare":
			return apiNeed{"contents", access}
		case "collaborators", "invitations":
			return apiNeed{"metadata", access}
		}
	}
	// A path outside repos (search/issues, user, rate_limit) or a resource
	// whose scope is not modeled: metadata, which every token has.
	return apiNeed{"metadata", access}
}

// scopeGranted reports whether a granted scope map covers a need.
// `metadata: read` is implicit in every token, so it is always granted.
func scopeGranted(granted map[string]string, need apiNeed) bool {
	if need.permission == "metadata" {
		return true
	}
	access, ok := granted[need.permission]
	if !ok {
		access = granted["*"] // read-all / write-all shorthand
	}
	if access == "" || access == "none" {
		return false
	}
	if need.access == "read" {
		return access == "read" || access == "write"
	}
	return access == "write"
}

// checkWorkflow returns the violations in one workflow's text, each naming
// the file, the job, the command and the missing scope. A job that declares
// no permissions inherits the workflow's top level; a job with an empty
// `permissions:` block gets nothing from it.
func checkWorkflow(fileName, text string) []string {
	var violations []string
	jobs := jobsInWorkflow(text)
	names := make([]string, 0, len(jobs))
	for name := range jobs {
		names = append(names, name)
	}
	sort.Strings(names) // stable messages across runs

	top := grantedScopes(topLevelPermissionsBlock(text))
	for _, name := range names {
		job := jobs[name]
		granted := top
		if own := jobPermissionsBlock(job); own != "" {
			granted = grantedScopes(own)
		}
		// Does this job pick pull requests out of the issues listing? The
		// queue does exactly that (issues #137, #174): it reads issues.json,
		// keeps the PRs labelled codeaf-review-needed / codeaf-continue-needed
		// and edits those PRs. GitHub leaves PRs out of the issues endpoint
		// for a token that cannot read them, so the listing itself needs
		// pull-requests: read -- the bug #177 fixed.
		picksPRs := picksPRsFromIssues(job)
		for _, script := range scriptsInJob(job) {
			for _, call := range ghCallsIn(script) {
				need, ok := needsFor(call, picksPRs)
				if !ok {
					violations = append(violations, fmt.Sprintf(
						"%s job %s: unknown gh subcommand %q; add its scopes to the table in workflowpermissions_test.go before using it",
						fileName, name, ghCommandLabel(call)))
					continue
				}
				if scopeGranted(granted, need) {
					continue
				}
				violations = append(violations, fmt.Sprintf(
					"%s job %s: %s needs %s", fileName, name, ghCommandLabel(call), need.name()))
			}
		}
	}
	// One line per distinct problem: a job that edits several labels would
	// otherwise repeat the same message.
	return dedupe(violations)
}

// picksPRsFromIssues reports whether a job picks pull requests out of the
// issues listing (the queue, issues #137 and #174): it asks the issues
// endpoint for open items, keeps their number in a file of its own, and then
// acts on the picked items with gh pr / gh issue calls. GitHub leaves pull
// requests out of that listing for a token that cannot read them, so the
// listing itself needs pull-requests: read -- the gap #177 fixed.
//
// The test is deliberately tied to that shape. codeaf.yml's publish job asks
// the issues endpoint too, but only to dedupe planner proposal titles; it
// never picks PRs from it, so it needs issues read alone.
func picksPRsFromIssues(job string) bool {
	picks := false
	for _, script := range scriptsInJob(job) {
		// The listing must be kept in a file the same job reads again: the
		// queue writes issues.json and hands it to the harness, which picks
		// the PRs.
		consumed := false
		for _, name := range []string{"issues.json", "OPEN_ISSUES_PATH"} {
			if strings.Count(script, name) >= 2 {
				consumed = true
			}
		}
		if !consumed {
			continue
		}
		for _, call := range ghCallsIn(script) {
			if strings.HasPrefix(call, "pr ") {
				picks = true
			}
		}
	}
	return picks
}

// dedupe removes repeated violation lines, preserving order.
func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// ghCommandLabel names a call the way the issue's end-to-end check does:
// the two words after gh for a subcommand call, the path for an api call.
func ghCommandLabel(call string) string {
	words := callWords(call)
	if len(words) == 0 {
		return "gh"
	}
	if words[0] == "api" {
		for _, w := range words[1:] {
			if !strings.HasPrefix(w, "-") {
				return "gh api " + w
			}
		}
		return "gh api"
	}
	if len(words) == 1 {
		return "gh " + words[0]
	}
	return "gh " + words[0] + " " + words[1]
}

// workflowFilesIn lists the workflows the test checks, so subtest names are
// stable and a missing directory is a failure, not a silent pass.
func workflowFilesIn(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	for _, pattern := range []string{"*.yml", "*.yaml"} {
		found, err := filepath.Glob(filepath.Join(dir, pattern))
		if err != nil {
			t.Fatalf("listing %s/%s: %v", dir, pattern, err)
		}
		files = append(files, found...)
	}
	sort.Strings(files)
	return files
}

// TestWorkflowPermissionsCoverAPIUse fails when a workflow job's `run:`
// scripts call the GitHub API with a scope the job does not hold (issue #178).
//
// It checks the committed workflows, and then two temp copies that prove the
// test actually catches the class of bug: the #177 regression (a queue job
// without pull-requests) and an unknown gh subcommand.
func TestWorkflowPermissionsCoverAPIUse(t *testing.T) {
	dir := filepath.Join("..", "workflows")
	files := workflowFilesIn(t, dir)
	if len(files) == 0 {
		t.Fatal("no workflow files found under ../workflows")
	}

	for _, file := range files {
		file := file
		t.Run(filepath.Base(file), func(t *testing.T) {
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("reading %s: %v", file, err)
			}
			for _, v := range checkWorkflow(filepath.Base(file), string(data)) {
				t.Errorf("%s", v)
			}
		})
	}

	// The #177 regression: delete the queue's own `pull-requests: write`
	// line in a temp copy and the check must fail, naming the job, the
	// command and the missing scope -- exactly the issue's end-to-end check.
	t.Run("missing pull-requests fails", func(t *testing.T) {
		src := filepath.Join(dir, "codeaf-queue.yml")
		text, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("reading %s: %v", src, err)
		}
		copyPath := filepath.Join(t.TempDir(), "codeaf-queue.yml")
		if err := os.WriteFile(copyPath, []byte(dropPermissionLine(t, string(text), "pull-requests")), 0o600); err != nil {
			t.Fatalf("writing the temp copy: %v", err)
		}
		copied, err := os.ReadFile(copyPath)
		if err != nil {
			t.Fatalf("reading the temp copy: %v", err)
		}
		violations := checkWorkflow("codeaf-queue.yml", string(copied))
		want := "codeaf-queue.yml job queue: gh pr edit needs pull-requests: write"
		if !containsViolation(violations, want) {
			t.Fatalf("removing the queue's pull-requests permission gave %v, which does not contain %q", violations, want)
		}
		// Every violation the reverted copy produces is about the scope that
		// was removed: the failure is the missing permission, not a parse
		// accident.
		for _, v := range violations {
			if !strings.Contains(v, "pull-requests") {
				t.Errorf("violation does not name the missing scope: %s", v)
			}
		}
	})

	// The queue's issue listing is what picks the PRs, and it is the shape
	// GitHub silently shortens: the listing must need pull-requests: read on
	// its own, not only through the gh pr edits that follow it (#174).
	t.Run("issues listing that picks PRs needs pull-requests read", func(t *testing.T) {
		src := filepath.Join(dir, "codeaf-queue.yml")
		text, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("reading %s: %v", src, err)
		}
		reverted := dropPermissionLine(t, string(text), "pull-requests")
		found := false
		for _, v := range checkWorkflow("codeaf-queue.yml", reverted) {
			if strings.Contains(v, "gh api ") && strings.Contains(v, "needs pull-requests: read") {
				found = true
			}
		}
		if !found {
			t.Fatalf("the queue's issues listing did not need pull-requests: read: %v", checkWorkflow("codeaf-queue.yml", reverted))
		}

		// A job that lists issues without picking PRs out of them needs
		// issues only, so the rule does not over-constrain a job like
		// codeaf.yml's publish job (planner title dedupe).
		plain := strings.Join([]string{
			"name: x",
			"on: push",
			"permissions:",
			"  contents: read",
			"  issues: write",
			"jobs:",
			"  q:",
			"    runs-on: ubuntu-latest",
			"    steps:",
			"      - run: |",
			"          gh api --paginate \"repos/$GITHUB_REPOSITORY/issues?state=open&per_page=100\" > issues.json",
			"          jq -r '.[].title' issues.json",
			"          gh issue edit 1 --add-label x",
		}, "\n")
		plainPath := filepath.Join(t.TempDir(), "plain.yml")
		if err := os.WriteFile(plainPath, []byte(plain), 0o600); err != nil {
			t.Fatalf("writing the temp copy: %v", err)
		}
		plainCopy, err := os.ReadFile(plainPath)
		if err != nil {
			t.Fatalf("reading the temp copy: %v", err)
		}
		if vs := checkWorkflow("plain.yml", string(plainCopy)); len(vs) != 0 {
			t.Fatalf("a job that lists issues without picking PRs was flagged: %v", vs)
		}
	})

	// A gh subcommand the table does not know must fail the test, so a new
	// call cannot slip past by being unfamiliar.
	t.Run("unknown gh subcommand fails", func(t *testing.T) {
		src := filepath.Join(dir, "codeaf-queue.yml")
		text, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("reading %s: %v", src, err)
		}
		// The copied line goes into the run: block of the queue job, at
		// the same indentation as the surrounding script.
		body := string(text)
		marker := "          \"$HARNESS\" gh-retry -- gh workflow run codeaf.yml --ref \"$DEFAULT_BRANCH\" -f plan=true -f \"lane=$LANE\"" // issue #429 wrapped the dispatch
		if !strings.Contains(body, marker) {
			t.Fatalf("the queue job's plan dispatch line moved; update this subtest's anchor")
		}
		body = strings.Replace(body, marker, "          gh frobnicate --now\n"+marker, 1)
		copyPath := filepath.Join(t.TempDir(), "codeaf-queue.yml")
		if err := os.WriteFile(copyPath, []byte(body), 0o600); err != nil {
			t.Fatalf("writing the temp copy: %v", err)
		}
		copied, err := os.ReadFile(copyPath)
		if err != nil {
			t.Fatalf("reading the temp copy: %v", err)
		}
		violations := checkWorkflow("codeaf-queue.yml", string(copied))
		found := false
		for _, v := range violations {
			if strings.Contains(v, "unknown gh subcommand") && strings.Contains(v, "frobnicate") {
				found = true
			}
		}
		if !found {
			t.Fatalf("an unknown gh subcommand did not fail the check: %v", violations)
		}
	})
}

// dropPermissionLine removes the first `scope:` line of a permissions block,
// the way deleting #177's fix would.
func dropPermissionLine(t *testing.T, text, scope string) string {
	t.Helper()
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	dropped := false
	for _, line := range lines {
		if !dropped && strings.HasPrefix(strings.TrimSpace(line), scope+":") {
			dropped = true
			continue
		}
		out = append(out, line)
	}
	if !dropped {
		t.Fatalf("no %s: line to remove from the workflow copy", scope)
	}
	return strings.Join(out, "\n")
}

// containsViolation reports whether one violation line contains want.
func containsViolation(violations []string, want string) bool {
	for _, v := range violations {
		if strings.Contains(v, want) {
			return true
		}
	}
	return false
}
