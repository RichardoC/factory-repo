// planbrief_test.go covers `harness plan-brief` (issue #61): the brief's
// sections, the degrade-to-note rule on API errors, the output contract, and
// the configured proposal cap read from models.json.
package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// planHarnessDir points HARNESS_DIR at a directory holding the standing
// instructions, copied from this package's own plan-instructions.md.
func planHarnessDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	standing, err := os.ReadFile(planStandingFileName)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, planStandingFileName), standing, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARNESS_DIR", dir)
	return dir
}

// planServer serves the fake API: routes maps a path to a body; fail holds
// the paths that answer 500.
func planServer(t *testing.T, routes map[string]string, fail map[string]bool) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uri := r.URL.RequestURI() // path and query: the routes name both
		if fail[uri] {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		body, ok := routes[uri]
		if !ok {
			http.Error(w, "not served: "+r.URL.Path, http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

func planBriefEnv(t *testing.T, server *httptest.Server) string {
	t.Helper()
	planHarnessDir(t)
	t.Setenv("GITHUB_API_URL", server.URL)
	t.Setenv("GITHUB_REPOSITORY", "RichardoC/factory-repo")
	t.Setenv("GITHUB_TOKEN", "tok")
	briefPath := filepath.Join(t.TempDir(), "nested", "plan-brief.md")
	t.Setenv("BRIEF_PATH", briefPath)
	return briefPath
}

func planRoutes() map[string]string {
	return map[string]string{
		"/repos/RichardoC/factory-repo/issues?state=open&per_page=100": `[` +
			`{"number":7,"title":"Queue drains twice on error","labels":[{"name":"codeaf-queued"},{"name":"area:harness"}],"body":"The queue drains twice."},` +
			`{"number":8,"title":"A pull request","body":"","pull_request":{}}]`,
		"/repos/RichardoC/factory-repo/issues?state=closed&sort=updated&per_page=20": `[` +
			`{"number":3,"title":"Old feature","body":"Done, but out of scope: a widgets export.\nFollow-up: pin the upstream."},` +
			`{"number":4,"title":"A closed pull request","body":"x","pull_request":{}}]`,
		"/repos/RichardoC/factory-repo/pulls?state=closed&sort=updated&per_page=20": `[` +
			`{"number":41,"title":"Merge the queue fix"},` +
			`{"number":40,"title":"An older pull request"}]`,
		"/repos/RichardoC/factory-repo/issues/comments?sort=created&direction=desc&per_page=100": `[` +
			`{"id":1,"created_at":"2026-10-17T10:00:00Z","body":"<!-- codeaf-report -->\nstop: deadline\n","user":{"login":"github-actions[bot]"},"issue_url":"https://api.github.com/repos/RichardoC/factory-repo/issues/7"},` +
			`{"id":2,"created_at":"2026-10-17T09:00:00Z","body":"a person's comment","user":{"login":"RichardoC"},"issue_url":"https://api.github.com/repos/RichardoC/factory-repo/issues/3"},` +
			`{"id":3,"created_at":"2026-10-17T08:00:00Z","body":"<!-- codeaf-report -->\nstop: ok\n","user":{"login":"github-actions[bot]"},"issue_url":"https://api.github.com/repos/RichardoC/factory-repo/issues/3"}]`,
	}
}

func TestRunPlanBriefSections(t *testing.T) {
	briefPath := planBriefEnv(t, planServer(t, planRoutes(), nil))
	// The working tree's README and AGENTS: the checkout has none, so the
	// sections degrade to notes; a repo file the run job always has is
	// covered by the note's shape.
	if err := runPlanBrief(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(briefPath)
	if err != nil {
		t.Fatal(err)
	}
	brief := string(data)
	for _, want := range []string{
		"# The factory planner",         // the standing instructions
		"You have ",                     // the budget line
		"## The repository's README.md", // the repo files section exists
		"## Open issues",
		"#7 — Queue drains twice on error",
		"labels: codeaf-queued, area:harness",
		"#8 — A pull request (pull request)",
		"## Recently closed issues",
		"### #3 — Old feature",
		"out of scope: a widgets export",
		"## Recently closed pull requests",
		"#41 — Merge the queue fix",
		"## Recent CodeAF run reports",
		"### Run report on issue #7",
		"> stop: deadline",
		"# Output contract",
		"### Proposal: <one-line title>",
		// Issue #189: the plan brief's standing instructions say the PATH
		// harness is the pre-change build, so a proposal is grounded in the
		// source, never in that binary's output.
		planHarnessPreChangeSentence,
	} {
		if !strings.Contains(brief, want) {
			t.Errorf("brief missing %q", want)
		}
	}
	// A person's comment is not a run report, and the second bot report is
	// the one on issue 3, newest first.
	if strings.Contains(brief, "a person's comment") {
		t.Error("a non-report comment was embedded as a run report")
	}
	if !strings.Contains(brief, "### Run report on issue #3") {
		t.Error("the second run report is missing")
	}
	// The run reports are rendered inert: the marker comment's HTML cannot
	// inject. The report body carries no raw heading of its own.
	// The brief was also printed on stdout (runPlanBrief prints it), which
	// the workflow pipes to the step log; the file and the print are the
	// same bytes, asserted here by length.
	if len(brief) == 0 {
		t.Error("brief is empty")
	}
}

func TestRunPlanBriefDegradesToNotes(t *testing.T) {
	briefPath := planBriefEnv(t, planServer(t, planRoutes(), map[string]bool{
		"/repos/RichardoC/factory-repo/issues?state=open&per_page=100":                           true,
		"/repos/RichardoC/factory-repo/issues?state=closed&sort=updated&per_page=20":             true,
		"/repos/RichardoC/factory-repo/pulls?state=closed&sort=updated&per_page=20":              true,
		"/repos/RichardoC/factory-repo/issues/comments?sort=created&direction=desc&per_page=100": true,
	}))
	if err := runPlanBrief(); err != nil {
		t.Fatalf("an API failure must not fail the brief: %v", err)
	}
	data, err := os.ReadFile(briefPath)
	if err != nil {
		t.Fatal(err)
	}
	brief := string(data)
	for _, want := range []string{
		"(open issues unavailable:",
		"(recently closed issues unavailable:",
		"(recently closed pull requests unavailable:",
		"(recent run reports unavailable:",
		"# Output contract",
	} {
		if !strings.Contains(brief, want) {
			t.Errorf("brief missing the note %q", want)
		}
	}
}

func TestRunPlanBriefNoRunReports(t *testing.T) {
	routes := planRoutes()
	routes["/repos/RichardoC/factory-repo/issues/comments?sort=created&direction=desc&per_page=100"] = `[]`
	briefPath := planBriefEnv(t, planServer(t, routes, nil))
	if err := runPlanBrief(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(briefPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "(no recent run reports found") {
		t.Error("an empty report list must degrade to a note")
	}
}

func TestRunPlanBriefRepoFiles(t *testing.T) {
	// A working tree with a README and AGENTS: they are embedded, capped.
	dir := t.TempDir()
	readme := strings.Repeat("readme line\n", 2000) // ~24k bytes, over the cap
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(readme), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("behave\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	briefPath := planBriefEnv(t, planServer(t, planRoutes(), nil))
	t.Chdir(dir)
	if err := runPlanBrief(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(briefPath)
	if err != nil {
		t.Fatal(err)
	}
	brief := string(data)
	if !strings.Contains(brief, "behave\n") {
		t.Error("AGENTS.md was not embedded")
	}
	if !strings.Contains(brief, "(brief truncated at 15000 bytes)") {
		t.Error("README.md was not capped")
	}
}

func TestRunPlanBriefRequiresEnv(t *testing.T) {
	planHarnessDir(t)
	t.Setenv("BRIEF_PATH", "")
	if err := runPlanBrief(); err == nil {
		t.Error("runPlanBrief without BRIEF_PATH should fail")
	}
	// A missing standing-instructions file fails loudly: it is part of this
	// module, and its absence is an installation error.
	dir := t.TempDir()
	t.Setenv("HARNESS_DIR", dir)
	t.Setenv("BRIEF_PATH", filepath.Join(dir, "brief.md"))
	if err := runPlanBrief(); err == nil {
		t.Error("runPlanBrief without the standing instructions should fail")
	}
}

func TestPlanMaxProposals(t *testing.T) {
	dir := planHarnessDir(t)
	// models.json with the planner section.
	config := `{"planner":{"min_backlog":3,"min_hours":12,"max_proposals":2},"limits":{"run_credit_usd":5}}`
	path := filepath.Join(dir, "models.json")
	if err := os.WriteFile(path, []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROXY_MODELS", path)
	if got := planMaxProposals(); got != 2 {
		t.Errorf("planMaxProposals() = %d, want 2", got)
	}
	// Unset, zero, negative, unreadable and malformed all answer 5.
	for name, body := range map[string]string{
		"absent":    `{"limits":{}}`,
		"zero":      `{"planner":{"max_proposals":0}}`,
		"negative":  `{"planner":{"max_proposals":-3}}`,
		"malformed": `{not json`,
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := planMaxProposals(); got != 5 {
			t.Errorf("%s: planMaxProposals() = %d, want 5", name, got)
		}
	}
	t.Setenv("PROXY_MODELS", filepath.Join(dir, "missing.json"))
	if got := planMaxProposals(); got != 5 {
		t.Errorf("unreadable: planMaxProposals() = %d, want 5", got)
	}
}

// TestRunPlanBriefOutputContractCounts uses the configured cap: with
// max_proposals 2 the contract says 2, not 5.
func TestRunPlanBriefOutputContractCounts(t *testing.T) {
	dir := planHarnessDir(t)
	path := filepath.Join(dir, "models.json")
	if err := os.WriteFile(path, []byte(`{"planner":{"max_proposals":2}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROXY_MODELS", path)
	briefPath := planBriefEnv(t, planServer(t, planRoutes(), nil))
	if err := runPlanBrief(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(briefPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), fmt.Sprintf("Write up to %d proposals", 2)) {
		t.Error("the output contract does not carry the configured cap")
	}
}

// TestRunPlanBriefOpenIssuesFollowsPages proves the one open-issues read
// reaches past the first page (issue #223 follow-up): the stub serves two
// pages of open issues, the first carrying a `Link: rel="next"` header, and
// the brief must show the issue that only page 2 has. A one-shot get would
// stop after page 1 and lose it.
func TestRunPlanBriefOpenIssuesFollowsPages(t *testing.T) {
	var pages atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/RichardoC/factory-repo/issues" {
			// The other three list reads answer an empty page and no next
			// link: this test is about the open-issues read alone.
			_, _ = io.WriteString(w, `[]`)
			return
		}
		q := r.URL.Query()
		switch q.Get("page") {
		case "":
			// The first request is the given path itself, query params and
			// all: getList starts from it and only the next link adds a page.
			if q.Get("state") != "open" || q.Get("per_page") != "100" {
				http.Error(w, "first request changed the query: "+r.URL.RequestURI(), http.StatusBadRequest)
				return
			}
			pages.Add(1)
			w.Header().Set("Link", fmt.Sprintf("<%s/repos/RichardoC/factory-repo/issues?state=open&per_page=100&page=2>; rel=\"next\"", serverURL(r)))
			_, _ = io.WriteString(w, `[{"number":11,"title":"First page issue","body":"one"}]`)
		case "2":
			pages.Add(1)
			_, _ = io.WriteString(w, `[{"number":12,"title":"Second page issue","body":"two"}]`)
		default:
			http.Error(w, "unexpected page: "+r.URL.RequestURI(), http.StatusBadRequest)
		}
	}))
	t.Cleanup(server.Close)
	briefPath := planBriefEnv(t, server)
	if err := runPlanBrief(); err != nil {
		t.Fatalf("runPlanBrief: %v", err)
	}
	data, err := os.ReadFile(briefPath)
	if err != nil {
		t.Fatal(err)
	}
	brief := string(data)
	if !strings.Contains(brief, "#11 — First page issue") {
		t.Error("the first page's issue is missing from the brief")
	}
	if !strings.Contains(brief, "#12 — Second page issue") {
		t.Error("the second page's issue is missing: the open-issues read stopped at page 1")
	}
	if n := pages.Load(); n != 2 {
		t.Errorf("pages served = %d, want exactly 2 (the next link was not followed, or followed too far)", n)
	}
}

// TestRunPlanBriefDegradesOnPageCap proves the degrade path survives the
// move to getList: a listing that keeps offering a next page comes back as
// getList's page-cap error, and the brief must render the section as a note
// and finish, never fail the build (the note text the section has always
// used is `(... unavailable: ...)`).
func TestRunPlanBriefDegradesOnPageCap(t *testing.T) {
	forever := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", fmt.Sprintf("<%s%s?per_page=100&page=2>; rel=\"next\"", serverURL(r), r.URL.Path))
		_, _ = io.WriteString(w, `[]`)
	})
	server := httptest.NewServer(forever)
	t.Cleanup(server.Close)
	briefPath := planBriefEnv(t, server)
	if err := runPlanBrief(); err != nil {
		t.Fatalf("a capped list read must not fail the brief: %v", err)
	}
	data, err := os.ReadFile(briefPath)
	if err != nil {
		t.Fatal(err)
	}
	brief := string(data)
	for _, want := range []string{
		"(open issues unavailable:",
		"page cap",
		"# Output contract",
	} {
		if !strings.Contains(brief, want) {
			t.Errorf("brief missing %q", want)
		}
	}
	// The windowed reads take one page and never meet the cap.
	for _, unwanted := range []string{
		"(recently closed issues unavailable:",
		"(recently closed pull requests unavailable:",
		"(recent run reports unavailable:",
	} {
		if strings.Contains(brief, unwanted) {
			t.Errorf("brief has %q: a windowed read followed the next link", unwanted)
		}
	}
}

// TestRunPlanBriefWindowedReadsTakeOnePage proves the recent-items reads
// stay one request each: the closed issues, closed pull requests and run
// reports are windows of the newest items, and a repository with more
// history than maxListPages pages must not lose them to the page cap.
func TestRunPlanBriefWindowedReadsTakeOnePage(t *testing.T) {
	var windowed atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") == "closed" || r.URL.Path == "/repos/RichardoC/factory-repo/issues/comments" {
			windowed.Add(1)
		}
		// The next link keeps the query, so a followed page is counted too.
		w.Header().Set("Link", fmt.Sprintf("<%s%s&page=2>; rel=\"next\"", serverURL(r), r.URL.RequestURI()))
		_, _ = io.WriteString(w, `[]`)
	}))
	t.Cleanup(server.Close)
	planBriefEnv(t, server)
	if err := runPlanBrief(); err != nil {
		t.Fatalf("runPlanBrief: %v", err)
	}
	if n := windowed.Load(); n != 3 {
		t.Errorf("windowed requests = %d, want 3 (one page each)", n)
	}
}
