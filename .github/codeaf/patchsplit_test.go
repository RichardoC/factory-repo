package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildPatch writes a small `git diff --binary`-shaped fixture: a plain edit
// under app/, an edit under .github/codeaf/ (allowed), a new workflow file
// (refused) and a binary file under app/ carried as a GIT binary patch.
func buildPatch(t *testing.T) []byte {
	t.Helper()
	patch := "diff --git a/app/x/a.go b/app/x/a.go\n" +
		"index 1111111..2222222 100644\n" +
		"--- a/app/x/a.go\n" +
		"+++ b/app/x/a.go\n" +
		"@@ -1 +1,2 @@\n" +
		" package x\n" +
		"+fmt.Println(\"hi\")\n" +
		"diff --git a/.github/codeaf/main.go b/.github/codeaf/main.go\n" +
		"index 3333333..4444444 100644\n" +
		"--- a/.github/codeaf/main.go\n" +
		"+++ b/.github/codeaf/main.go\n" +
		"@@ -1 +1,2 @@\n" +
		" package main\n" +
		"+// touched\n" +
		"diff --git a/.github/workflows/new.yml b/.github/workflows/new.yml\n" +
		"new file mode 100644\n" +
		"index 0000000..5555555\n" +
		"--- /dev/null\n" +
		"+++ b/.github/workflows/new.yml\n" +
		"@@ -0,0 +1 @@\n" +
		"+on: push\n" +
		"diff --git a/app/x/blob.bin b/app/x/blob.bin\n" +
		"new file mode 100644\n" +
		"index 0000000000000000000000000000000000000000..8c7dd922ad47494ca30a461b04f5716a\n" +
		"GIT binary patch\n" +
		"literal 10\n" +
		"KcmZo$)hbC>|E~2A\n" +
		"\n"
	return []byte(patch)
}

func TestPatchSplit(t *testing.T) {
	patch := buildPatch(t)
	dir := t.TempDir()
	patchFile := filepath.Join(dir, "changes.patch")
	keepFile := filepath.Join(dir, "keep.patch")
	refusedFile := filepath.Join(dir, "refused.patch")
	if err := os.WriteFile(patchFile, patch, 0o644); err != nil {
		t.Fatal(err)
	}

	err := patchSplit(patchFile, keepFile, refusedFile)
	if err != nil {
		t.Fatalf("patchSplit: %v", err)
	}
	gotKeep, err := os.ReadFile(keepFile)
	if err != nil {
		t.Fatal(err)
	}
	gotRefused, err := os.ReadFile(refusedFile)
	if err != nil {
		t.Fatal(err)
	}

	// KEEP is the three allowed sections, byte-equal to the input sections.
	wantKeep := []byte("diff --git a/app/x/a.go b/app/x/a.go\n" +
		"index 1111111..2222222 100644\n" +
		"--- a/app/x/a.go\n" +
		"+++ b/app/x/a.go\n" +
		"@@ -1 +1,2 @@\n" +
		" package x\n" +
		"+fmt.Println(\"hi\")\n" +
		"diff --git a/.github/codeaf/main.go b/.github/codeaf/main.go\n" +
		"index 3333333..4444444 100644\n" +
		"--- a/.github/codeaf/main.go\n" +
		"+++ b/.github/codeaf/main.go\n" +
		"@@ -1 +1,2 @@\n" +
		" package main\n" +
		"+// touched\n" +
		"diff --git a/app/x/blob.bin b/app/x/blob.bin\n" +
		"new file mode 100644\n" +
		"index 0000000000000000000000000000000000000000..8c7dd922ad47494ca30a461b04f5716a\n" +
		"GIT binary patch\n" +
		"literal 10\n" +
		"KcmZo$)hbC>|E~2A\n" +
		"\n")
	if !bytes.Equal(gotKeep, wantKeep) {
		t.Errorf("KEEP not byte-equal to the three input sections:\n%s", gotKeep)
	}
	if !strings.Contains(string(gotKeep), "GIT binary patch") {
		t.Errorf("binary section lost from KEEP:\n%s", gotKeep)
	}

	// REFUSED is exactly the workflow section.
	wantRefused := []byte("diff --git a/.github/workflows/new.yml b/.github/workflows/new.yml\n" +
		"new file mode 100644\n" +
		"index 0000000..5555555\n" +
		"--- /dev/null\n" +
		"+++ b/.github/workflows/new.yml\n" +
		"@@ -0,0 +1 @@\n" +
		"+on: push\n")
	if !bytes.Equal(gotRefused, wantRefused) {
		t.Errorf("REFUSED = %q, want exactly the one workflow section", gotRefused)
	}
	if got := sectionsIn(string(gotRefused)); got != 1 {
		t.Errorf("REFUSED has %d sections, want 1", got)
	}
	if got := sectionsIn(string(gotKeep)); got != 3 {
		t.Errorf("KEEP has %d sections, want 3", got)
	}

	// With git on PATH, KEEP must apply cleanly on a matching tree.
	if _, err := exec.LookPath("git"); err == nil {
		repo := t.TempDir()
		run := func(args ...string) {
			t.Helper()
			cmd := exec.Command("git", args...)
			cmd.Dir = repo
			cmd.Env = append(os.Environ(),
				"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
				"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
			}
		}
		run("init", "-q", ".")
		if err := os.MkdirAll(filepath.Join(repo, "app", "x"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(repo, ".github", "codeaf"), 0o755); err != nil {
			t.Fatal(err)
		}
		// The pre-images the KEEP patch expects.
		if err := os.WriteFile(filepath.Join(repo, "app", "x", "a.go"),
			[]byte("package x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, ".github", "codeaf", "main.go"),
			[]byte("package main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		run("add", ".")
		run("commit", "-qm", "base")
		cmd := exec.Command("git", "apply", "--check", keepFile)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("git apply --check of KEEP failed: %v\n%s", err, out)
		}
	}

	// A rename into .github/workflows/ is refused with exit 2 and nothing written.
	renamePatch := "diff --git a/app/x/tool.yml b/.github/workflows/tool.yml\n" +
		"similarity index 100%\n" +
		"rename from app/x/tool.yml\n" +
		"rename to .github/workflows/tool.yml\n"
	if err := os.WriteFile(patchFile, []byte(renamePatch), 0o644); err != nil {
		t.Fatal(err)
	}
	keep2 := filepath.Join(dir, "keep2.patch")
	refused2 := filepath.Join(dir, "refused2.patch")
	err = patchSplit(patchFile, keep2, refused2)
	var ref exitError2
	if err == nil || !errors.As(err, &ref) {
		t.Fatalf("rename into .github/workflows/: err = %v, want exitError2", err)
	}
	if _, statErr := os.Stat(keep2); statErr == nil {
		t.Error("exit-2 refusal wrote a KEEP file")
	}
	if _, statErr := os.Stat(refused2); statErr == nil {
		t.Error("exit-2 refusal wrote a REFUSED file")
	}
}

// sectionsIn counts diff --git headers in a patch.
func sectionsIn(patch string) int {
	n := 0
	for _, line := range strings.Split(patch, "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			n++
		}
	}
	return n
}

// TestPatchSplitExit2 runs runPatchSplit in a subprocess (it calls os.Exit)
// and proves a refusal exits 2 with nothing written. The driver case is the
// one that runs without PATCH_SPLIT_EXIT2_CHILD set; the child case runs the
// command under the recorded os.Args.
func TestPatchSplitExit2(t *testing.T) {
	patch := "diff --git a/app/x/tool.yml b/.github/workflows/tool.yml\n" +
		"similarity index 100%\n" +
		"rename from app/x/tool.yml\n" +
		"rename to .github/workflows/tool.yml\n"
	dir := t.TempDir()
	patchFile, keepFile, refusedFile := filepath.Join(dir, "p.patch"),
		filepath.Join(dir, "keep.patch"), filepath.Join(dir, "refused.patch")
	if err := os.WriteFile(patchFile, []byte(patch), 0o644); err != nil {
		t.Fatal(err)
	}

	if child := os.Getenv("PATCH_SPLIT_EXIT2_CHILD"); child == "1" {
		os.Args = []string{"harness", "patch-split", patchFile, keepFile, refusedFile}
		_ = runPatchSplit() // refuses: exits 2
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run", "^TestPatchSplitExit2$")
	cmd.Env = append(os.Environ(), "PATCH_SPLIT_EXIT2_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("runPatchSplit exited 0, want exit 2\n%s", out)
	}
	if code, ok := err.(*exec.ExitError); !ok || code.ExitCode() != 2 {
		t.Fatalf("exit code = %v, want 2\n%s", err, out)
	}
	for _, f := range []string{keepFile, refusedFile} {
		if _, statErr := os.Stat(f); statErr == nil {
			t.Errorf("%s written despite the exit-2 refusal", filepath.Base(f))
		}
	}
}

// TestPatchSplitTooBig: a patch over the 50 MB cap (a sparse file) is
// refused with exit 2 before anything is read or written.
func TestPatchSplitTooBig(t *testing.T) {
	dir := t.TempDir()
	patchFile := filepath.Join(dir, "big.patch")
	f, err := os.Create(patchFile) // sparse: 50 MB of holes, no real bytes
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := os.Truncate(patchFile, maxPatchSplitBytes+1); err != nil {
		t.Fatal(err)
	}
	keepFile, refusedFile := filepath.Join(dir, "keep.patch"), filepath.Join(dir, "refused.patch")
	err = patchSplit(patchFile, keepFile, refusedFile)
	var ref exitError2
	if !errors.As(err, &ref) {
		t.Fatalf("oversized patch: err = %v, want exitError2", err)
	}
	if _, statErr := os.Stat(keepFile); statErr == nil {
		t.Error("oversized patch wrote a KEEP file")
	}
}

// TestPatchSplitBadHeader: a diff --git header headerPaths cannot parse
// (an unterminated quoted path) refuses the whole patch.
func TestPatchSplitBadHeader(t *testing.T) {
	dir := t.TempDir()
	patchFile := filepath.Join(dir, "p.patch")
	if err := os.WriteFile(patchFile,
		[]byte("diff --git \"unterminated b/also.lost\n--- a/app/x/a.go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	keepFile, refusedFile := filepath.Join(dir, "keep.patch"), filepath.Join(dir, "refused.patch")
	err := patchSplit(patchFile, keepFile, refusedFile)
	var ref exitError2
	if !errors.As(err, &ref) {
		t.Fatalf("unparseable header: err = %v, want exitError2", err)
	}
	if _, statErr := os.Stat(keepFile); statErr == nil {
		t.Error("unparseable header wrote a KEEP file")
	}
}
