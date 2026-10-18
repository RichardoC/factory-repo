package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestWorkflowShellcheck lints every run: block of every workflow under
// ../workflows with shellcheck, so a broken shell line is caught by go test
// instead of by a failed Actions run (issue #135).
//
// The exact shellcheck invocation is part of the contract:
//
//	shellcheck -s bash -S warning -e SC2016,SC2024,SC2296
//
// SC2016 is excluded because run: blocks legitimately single-quote Actions
// expressions such as '${{ ... }}'; SC2296 because a run: block may use an
// Actions expression directly inside a double-quoted string, e.g.
// target="${{ steps.pr.outputs.number }}", which is valid YAML/Actions and
// only looks like a parameter expansion to shellcheck (same class as SC2016);
// and SC2024 because the scripts were checked as a unit, not a sourced file.
// When shellcheck reports anything,
// the test fails with the offending step's script text in the message: the
// step lives inside workflow YAML, which this test deliberately must not
// edit, so the quoted text is what lets a maintainer find and fix the step
// (findings on workflow files are reported back, never patched here).
func TestWorkflowShellcheck(t *testing.T) {
	// Both tools are preinstalled on ubuntu-latest; the skip keeps the test
	// runnable on machines that have neither.
	for _, tool := range []string{"yq", "shellcheck"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not on PATH (both are on ubuntu-latest): %v", tool, err)
		}
	}

	workflowFiles, err := filepath.Glob(filepath.Join("..", "workflows", "*.yml"))
	if err != nil {
		t.Fatalf("listing ../workflows/*.yml: %v", err)
	}
	if len(workflowFiles) == 0 {
		t.Fatal("no workflow files found under ../workflows")
	}

	for _, wf := range workflowFiles {
		wf := wf
		t.Run(filepath.Base(wf), func(t *testing.T) {
			// One JSON string per run: block, in workflow order.
			out, err := exec.Command("yq", "-o=json", ".jobs[].steps[] | select(.run) | .run", wf).Output()
			if err != nil {
				t.Fatalf("yq on %s: %v", wf, err)
			}
			dec := json.NewDecoder(bytes.NewReader(out))
			for i := 0; ; i++ {
				var script string
				if err := dec.Decode(&script); err != nil {
					if err == io.EOF {
						break
					}
					t.Fatalf("decoding run block %d of %s: %v", i, wf, err)
				}
				dir := t.TempDir()
				path := filepath.Join(dir, "step.sh")
				body := "#!/usr/bin/env bash\n" + script
				if !strings.HasSuffix(body, "\n") {
					body += "\n"
				}
				if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
					t.Fatalf("writing step script: %v", err)
				}
				out, err := exec.Command("shellcheck", "-s", "bash", "-S", "warning", "-e", "SC2016,SC2024,SC2296", path).CombinedOutput()
				if err == nil {
					continue
				}
				// A finding is an ExitError from shellcheck; anything else is
				// the tool itself failing, which is fatal, not a lint result.
				if _, ok := err.(*exec.ExitError); !ok {
					t.Fatalf("shellcheck on run block %d of %s: %v\n%s", i, wf, err, out)
				}
				// The step text is quoted in full: this test cannot edit the
				// workflow, so this message is the maintainer's pointer to it.
				t.Errorf("shellcheck found issues in run block %d of %s (findings are reported, workflow files are not edited by this test).\n--- step text ---\n%s\n--- shellcheck output ---\n%s", i, wf, script, out)
			}
		})
	}
}
