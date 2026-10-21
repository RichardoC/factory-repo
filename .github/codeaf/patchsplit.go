package main

// harness patch-split PATCH KEEP REFUSED
//
// Splits a `git diff --binary` patch into file sections at each
// `diff --git a/<p> b/<p>` header, and routes them: every section whose
// pre- or post-image path is under .github/ but not under .github/codeaf/
// goes to REFUSED, everything else to KEEP. Sections are kept as raw bytes,
// so the KEEP sections come out byte-identical to the input (binary content
// included) and `git apply` on the KEEP patch works as if only the .github/
// files had never been touched.
//
// The command refuses (exit 2, nothing written to either output) when:
//   - a refused section renames or copies a file (`rename from` /
//     `copy from` lines): a renamed workflow file would leave the old path
//     behind or the new one unpublished, and the policy cannot judge either;
//   - the patch is over 50 MB (the same cap the publish job applies);
//   - a section's `---`/`+++` (or `rename from`/`rename to`, `copy from`/
//     `copy to`) path disagrees with its `diff --git` header: git apply
//     routes by the `---`/`+++` paths, so a header that names a KEEP
//     file cannot be trusted on its own;
//   - a `diff --git` header does not parse (headerPaths returns both paths
//     empty), because the section could then be routed the wrong way.
//
// On success it prints `kept=<n> refused=<n>`.
//
// Bytes before the first `diff --git` header (git diff --binary emits none;
// format-patch and diffstat-adjacent output can) are attached to the first
// section so nothing is silently dropped.

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
)

const maxPatchSplitBytes = 52428800 // 50 MB

// exitError2 marks a policy refusal: the caller prints the message and
// exits 2 instead of the generic log.Fatal's exit 1.
type exitError2 struct{ msg string }

func (e exitError2) Error() string { return e.msg }

func runPatchSplit() error {
	if len(os.Args) != 5 {
		return errors.New("usage: harness patch-split PATCH KEEP REFUSED")
	}
	if err := patchSplit(os.Args[2], os.Args[3], os.Args[4]); err != nil {
		var ref exitError2
		if errors.As(err, &ref) {
			fmt.Fprintln(os.Stderr, "patch-split: "+ref.msg)
			os.Exit(2)
		}
		return err
	}
	return nil
}

// patchSplit does the work of patch-split on paths, so tests can run it
// against a temp dir without os.Args.
func patchSplit(patchFile, keepFile, refusedFile string) error {
	data, err := os.ReadFile(patchFile)
	if err != nil {
		return fmt.Errorf("reading patch: %w", err)
	}
	if len(data) > maxPatchSplitBytes {
		return exitError2{fmt.Sprintf("patch is over 50 MB (%d bytes); refusing it", len(data))}
	}

	type section struct {
		bytes   []byte
		refused bool
	}
	var sections []section
	// Split at every line that starts a file section. The split is on raw
	// bytes: each section is exactly the input bytes for that file, header
	// through the byte before the next header.
	start := 0
	for i := 0; i < len(data); {
		j := bytes.IndexByte(data[i:], '\n')
		var line []byte
		next := len(data)
		if j < 0 {
			line = data[i:]
		} else {
			line = data[i : i+j]
			next = i + j + 1
		}
		if bytes.HasPrefix(line, []byte("diff --git ")) && i > start {
			// A header line that is not the very first bytes of the file
			// ends the section before it — at the byte before the header,
			// so every section keeps its own trailing newline. A header at
			// offset 0 (the normal case) starts section zero.
			sections = append(sections, section{bytes: data[start:i]})
			start = i
		}
		i = next
	}
	if len(data) > 0 {
		sections = append(sections, section{bytes: data[start:]})
	}

	var keep, refused [][]byte
	for _, s := range sections {
		ref, why, err := refusedSection(s.bytes)
		if err != nil {
			return err
		}
		if why != "" {
			return exitError2{why}
		}
		if ref {
			refused = append(refused, s.bytes)
		} else {
			keep = append(keep, s.bytes)
		}
	}

	// Everything is validated; only now is anything written.
	var buf bytes.Buffer
	for _, s := range keep {
		buf.Write(s)
	}
	if err := os.WriteFile(keepFile, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", keepFile, err)
	}
	buf.Reset()
	for _, s := range refused {
		buf.Write(s)
	}
	if err := os.WriteFile(refusedFile, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", refusedFile, err)
	}
	fmt.Printf("kept=%d refused=%d\n", len(keep), len(refused))
	return nil
}

// refusedSection classifies one file section: is it under .github/ outside
// .github/codeaf/, and, if so, does it rename or copy a file? A non-nil
// error return is a refusal reason (exit 2). A header whose paths do not
// parse is a refusal for every section, refused or not: it cannot be
// routed safely either way.
//
// git apply takes a section's pre- and post-image paths from its `---`/
// `+++` lines (and from `rename from`/`rename to`, `copy from`/`copy to`
// when there are none), so the header is not trusted alone: every path
// the section names, header or body, routes it, and a body path that
// disagrees with the header is a refusal the same way an unparseable
// header is, because git apply would route one way while the header
// says the other. Metadata is read only before the first `@@` hunk or
// `GIT binary patch` payload: inside a hunk, a deleted line whose
// content starts with `-- ` is line-shaped like a `---` marker.
func refusedSection(sec []byte) (refused bool, why string, err error) {
	var line []byte
	rest := sec
	meta := true         // still before the hunks: header metadata only
	var pre, post string // the paths the diff --git line names
	for len(rest) > 0 {
		if i := bytes.IndexByte(rest, '\n'); i >= 0 {
			line, rest = rest[:i], rest[i+1:]
		} else {
			line, rest = rest, nil
		}
		switch {
		case bytes.HasPrefix(line, []byte("diff --git ")):
			pre, post = headerPaths(string(line[len("diff --git "):]))
			if pre == "" && post == "" {
				return false, "", exitError2{"a diff --git header does not parse; refusing the patch"}
			}
			if underGithub(pre) || underGithub(post) {
				refused = true
			}
		case !meta:
			// A hunk body or binary payload: nothing later names a path.
		case bytes.HasPrefix(line, []byte("@@")), bytes.HasPrefix(line, []byte("GIT binary patch")):
			meta = false
		case bytes.HasPrefix(line, []byte("--- ")):
			if p := bodyPath(string(line[len("--- "):]), "a/"); p != "" {
				if pre != "" && p != pre {
					return false, "", exitError2{"a section's --- path disagrees with its diff --git header; refusing the patch"}
				}
				if underGithub(p) {
					refused = true
				}
			}
		case bytes.HasPrefix(line, []byte("+++ ")):
			if p := bodyPath(string(line[len("+++ "):]), "b/"); p != "" {
				if post != "" && p != post {
					return false, "", exitError2{"a section's +++ path disagrees with its diff --git header; refusing the patch"}
				}
				if underGithub(p) {
					refused = true
				}
			}
		case bytes.HasPrefix(line, []byte("rename from ")), bytes.HasPrefix(line, []byte("copy from ")):
			verb := "rename from "
			if bytes.HasPrefix(line, []byte("copy from ")) {
				verb = "copy from "
			}
			if p := bodyPath(string(line[len(verb):]), ""); p != "" {
				if pre != "" && p != pre {
					return false, "", exitError2{"a section's rename/copy source path disagrees with its diff --git header; refusing the patch"}
				}
				if underGithub(p) {
					refused = true
				}
			}
			if refused {
				return false, "", exitError2{"a .github/ section renames or copies a file; refusing the patch"}
			}
		case bytes.HasPrefix(line, []byte("rename to ")), bytes.HasPrefix(line, []byte("copy to ")):
			verb := "rename to "
			if bytes.HasPrefix(line, []byte("copy to ")) {
				verb = "copy to "
			}
			if p := bodyPath(string(line[len(verb):]), ""); p != "" {
				if post != "" && p != post {
					return false, "", exitError2{"a section's rename/copy target path disagrees with its diff --git header; refusing the patch"}
				}
				if underGithub(p) {
					refused = true
				}
			}
			if refused {
				return false, "", exitError2{"a .github/ section renames or copies a file; refusing the patch"}
			}
		}
	}
	return refused, "", nil
}

// bodyPath cleans one path out of a `---`/`+++`/`rename from`/`rename to`
// line: git appends a tab and a timestamp to `---`/`+++` paths when asked,
// and that suffix is not part of the path. The rest of the cleaning —
// /dev/null, quoting, the a/ or b/ prefix, the length cap — is patchPath's.
func bodyPath(rest, prefix string) string {
	if i := strings.IndexByte(rest, '\t'); i >= 0 {
		rest = rest[:i]
	}
	return patchPath(rest, prefix)
}

// underGithub reports whether p is under .github/ but not .github/codeaf/:
// the part of the repository only a maintainer may change without approval.
func underGithub(p string) bool {
	if p == "" {
		return false
	}
	if p == ".github/codeaf" || strings.HasPrefix(p, ".github/codeaf/") {
		return false
	}
	return p == ".github" || strings.HasPrefix(p, ".github/")
}

// fenceBackticks opens and closes the refused diff's code fence: five
// backticks survive any 3+ run left in the diff (they are all replaced).
const fenceBackticks = "`````"
