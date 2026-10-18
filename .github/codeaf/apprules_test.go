// apprules_test.go covers the per-app rules embedded in the briefs
// (issue #139): the worker's and the review run's brief carry the AGENTS.md
// of every app the issue names, capped; the planner carries every app's.
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// appModuleDir points HARNESS_DIR at this module's directory, where the
// standing instructions live; the tests chdir away to a temp working
// directory, so a relative HARNESS_DIR would not find them.
func appModuleDir(t *testing.T) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARNESS_DIR", wd)
}

// writeAppRules creates app/<name>/AGENTS.md with rules in the (temp) working
// directory.
func writeAppRules(t *testing.T, name, rules string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join("app", name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("app", name, "AGENTS.md"), []byte(rules), 0o644); err != nil {
		t.Fatal(err)
	}
}

// runIssueBriefEnv points the environment at the fake API and temp files for
// one runBrief call; the caller has already chdir'd to the working directory
// the brief reads app rules from. It returns the brief as written.
func runIssueBriefEnv(t *testing.T, routes map[string]string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if body, ok := routes[r.URL.Path]; ok {
			_, _ = w.Write([]byte(body))
			return
		}
		http.Error(w, "not served: "+r.URL.Path, http.StatusNotFound)
	}))
	t.Cleanup(server.Close)
	t.Setenv("GITHUB_API_URL", server.URL)
	t.Setenv("GITHUB_REPOSITORY", "RichardoC/factory-repo")
	t.Setenv("GITHUB_TOKEN", "tok")
	t.Setenv("MODE", "issue")
	t.Setenv("NUMBER", "7")
	t.Setenv("CODEAF_BUDGET_MINUTES", "45")
	dir := t.TempDir()
	briefPath := filepath.Join(dir, "brief.md")
	t.Setenv("BRIEF_PATH", briefPath)
	eventPath := filepath.Join(dir, "event.json")
	if err := os.WriteFile(eventPath, []byte(issueEvent), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITHUB_EVENT_PATH", eventPath)
	out, err := captureStdout(t, runBrief)
	if err != nil {
		t.Fatalf("runBrief: %v", err)
	}
	data, err := os.ReadFile(briefPath)
	if err != nil {
		t.Fatal(err)
	}
	if out != string(data) {
		t.Error("stdout and the brief file differ")
	}
	return string(data)
}

func issueRoutes(labels string) map[string]string {
	return map[string]string{
		"/repos/RichardoC/factory-repo/issues/7": fmt.Sprintf(
			`{"number":7,"title":"The task","body":"Do the thing","state":"open","comments":0,"labels":[%s]}`, labels),
		"/repos/RichardoC/factory-repo/issues/7/comments": `[]`,
	}
}

func TestBriefEmbedsAppRules(t *testing.T) {
	appModuleDir(t)
	section := "## Rules for app/x (app/x/AGENTS.md)"

	t.Run("labelled_app_issue", func(t *testing.T) {
		t.Chdir(t.TempDir())
		writeAppRules(t, "x", "Build with cgo and Go 1.27.2.\nRun the four checks.\n")
		brief := runIssueBriefEnv(t, issueRoutes(`{"name":"area:app/x"}`))
		if !strings.Contains(brief, section) {
			t.Errorf("brief missing the app rules section:\n%s", briefSection(brief, "Task: issue #7"))
		}
		if !strings.Contains(brief, "Run the four checks.") {
			t.Error("the app's AGENTS.md contents are not embedded")
		}
	})

	t.Run("harness_issue", func(t *testing.T) {
		t.Chdir(t.TempDir())
		writeAppRules(t, "x", "Run the four checks.\n")
		brief := runIssueBriefEnv(t, issueRoutes(`{"name":"area:harness"}`))
		if strings.Contains(brief, "## Rules for app/") {
			t.Errorf("a harness issue must carry no app rules section:\n%s", brief)
		}
	})

	t.Run("one_mebibyte_capped", func(t *testing.T) {
		t.Chdir(t.TempDir())
		writeAppRules(t, "x", strings.Repeat("b", 1<<20))
		brief := runIssueBriefEnv(t, issueRoutes(`{"name":"area:app/x"}`))
		sec := briefSection(brief, "Rules for app/x (app/x/AGENTS.md)")
		if !utf8.ValidString(sec) {
			t.Error("the capped section is not valid UTF-8")
		}
		if want := fmt.Sprintf("(app/x/AGENTS.md truncated at %d bytes)", maxAppRulesBytes); !strings.Contains(sec, want) {
			t.Errorf("no truncation note %q in section", want)
		}
		if got := strings.Count(sec, "b"); got > maxAppRulesBytes {
			t.Errorf("section holds %d bytes of the file, cap is %d", got, maxAppRulesBytes)
		}
	})
}

func TestReviewBriefEmbedsAppRules(t *testing.T) {
	appModuleDir(t)
	t.Chdir(t.TempDir())
	writeAppRules(t, "x", "Review against the app's own rules.\n")
	routes := map[string]string{
		"/repos/RichardoC/factory-repo/pulls/7":           `{"number":7,"title":"Review target","body":"The change.\n\nCloses #3","state":"open","head":{"ref":"codeaf/issue-3-42"},"base":{"ref":"main"}}`,
		"/repos/RichardoC/factory-repo/issues/3":          `{"number":3,"title":"The task","body":"Build the thing the review judges against.","labels":[{"name":"area:app/x"}]}`,
		"/repos/RichardoC/factory-repo/issues/7/comments": `[]`,
	}
	var sawAccept string
	server := reviewServer(t, routes, "diff --git a/app/x/main.go b/app/x/main.go\n", &sawAccept)
	ghEnv(t, server)
	t.Setenv("NUMBER", "7")
	briefPath := filepath.Join(t.TempDir(), "nested", "review-brief.md")
	t.Setenv("BRIEF_PATH", briefPath)
	if _, err := captureStdout(t, runReviewBrief); err != nil {
		t.Fatalf("runReviewBrief: %v", err)
	}
	data, err := os.ReadFile(briefPath)
	if err != nil {
		t.Fatal(err)
	}
	brief := string(data)
	if !strings.Contains(brief, "## Rules for app/x (app/x/AGENTS.md)") {
		t.Errorf("review brief missing the app rules section:\n%s", briefSection(brief, "The issue this PR closes (#3)"))
	}
	if !strings.Contains(brief, "Review against the app's own rules.") {
		t.Error("the app's AGENTS.md contents are not embedded")
	}
}

func TestPlanBriefEmbedsEveryApp(t *testing.T) {
	appModuleDir(t)
	t.Chdir(t.TempDir())
	writeAppRules(t, "a", "App a builds with cgo.\n")
	writeAppRules(t, "b", "App b is a test fixture.\n")
	if err := os.MkdirAll(filepath.Join("app", "c"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Every route misses: the plan brief degrades the gathered sections to
	// notes, which is exactly the behaviour under test here.
	server := planServer(t, nil, nil)
	ghEnv(t, server)
	briefPath := filepath.Join(t.TempDir(), "nested", "plan-brief.md")
	t.Setenv("BRIEF_PATH", briefPath)
	if _, err := captureStdout(t, runPlanBrief); err != nil {
		t.Fatalf("runPlanBrief: %v", err)
	}
	data, err := os.ReadFile(briefPath)
	if err != nil {
		t.Fatal(err)
	}
	brief := string(data)
	for _, want := range []string{
		"## Rules for app/a (app/a/AGENTS.md)",
		"App a builds with cgo.",
		"## Rules for app/b (app/b/AGENTS.md)",
		"App b is a test fixture.",
		"(app/c/AGENTS.md is unavailable:",
	} {
		if !strings.Contains(brief, want) {
			t.Errorf("plan brief missing %q", want)
		}
	}
}
