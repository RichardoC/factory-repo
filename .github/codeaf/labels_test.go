package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// areaLabelServer records the POSTs to /labels and answers /contents/app
// with the given entries. The handlers below build on the ghEnv convention
// from continue_test.go, which points newGitHub at the fake server.
type areaLabelServer struct {
	mu    sync.Mutex
	posts []map[string]string
}

func (s *areaLabelServer) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/repos/RichardoC/factory-repo/contents/app":
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"name":"kube-sqlite3-vfs","type":"dir"},{"name":"exp-int-seed","type":"dir"},{"name":"README.md","type":"file"}]`)
	case r.Method == http.MethodPost && r.URL.Path == "/repos/RichardoC/factory-repo/labels":
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.posts = append(s.posts, body)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, "{}")
	default:
		http.NotFound(w, r)
	}
}

func TestEnsureAreaLabelsPostsPerAppDirectory(t *testing.T) {
	srv := &areaLabelServer{}
	server := httptest.NewServer(http.HandlerFunc(srv.handle))
	defer server.Close()
	ghEnv(t, server)

	if err := ensureAreaLabels(newGitHub(), func(string, ...any) {}); err != nil {
		t.Fatalf("ensureAreaLabels: %v", err)
	}
	if len(srv.posts) != 2 {
		t.Fatalf("got %d label POSTs, wanted 2: %v", len(srv.posts), srv.posts)
	}
	want := map[string]bool{"area:app/kube-sqlite3-vfs": false, "area:app/exp-int-seed": false}
	for _, post := range srv.posts {
		name, ok := want[post["name"]]
		if !ok {
			t.Fatalf("unexpected label POST %q", post["name"])
		}
		if name {
			t.Fatalf("label POST %q repeated", post["name"])
		}
		want[post["name"]] = true
		if post["color"] != "cccccc" {
			t.Fatalf("label %q: colour %q, wanted cccccc", post["name"], post["color"])
		}
		if wantDesc := "CodeAF area: app/" + strings.TrimPrefix(post["name"], "area:app/"); post["description"] != wantDesc {
			t.Fatalf("label %q: description %q, wanted %q", post["name"], post["description"], wantDesc)
		}
	}
	for name, seen := range want {
		if !seen {
			t.Fatalf("no POST for %q", name)
		}
	}
}

func TestEnsureAreaLabelsTolerates422(t *testing.T) {
	srv := &areaLabelServer{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/repos/RichardoC/factory-repo/labels" {
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
				srv.mu.Lock()
				srv.posts = append(srv.posts, body)
				srv.mu.Unlock()
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			fmt.Fprint(w, `{"message":"already_exists","documentation_url":"https://docs.github.com/rest"}`)
			return
		}
		srv.handle(w, r)
	}))
	defer server.Close()
	ghEnv(t, server)

	// Both POSTs come back already_exists: no error, and the loop carried
	// on to the second directory — both POSTs landed.
	if err := ensureAreaLabels(newGitHub(), func(f string, a ...any) {
		t.Fatalf("unexpected log: %s", fmt.Sprintf(f, a...))
	}); err != nil {
		t.Fatalf("ensureAreaLabels with 422s: %v", err)
	}
	if len(srv.posts) != 2 {
		t.Fatalf("got %d label POSTs, wanted 2: %v", len(srv.posts), srv.posts)
	}
}

func TestEnsureAreaLabelsSwallowsOtherFailures(t *testing.T) {
	var logs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/repos/RichardoC/factory-repo/labels" {
			http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"name":"kube-sqlite3-vfs","type":"dir"},{"name":"exp-int-seed","type":"dir"}]`)
	}))
	defer server.Close()
	ghEnv(t, server)

	// The 500s are logged and swallowed: no error, both POSTs attempted.
	if err := ensureAreaLabels(newGitHub(), func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }); err != nil {
		t.Fatalf("ensureAreaLabels with 500s: %v", err)
	}
	if len(logs) != 2 || !strings.Contains(strings.Join(logs, "\n"), "500") {
		t.Fatalf("wanted both 500s logged, got %q", logs)
	}

	// The same server under runNext: the pick is unaffected by the failed
	// label posts — stdout still carries the three GITHUB_OUTPUT lines.
	t.Setenv("PROXY_MODELS", "models.json")
	t.Setenv("CODEAF_WEEK_SPENT_USD", "1")
	dir := t.TempDir()
	issuesPath := filepath.Join(dir, "issues.json")
	runsPath := filepath.Join(dir, "runs.json")
	if err := os.WriteFile(issuesPath, []byte(`[{"number":3,"state":"open","created_at":"2024-01-01T00:00:00Z","body":"## Done when\n\n- tests pass\n\n## End-to-end check\n\nharness next prints number=3\n","labels":[{"name":"codeaf-queued"},{"name":"area:app/x"}]}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runsPath, []byte(`[{"status":"completed"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := runNextStdout(t, issuesPath, runsPath); err != nil || out != "kind=issue\nnumber=3\nlane=0\n" {
		t.Fatalf("runNext printed %q, %v", out, err)
	}
}
