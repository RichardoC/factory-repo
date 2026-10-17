package main

// harness workflow-policy BEFORE_DIR AFTER_DIR
//
// The deterministic gate on a CodeAF patch that changes .github/workflows/
// without a maintainer's approval. BEFORE_DIR holds the default branch's
// workflows and AFTER_DIR the patched ones, each as `<file>.json` (the
// workflow converted from YAML by yq). The patch may be published (pushed
// with the harness app's token) only if every rule below holds; any
// violation is printed and the command exits non-zero.
//
// Why these rules: a push made with the app token can trigger the pushed
// branch's own workflows (push and pull_request events run the branch's
// version), and those runs get the repository's secrets. The rules make the
// patched workflows unable to do anything the default branch's workflows
// cannot already do, before anyone has reviewed them:
//
//   - the set of workflow files is unchanged (no new or deleted workflows);
//   - each workflow's triggers (`on`) and top-level permissions are
//     unchanged (a workflow_dispatch may gain new inputs, nothing else),
//     and so is a top-level env that references secrets;
//   - every existing job keeps its permissions, environment and reusable
//     workflow (`uses`) unchanged, and no job is removed;
//   - a new job declares permissions that are only read or none, has no
//     environment, calls no reusable workflow and references no secrets;
//   - a step that references secrets is unchanged (it must equal a step of
//     the same job in the default branch), and no other step, job env or
//     job `with` may reference secrets;
//   - every action is pinned to a full commit SHA and already used, at that
//     SHA, somewhere in the default branch's workflows (no new actions);
//   - no new `run:` script interpolates `github.event` (script injection).
//
// What the rules cannot judge — whether a changed `run:` script is correct
// or benign — is left to the independent review run on the PR, and the
// merge stays with a person or Claude acting for the owner.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
)

var pinnedAction = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_./-]+@[0-9a-f]{40}$`)

// loadWorkflowDir reads every `*.json` workflow in dir, keyed by file name.
func loadWorkflowDir(dir string) (map[string]map[string]any, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]any{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		var wf map[string]any
		if err := json.Unmarshal(data, &wf); err != nil || wf == nil {
			return nil, fmt.Errorf("%s: not a workflow object: %v", e.Name(), err)
		}
		out[strings.TrimSuffix(e.Name(), ".json")] = wf
	}
	return out, nil
}

// mentionsSecrets reports whether any string inside v references secrets.
func mentionsSecrets(v any) bool {
	found := false
	walkStrings(v, func(s string) {
		if strings.Contains(s, "secrets.") || strings.Contains(s, "secrets[") {
			found = true
		}
	})
	return found
}

func walkStrings(v any, f func(string)) {
	switch t := v.(type) {
	case string:
		f(t)
	case map[string]any:
		for _, x := range t {
			walkStrings(x, f)
		}
	case []any:
		for _, x := range t {
			walkStrings(x, f)
		}
	}
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

// collectUses gathers every step-level `uses` across workflows.
func collectUses(wfs map[string]map[string]any) map[string]bool {
	uses := map[string]bool{}
	for _, wf := range wfs {
		for _, job := range asMap(wf["jobs"]) {
			for _, step := range asSlice(asMap(job)["steps"]) {
				if u, ok := asMap(step)["uses"].(string); ok {
					uses[u] = true
				}
			}
		}
	}
	return uses
}

// collectEventRuns gathers every `run` script that interpolates github.event.
func collectEventRuns(wfs map[string]map[string]any) map[string]bool {
	runs := map[string]bool{}
	for _, wf := range wfs {
		for _, job := range asMap(wf["jobs"]) {
			for _, step := range asSlice(asMap(job)["steps"]) {
				if r, ok := asMap(step)["run"].(string); ok && strings.Contains(r, "${{") && strings.Contains(r, "github.event") {
					runs[r] = true
				}
			}
		}
	}
	return runs
}

// sameTriggers reports whether two `on` blocks are equal, except that the
// patched one may add new workflow_dispatch inputs. Only someone with write
// access can dispatch a workflow, and an input is data, so a new input
// grants nothing; every existing input and every other trigger must be
// unchanged.
func sameTriggers(after, before any) bool {
	if reflect.DeepEqual(after, before) {
		return true
	}
	a, b := asMap(after), asMap(before)
	if a == nil || b == nil || len(a) != len(b) {
		return false
	}
	for event, bv := range b {
		av, ok := a[event]
		if !ok {
			return false
		}
		if event != "workflow_dispatch" {
			if !reflect.DeepEqual(av, bv) {
				return false
			}
			continue
		}
		ad, bd := asMap(av), asMap(bv)
		for key, value := range bd {
			if key != "inputs" && !reflect.DeepEqual(ad[key], value) {
				return false
			}
		}
		for key := range ad {
			if _, ok := bd[key]; !ok && key != "inputs" {
				return false
			}
		}
		ai, bi := asMap(ad["inputs"]), asMap(bd["inputs"])
		for input, value := range bi {
			if !reflect.DeepEqual(ai[input], value) {
				return false
			}
		}
	}
	return true
}

// onlyReadPermissions reports whether a job's permissions are declared and
// grant nothing beyond read.
func onlyReadPermissions(v any) bool {
	switch t := v.(type) {
	case map[string]any:
		for _, p := range t {
			if s, ok := p.(string); !ok || (s != "read" && s != "none") {
				return false
			}
		}
		return true
	case string:
		return t == "read-all" || t == "{}"
	}
	return false
}

// WorkflowPolicy returns every rule the patched workflows break.
func WorkflowPolicy(before, after map[string]map[string]any) []string {
	var v []string
	add := func(format string, args ...any) { v = append(v, fmt.Sprintf(format, args...)) }

	for name := range after {
		if _, ok := before[name]; !ok {
			add("%s: new workflow files are not allowed without a maintainer", name)
		}
	}
	for name := range before {
		if _, ok := after[name]; !ok {
			add("%s: deleting a workflow is not allowed without a maintainer", name)
		}
	}

	knownUses := collectUses(before)
	knownEventRuns := collectEventRuns(before)

	names := make([]string, 0, len(after))
	for name := range after {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		a, b := after[name], before[name]
		if b == nil {
			continue
		}
		if !sameTriggers(a["on"], b["on"]) {
			add("%s: triggers (`on`) changed", name)
		}
		if !reflect.DeepEqual(a["permissions"], b["permissions"]) {
			add("%s: top-level permissions changed", name)
		}
		if (mentionsSecrets(a["env"]) || mentionsSecrets(b["env"])) && !reflect.DeepEqual(a["env"], b["env"]) {
			add("%s: a top-level env that references secrets changed", name)
		}

		aJobs, bJobs := asMap(a["jobs"]), asMap(b["jobs"])
		for id := range bJobs {
			if _, ok := aJobs[id]; !ok {
				add("%s: job %q removed", name, id)
			}
		}
		jobIDs := make([]string, 0, len(aJobs))
		for id := range aJobs {
			jobIDs = append(jobIDs, id)
		}
		sort.Strings(jobIDs)
		for _, id := range jobIDs {
			aj := asMap(aJobs[id])
			bj, existed := bJobs[id]
			if !existed {
				if !onlyReadPermissions(aj["permissions"]) {
					add("%s: new job %q must declare permissions that are only read or none", name, id)
				}
				if aj["environment"] != nil {
					add("%s: new job %q may not use an environment", name, id)
				}
				if aj["uses"] != nil {
					add("%s: new job %q may not call a reusable workflow", name, id)
				}
				if mentionsSecrets(aj) {
					add("%s: new job %q may not reference secrets", name, id)
				}
			} else {
				bjm := asMap(bj)
				for _, key := range []string{"permissions", "environment", "uses", "secrets"} {
					if !reflect.DeepEqual(aj[key], bjm[key]) {
						add("%s: job %q changed its %s", name, id, key)
					}
				}
				for _, key := range []string{"env", "with"} {
					if (mentionsSecrets(aj[key]) || mentionsSecrets(bjm[key])) && !reflect.DeepEqual(aj[key], bjm[key]) {
						add("%s: job %q changed a %s that references secrets", name, id, key)
					}
				}
			}

			var beforeSteps []any
			if existed {
				beforeSteps = asSlice(asMap(bj)["steps"])
			}
			for i, step := range asSlice(aj["steps"]) {
				sm := asMap(step)
				label := fmt.Sprintf("%s: job %q step %d", name, id, i+1)
				if n, ok := sm["name"].(string); ok {
					label = fmt.Sprintf("%s: job %q step %q", name, id, n)
				}
				if mentionsSecrets(sm) {
					same := false
					for _, bs := range beforeSteps {
						if reflect.DeepEqual(step, bs) {
							same = true
							break
						}
					}
					if !same {
						add("%s references secrets and is new or changed", label)
					}
				}
				if u, ok := sm["uses"].(string); ok {
					switch {
					case strings.HasPrefix(u, "./") || strings.HasPrefix(u, "docker://"):
						add("%s uses %q: local and docker actions are not allowed", label, u)
					case !pinnedAction.MatchString(u):
						add("%s uses %q: actions must be pinned to a full commit SHA", label, u)
					case !knownUses[u]:
						add("%s uses %q: a new action or version; only actions the default branch already uses are allowed", label, u)
					}
				}
				if r, ok := sm["run"].(string); ok && strings.Contains(r, "${{") && strings.Contains(r, "github.event") && !knownEventRuns[r] {
					add("%s interpolates github.event into a run script; pass it through env instead", label)
				}
			}
		}
	}
	sort.Strings(v)
	return v
}

func runWorkflowPolicy() error {
	if len(os.Args) != 4 {
		return errors.New("usage: harness workflow-policy BEFORE_DIR AFTER_DIR")
	}
	before, err := loadWorkflowDir(os.Args[2])
	if err != nil {
		return fmt.Errorf("default branch workflows: %w", err)
	}
	after, err := loadWorkflowDir(os.Args[3])
	if err != nil {
		return fmt.Errorf("patched workflows: %w", err)
	}
	if len(before) == 0 {
		return errors.New("no default-branch workflows to compare against; refusing")
	}
	violations := WorkflowPolicy(before, after)
	for _, line := range violations {
		fmt.Println(line)
	}
	if len(violations) > 0 {
		return fmt.Errorf("the workflow change breaks %d rule(s); it needs a maintainer's approval", len(violations))
	}
	fmt.Println("workflow policy: ok")
	return nil
}
