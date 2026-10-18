package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// maxAppFileSize is the largest file allowed under app/: 1 MiB. A 3.9 MB
// binary once sat committed at app/exp-int-seed/exp-int-seed for a day
// before anyone noticed (issue #139).
const maxAppFileSize = 1 << 20

// elfMagic is the first four bytes of an ELF object; binaries never belong
// in the repository.
var elfMagic = []byte{0x7f, 'E', 'L', 'F'}

// appRequiredFiles are the files AGENTS.md ("Repository layout") requires in
// every app directory.
var appRequiredFiles = []string{"go.mod", "README.md", "LICENSE", "AGENTS.md"}

// checkNoBigOrELF reports the first tracked file that is over 1 MiB or
// starts with the ELF magic. It takes plain paths so tests can run it on
// synthetic trees too.
func checkNoBigOrELF(paths []string) error {
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			return fmt.Errorf("stat %s: %w", p, err)
		}
		if fi.Size() > maxAppFileSize {
			return fmt.Errorf("file %s is %d bytes, over the %d-byte limit", p, fi.Size(), maxAppFileSize)
		}
		f, err := os.Open(p)
		if err != nil {
			return fmt.Errorf("open %s: %w", p, err)
		}
		head := make([]byte, len(elfMagic))
		n, err := io.ReadFull(f, head)
		f.Close()
		if err == nil && bytes.Equal(head, elfMagic) {
			return fmt.Errorf("file %s starts with the ELF magic", p)
		}
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return fmt.Errorf("read %s: %w", p, err)
		}
		_ = n
	}
	return nil
}

// goModModulePath returns the module path of a go.mod file: the second field
// of its `module ` line. ok is false when there is no module line.
func goModModulePath(goMod []byte) (path string, ok bool) {
	for _, line := range strings.Split(string(goMod), "\n") {
		line = strings.TrimSpace(line)
		if rest, found := strings.CutPrefix(line, "module "); found {
			return strings.TrimSpace(rest), true
		}
	}
	return "", false
}

func TestAppLayout(t *testing.T) {
	matches, err := filepath.Glob(filepath.Join("..", "..", "app", "*"))
	if err != nil {
		t.Fatalf("glob app directories: %v", err)
	}
	var dirs []string
	for _, m := range matches {
		if fi, err := os.Stat(m); err == nil && fi.IsDir() {
			dirs = append(dirs, m)
		}
	}
	if len(dirs) == 0 {
		t.Fatal("no app directories found under ../../app")
	}

	// modulePaths maps module path -> owning directory, to detect duplicates.
	modulePaths := map[string]string{}

	for _, dir := range dirs {
		dir := dir
		t.Run(filepath.Base(dir), func(t *testing.T) {
			for _, name := range appRequiredFiles {
				fi, err := os.Stat(filepath.Join(dir, name))
				if err != nil {
					t.Errorf("app %s is missing %s: %v", dir, name, err)
				} else if fi.IsDir() {
					t.Errorf("app %s: %s is a directory, want a regular file", dir, name)
				}
			}

			data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
			if err != nil {
				t.Fatalf("read go.mod: %v", err)
			}
			mod, ok := goModModulePath(data)
			if !ok {
				t.Fatalf("%s/go.mod has no module line", dir)
			}
			if prev, dup := modulePaths[mod]; dup {
				t.Errorf("duplicate module path %q: %s and %s", mod, prev, dir)
			}
			modulePaths[mod] = dir
		})
	}

	t.Run("tracked files", func(t *testing.T) {
		if _, err := exec.LookPath("git"); err != nil {
			t.Skipf("git is not installed: %v", err)
		}
		cmd := exec.Command("git", "ls-files", "-z", "--", "app")
		cmd.Dir = filepath.Join("..", "..")
		out, err := cmd.Output()
		if err != nil {
			t.Skipf("../../ is not a git repository (git ls-files failed: %v)", err)
		}
		var paths []string
		for _, p := range bytes.Split(out, []byte{0}) {
			if len(p) > 0 {
				paths = append(paths, filepath.Join("..", "..", string(p)))
			}
		}
		if len(paths) == 0 {
			t.Fatal("no tracked files under app/")
		}
		if err := checkNoBigOrELF(paths); err != nil {
			t.Errorf("tracked files under app/: %v", err)
		}
	})

	t.Run("oversize detection", func(t *testing.T) {
		dir := t.TempDir()
		small := filepath.Join(dir, "small.txt")
		if err := os.WriteFile(small, []byte("ok\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		big := filepath.Join(dir, "big.bin")
		if err := os.WriteFile(big, make([]byte, 2<<20), 0o644); err != nil {
			t.Fatal(err)
		}
		elf := filepath.Join(dir, "tiny.elf")
		if err := os.WriteFile(elf, append([]byte{}, elfMagic...), 0o644); err != nil {
			t.Fatal(err)
		}

		err := checkNoBigOrELF([]string{small, big})
		if err == nil {
			t.Fatal("checkNoBigOrELF returned nil for a tree with a 2 MiB file")
		}
		if !strings.Contains(err.Error(), "big.bin") {
			t.Errorf("error %q does not name the offending file big.bin", err)
		}

		if err := checkNoBigOrELF([]string{elf}); err == nil {
			t.Fatal("checkNoBigOrELF returned nil for a file with ELF magic")
		} else if !strings.Contains(err.Error(), "tiny.elf") {
			t.Errorf("error %q does not name the offending file tiny.elf", err)
		}

		if err := checkNoBigOrELF([]string{small}); err != nil {
			t.Errorf("checkNoBigOrELF flagged a small, non-ELF file: %v", err)
		}
	})
}
