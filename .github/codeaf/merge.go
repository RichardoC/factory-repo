// merge.go brings a pull request's base branch into its head branch (issue
// #57). A PR that conflicts with its base stays "dirty" forever: the run job
// hands publish a patch against the PR head, publish applies it and pushes a
// plain commit, and nothing ever produces a merge commit, so the branch never
// contains its base (#53 stayed stuck against #52 until a person merged by
// hand in #56).
//
// The flow, in pr mode:
//
//	run      (no write token; nothing it makes is pushed) checks out the PR
//	         head, then merges the base tip in BEFORE CodeAF works: a clean
//	         merge is committed as an ordinary merge commit, a conflicted one
//	         as a merge commit whose files carry conflict markers, and the
//	         brief names those files for CodeAF to resolve. The run's base
//	         commit becomes that merge commit, and a merge-info.json artifact
//	         records what was merged.
//	publish  (fresh runner, harness built from the default branch) recomputes
//	         the merge from the same two parents -- the PR head it fetched and
//	         the base tip named in merge-info.json -- and refuses unless the
//	         tree hash it computed itself matches the recorded one. The run's
//	         merge commit exists only in the run job's checkout, so it cannot
//	         be fetched; like every run artifact it is data, and it is
//	         verified by recomputation (AGENTS.md, "Security rules"). The
//	         patch is then applied -- it removes the markers -- and publish
//	         refuses while any conflict marker remains. When the run left no
//	         merge info (it died before merging), publish merges the base tip
//	         itself first, so a behind-but-clean PR is never left behind.
//
// The invariants hold: `git apply` is still the only thing applied from run;
// the .github/ approval check still reads the staged patch diff; and every
// pushed commit descends from the PR head, so there is no force-push.
//
// The merge itself uses `git merge-tree --write-tree --merge-base` (git 2.40,
// shipped on current runners), which computes the merged tree without touching
// the index or working tree, and `git commit-tree`. No history is walked, so
// the workflow's shallow checkouts work: the merge base comes from the compare
// API, and the three commits involved are fetched one level deep.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The two kinds a merge can have, as recorded in merge-info.json.
const (
	mergeKindClean      = "clean"
	mergeKindConflicted = "conflicted"
)

// mergeInfo is what the run job records about the base-branch merge. Every
// field is bounded and checked before use: it comes from the run job, whose
// outputs are untrusted data (AGENTS.md, "Security rules").
type mergeInfo struct {
	Kind   string   `json:"kind"`   // mergeKindClean or mergeKindConflicted
	Commit string   `json:"commit"` // the merge commit the run made (local to the run job)
	Base   string   `json:"base"`   // the PR head the merge started from
	Onto   string   `json:"onto"`   // the base branch tip merged in
	Tree   string   `json:"tree"`   // the merged tree (carrying markers when conflicted)
	Files  []string `json:"files"`  // the paths the merge conflicted in
}

// mergeOutcome is what a merge phase did, for the workflow steps to read.
// State "none" means no merge was made and the patch alone is pushed.
type mergeOutcome struct {
	State  string   // "clean", "conflicted", "ff" or "none"
	Commit string   // the merge commit made here, when one was
	Tree   string   // its tree
	Files  []string // the paths a conflicted merge conflicts in
}

// mergeCommitIdent names the author of the merge commits, matching the
// identity publish configures for its own commits.
var mergeCommitIdent = []string{
	"GIT_AUTHOR_NAME=codeaf[bot]",
	"GIT_AUTHOR_EMAIL=41898282+github-actions[bot]@users.noreply.github.com",
	"GIT_COMMITTER_NAME=codeaf[bot]",
	"GIT_COMMITTER_EMAIL=41898282+github-actions[bot]@users.noreply.github.com",
}

// isSHA reports whether s is a full lowercase hex commit id.
func isSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// gitOutput runs git in dir and returns its stdout. A nonzero exit is an
// error carrying the first of stderr, with the exit status preserved for
// callers that expect one (git merge-tree exits 1 on a conflict).
func gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	if err != nil {
		stderr := strings.TrimSpace(errBuf.String())
		if len(stderr) > 300 {
			stderr = stderr[:300]
		}
		return out.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr)
	}
	return out.String(), nil
}

// gitOutputAs runs git with the merge committer's identity, for commands that
// author commits.
func gitOutputAs(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), mergeCommitIdent...)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	if err != nil {
		stderr := strings.TrimSpace(errBuf.String())
		if len(stderr) > 300 {
			stderr = stderr[:300]
		}
		return out.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr)
	}
	return out.String(), nil
}

// gitRun runs git in dir, discarding stdout.
func gitRun(dir string, args ...string) error {
	_, err := gitOutput(dir, args...)
	return err
}

// revParse resolves one ref to a full commit id.
func revParse(dir, ref string) (string, error) {
	out, err := gitOutput(dir, "rev-parse", ref)
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(out)
	if !isSHA(sha) {
		return "", fmt.Errorf("git rev-parse %s printed %q, not a commit id", ref, sha)
	}
	return sha, nil
}

// fetchRef fetches one branch tip from origin, one level deep.
func fetchRef(dir, ref string) error {
	_, err := gitOutput(dir, "fetch", "-q", "--depth=1", "origin", "refs/heads/"+ref)
	return err
}

// haveCommit reports whether the commit object is already in the repository.
func haveCommit(dir, sha string) bool {
	_, err := gitOutput(dir, "cat-file", "-e", sha+"^{commit}")
	return err == nil
}

// fetchCommit fetches one commit by id, one level deep (its tree and blobs
// come with it). Both ids merge-default fetches this way -- a merge base and
// the base tip a recorded merge named -- are reachable from a branch, which
// is what GitHub allows fetching by id.
func fetchCommit(dir, sha string) error {
	if haveCommit(dir, sha) {
		return nil
	}
	_, err := gitOutput(dir, "fetch", "-q", "--depth=1", "origin", sha)
	return err
}

// compareInfo is what the compare API says about two commits: their status
// (ahead, behind, diverged or identical, describing head against base) and
// their merge base.
type compareInfo struct {
	Status    string
	MergeBase string // empty when the API named none
}

// compareFunc asks the repository host about two commits. The production
// implementation is ghCompare; tests stub it, the way Decide takes lookups.
type compareFunc func(base, head string) (compareInfo, error)

// ghCompare reads the compare API for two commit ids.
func ghCompare(base, head string) (compareInfo, error) {
	if !isSHA(base) || !isSHA(head) {
		return compareInfo{}, errors.New("the compare API needs two 40-hex commit ids")
	}
	var out struct {
		Status          string `json:"status"`
		MergeBaseCommit *struct {
			Sha string `json:"sha"`
		} `json:"merge_base_commit"`
	}
	if err := newGitHub().get(fmt.Sprintf("/compare/%s...%s", base, head), &out); err != nil {
		return compareInfo{}, err
	}
	info := compareInfo{Status: out.Status}
	if out.MergeBaseCommit != nil {
		info.MergeBase = out.MergeBaseCommit.Sha
	}
	return info, nil
}

// mergeTree computes the merge of ours and theirs over the explicit merge
// base, writing the merged tree into the object database without touching the
// index or the working tree. git exits 1 when the merge conflicts; the merged
// files then carry conflict markers, and the conflicted paths are listed
// after the tree id, up to the blank line before the informational messages.
func mergeTree(dir, base, ours, theirs string) (tree string, files []string, conflicted bool, err error) {
	out, err := gitOutput(dir, "merge-tree", "--write-tree", "--name-only", "--merge-base="+base, ours, theirs)
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if code := exitErr.ExitCode(); code != 1 {
			return "", nil, false, err
		}
		conflicted = true
	} else if err != nil {
		return "", nil, false, err
	}
	lines := strings.Split(out, "\n")
	if len(lines) == 0 || !isSHA(strings.TrimSpace(lines[0])) {
		return "", nil, false, fmt.Errorf("git merge-tree printed no tree id: %.200s", out)
	}
	tree = strings.TrimSpace(lines[0])
	if conflicted {
		for _, line := range lines[1:] {
			if line == "" {
				break // a blank line ends the conflicted paths; messages follow
			}
			files = append(files, line)
		}
	}
	return tree, files, conflicted, nil
}

// commitMerge makes the merge commit for a computed tree with the two parents.
func commitMerge(dir, tree, ours, theirs, message string) (string, error) {
	out, err := gitOutputAs(dir, "commit-tree", tree, "-p", ours, "-p", theirs, "-m", message)
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(out)
	if !isSHA(sha) {
		return "", fmt.Errorf("git commit-tree printed %q, not a commit id", sha)
	}
	return sha, nil
}

// usableMergePath bounds and checks one conflicted path. The paths end up as
// file reads in publish and as brief text, never inside a shell command.
func usableMergePath(path string) bool {
	if path == "" || len(path) > 512 || strings.ContainsAny(path, "\n\x00") {
		return false
	}
	if strings.HasPrefix(path, "/") || strings.HasPrefix(path, "-") {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		if part == ".." {
			return false
		}
	}
	return true
}

// sanitizeMergeFiles bounds the conflicted path list itself.
func sanitizeMergeFiles(files []string) ([]string, error) {
	const maxFiles = 200
	if len(files) > maxFiles {
		return nil, fmt.Errorf("merge info names %d conflicted paths, over the %d allowed", len(files), maxFiles)
	}
	out := make([]string, 0, len(files))
	seen := make(map[string]bool, len(files))
	for _, f := range files {
		if !usableMergePath(f) {
			return nil, fmt.Errorf("merge info names an unusable conflicted path %q", f)
		}
		if seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out, nil
}

// readMergeInfo loads and validates the run job's merge-info.json. Every
// field is bounded: this file is run-job output, and untrusted (AGENTS.md).
func readMergeInfo(path string) (*mergeInfo, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) > 1<<16 {
		return nil, errors.New("merge-info.json is over 64 KB")
	}
	var info mergeInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("merge-info.json: %w", err)
	}
	switch info.Kind {
	case mergeKindClean, mergeKindConflicted:
	default:
		return nil, fmt.Errorf("merge-info.json has unknown kind %q", info.Kind)
	}
	for name, sha := range map[string]string{
		"commit": info.Commit, "base": info.Base, "onto": info.Onto, "tree": info.Tree,
	} {
		if !isSHA(sha) {
			return nil, fmt.Errorf("merge-info.json %s is %q, not a 40-hex commit id", name, sha)
		}
	}
	if info.Kind == mergeKindConflicted {
		files, err := sanitizeMergeFiles(info.Files)
		if err != nil {
			return nil, err
		}
		info.Files = files
	} else {
		info.Files = nil
	}
	return &info, nil
}

// workflowChanges lists the files under .github/workflows/ that differ
// between from and to. Bringing such changes into a PR branch makes the push
// a workflow change, which GitHub refuses from GITHUB_TOKEN ("refusing to
// allow a GitHub App to create or update workflow"), so a merge that would
// carry any is skipped and the patch is pushed alone, as before #57.
func workflowChanges(dir, from, to string) ([]string, error) {
	out, err := gitOutput(dir, "diff", "--no-renames", "--name-only", from, to, "--", ".github/workflows/")
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

// baseBranchState decides from the compare status whether a merge is needed
// at all: "behind" and "identical" mean the branch already contains the base
// tip, and "ahead" means the branch is strictly behind it, which publish
// fast-forwards. Everything else is handled by the callers.
func mergeNeeded(status string) bool {
	return status == "diverged"
}

// runMerge is the run-job half of issue #57: it merges the base branch tip
// into the PR head branch in the run job's checkout, before CodeAF works. A
// clean merge is committed as an ordinary merge commit; a conflicted one as a
// merge commit whose files carry conflict markers, which the brief then names
// for CodeAF to resolve. The base-commit file is rewritten to the merge
// commit -- that is the commit the patch is diffed against -- and
// merge-info.json records the merge for publish to recreate and verify.
// Nothing here is ever pushed: the run job holds no write token.
//
// Every condition it cannot establish (a fetch or API failure, an unusable
// merge base) degrades to "no merge": the run proceeds on the bare PR head
// and publish falls back to merging the base tip itself. Only a failure
// recording a merge that was already made is an error, because the checkout
// and the artifacts would then disagree.
func runMerge(dir, branch, baseRef, baseCommitFile, mergeInfoFile string, compare compareFunc) (mergeOutcome, error) {
	out := mergeOutcome{State: "none"}
	if branch == "" || baseRef == "" {
		log.Printf("merge-default: no branch or base branch named; nothing to merge")
		return out, nil
	}
	head, err := revParse(dir, "HEAD")
	if err != nil {
		return out, err
	}
	// Only the branch the gate named is merged into: the checkout step put
	// this checkout on it, and anything else is a state worth stopping for.
	ref, err := gitOutput(dir, "symbolic-ref", "--short", "HEAD")
	if err != nil || strings.TrimSpace(ref) != branch {
		log.Printf("merge-default: HEAD is on %s, not %s; nothing to merge", strings.TrimSpace(ref), branch)
		return out, nil
	}
	if err := fetchRef(dir, baseRef); err != nil {
		log.Printf("merge-default: could not fetch %s (%v); continuing without bringing it in", baseRef, err)
		return out, nil
	}
	onto, err := revParse(dir, "FETCH_HEAD")
	if err != nil {
		return out, err
	}
	if head == onto {
		return out, nil // already the base tip: nothing to bring in
	}
	cmp, err := compare(head, onto)
	if err != nil {
		log.Printf("merge-default: could not compare the branch with %s (%v); continuing without a merge", baseRef, err)
		return out, nil
	}
	switch {
	case !mergeNeeded(cmp.Status):
		if cmp.Status != "ahead" && cmp.Status != "behind" && cmp.Status != "identical" {
			log.Printf("merge-default: unexpected compare status %q; continuing without a merge", cmp.Status)
		}
		// "ahead": the branch is strictly behind its base; publish
		// fast-forwards it. "behind"/"identical": the base is already in.
		return out, nil
	case cmp.MergeBase == "" || !isSHA(cmp.MergeBase):
		log.Printf("merge-default: the compare API named no merge base; continuing without a merge")
		return out, nil
	}
	base := cmp.MergeBase
	if err := fetchCommit(dir, base); err != nil {
		log.Printf("merge-default: could not fetch the merge base %s (%v); continuing without a merge", shortSHA(base), err)
		return out, nil
	}
	if wf, err := workflowChanges(dir, base, onto); err != nil || len(wf) > 0 {
		log.Printf("merge-default: %s changed workflows since the branch point (%v); the push token cannot carry them, so continuing without a merge", baseRef, wf)
		return out, nil
	}
	tree, files, conflicted, err := mergeTree(dir, base, head, onto)
	if err != nil {
		log.Printf("merge-default: could not compute the merge (%v); continuing without one", err)
		return out, nil
	}
	kind := mergeKindClean
	message := fmt.Sprintf("Merge %s into %s", baseRef, branch)
	if conflicted {
		kind = mergeKindConflicted
		message = fmt.Sprintf("Merge %s into %s (conflicts, to be resolved by CodeAF)", baseRef, branch)
	}
	commit, err := commitMerge(dir, tree, head, onto, message)
	if err != nil {
		log.Printf("merge-default: could not commit the merge (%v); continuing without one", err)
		return out, nil
	}
	if err := gitRun(dir, "reset", "--hard", commit); err != nil {
		log.Printf("merge-default: could not check the merge out (%v); continuing without one", err)
		return out, nil
	}
	out = mergeOutcome{State: kind, Commit: commit, Tree: tree, Files: files}
	// The patch is diffed against this commit, and publish recreates the
	// merge it names: both artifacts must carry it.
	if err := os.WriteFile(baseCommitFile, []byte(commit+"\n"), 0o644); err != nil {
		return out, fmt.Errorf("wrote the merge commit but could not record it as the base commit: %w", err)
	}
	info := mergeInfo{Kind: kind, Commit: commit, Base: head, Onto: onto, Tree: tree, Files: files}
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return out, err
	}
	if err := os.WriteFile(mergeInfoFile, append(data, '\n'), 0o644); err != nil {
		return out, fmt.Errorf("wrote the merge commit but could not record the merge info: %w", err)
	}
	if conflicted {
		list := strings.Join(files, ", ")
		if len(list) > 400 {
			list = list[:400] + "…"
		}
		fmt.Printf("::notice::merged %s (%s) into %s with conflicts; CodeAF must resolve: %s\n", baseRef, shortSHA(onto), branch, list)
	} else {
		fmt.Printf("::notice::merged %s (%s) into %s cleanly\n", baseRef, shortSHA(onto), branch)
	}
	return out, nil
}

// publishMerge is the publish half of issue #57. With the run's merge info it
// recreates the merge from the same two parents, refusing unless the tree it
// computes itself matches the recorded one. Without it -- a run that merged
// nothing, or died before merging -- it merges the base tip first, so a
// behind-but-clean PR is never left behind, and keeps the stale-patch refusal
// for a branch that moved. It writes the merge state for the workflow steps
// and the marker check; the caller pushes only what this produced.
func publishMerge(dir, branch, baseRef, baseCommitFile, mergeInfoFile, stateFile string, compare compareFunc) (out mergeOutcome, err error) {
	// On success the state file records what happened, for the workflow
	// steps (the push condition, the report) and the marker check.
	defer func() {
		if err == nil {
			err = writeMergeState(stateFile, out)
		}
	}()
	head, err := revParse(dir, "HEAD")
	if err != nil {
		return mergeOutcome{State: "none"}, err
	}
	baseCommitBytes, err := os.ReadFile(baseCommitFile)
	if err != nil {
		return mergeOutcome{State: "none"}, fmt.Errorf("the run recorded no base commit: %w", err)
	}
	baseCommit := strings.TrimSpace(string(baseCommitBytes))
	info, err := readMergeInfo(mergeInfoFile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		// A merge info that exists but does not validate is refused, not
		// ignored: the run's checkout was built on it.
		return mergeOutcome{State: "none"}, err
	}
	if info != nil {
		return recreateMerge(dir, branch, baseRef, info, baseCommit, head, compare)
	}
	// No merge info: the run merged nothing. The patch must be against the
	// branch head, as it always was.
	if baseCommit != head {
		return mergeOutcome{State: "none"}, fmt.Errorf("the branch moved while CodeAF ran (base commit %.12s, branch head %.12s); refusing to apply a stale patch", baseCommit, head)
	}
	out = mergeOutcome{State: "none"}
	if baseRef == "" {
		log.Printf("merge-default: no base branch named; pushing the patch alone")
		return out, nil
	}
	if err := fetchRef(dir, baseRef); err != nil {
		log.Printf("merge-default: could not fetch %s (%v); pushing the patch without bringing it in", baseRef, err)
		return out, nil
	}
	onto, err := revParse(dir, "FETCH_HEAD")
	if err != nil {
		return out, err
	}
	if onto == head {
		return out, nil // already the base tip
	}
	cmp, err := compare(head, onto)
	if err != nil {
		log.Printf("merge-default: could not compare the branch with %s (%v); pushing the patch without a merge", baseRef, err)
		return out, nil
	}
	switch {
	case !mergeNeeded(cmp.Status):
		if cmp.Status == "ahead" {
			// The branch is strictly behind its base: a fast-forward brings
			// the base in with no merge commit, and the patch applies on top.
			if wf, err := workflowChanges(dir, head, onto); err != nil || len(wf) > 0 {
				log.Printf("merge-default: %s changed workflows (%v); the push token cannot carry them, so pushing the patch without a fast-forward", baseRef, wf)
				return out, nil
			}
			if err := gitRun(dir, "reset", "--hard", onto); err != nil {
				return out, err
			}
			out.State = "ff"
			fmt.Printf("::notice::fast-forwarded %s to %s (%s)\n", branch, shortSHA(onto), baseRef)
			return out, nil
		}
		return out, nil
	case cmp.MergeBase == "" || !isSHA(cmp.MergeBase):
		log.Printf("merge-default: the compare API named no merge base; pushing the patch without a merge")
		return out, nil
	}
	base := cmp.MergeBase
	if err := fetchCommit(dir, base); err != nil {
		log.Printf("merge-default: could not fetch the merge base %s (%v); pushing the patch without a merge", shortSHA(base), err)
		return out, nil
	}
	if wf, err := workflowChanges(dir, base, onto); err != nil || len(wf) > 0 {
		log.Printf("merge-default: %s changed workflows since the branch point (%v); the push token cannot carry them, so pushing the patch without the merge", baseRef, wf)
		return out, nil
	}
	tree, _, conflicted, err := mergeTree(dir, base, head, onto)
	if err != nil {
		log.Printf("merge-default: could not compute the merge (%v); pushing the patch without one", err)
		return out, nil
	}
	if conflicted {
		// A conflict CodeAF never saw. Publishing a merge whose files carry
		// markers would push an unresolved merge, so the patch goes alone:
		// the PR stays dirty, and the next run does the conflicted flow.
		log.Printf("merge-default: merging %s would conflict; pushing the patch without the merge", baseRef)
		return out, nil
	}
	commit, err := commitMerge(dir, tree, head, onto, fmt.Sprintf("Merge %s into %s", baseRef, branch))
	if err != nil {
		return out, err
	}
	if err := gitRun(dir, "reset", "--hard", commit); err != nil {
		return out, err
	}
	out.State = mergeKindClean
	out.Commit = commit
	out.Tree = tree
	fmt.Printf("::notice::merged %s (%s) into %s cleanly before the patch\n", baseRef, shortSHA(onto), branch)
	return out, nil
}

// recreateMerge rebuilds the merge the run job committed, from the same two
// parents, and refuses unless everything checks out: the recorded base is the
// branch head publish fetched, the recorded commit is the run's base commit,
// the base tip it merged is a commit the base branch contains (anything else
// could smuggle content into the merge under the cover of "bringing the base
// in"), and the tree of the merge publish computes itself matches the
// recorded tree.
func recreateMerge(dir, branch, baseRef string, info *mergeInfo, baseCommit, head string, compare compareFunc) (mergeOutcome, error) {
	out := mergeOutcome{State: "none"}
	if info.Base != head {
		return out, fmt.Errorf("the merge info names base %s but the branch head is %s; refusing a merge made against another commit", shortSHA(info.Base), shortSHA(head))
	}
	if baseCommit != info.Commit {
		return out, fmt.Errorf("the run's base commit %s is not the merge commit its merge info names (%s); refusing", shortSHA(baseCommit), shortSHA(info.Commit))
	}
	if baseRef == "" {
		return out, errors.New("the merge info cannot be checked without the base branch name; refusing")
	}
	if err := fetchRef(dir, baseRef); err != nil {
		return out, fmt.Errorf("could not fetch %s to check the recorded merge: %w", baseRef, err)
	}
	tip, err := revParse(dir, "FETCH_HEAD")
	if err != nil {
		return out, err
	}
	if err := fetchCommit(dir, info.Onto); err != nil {
		return out, fmt.Errorf("could not fetch the recorded merge's base tip %s: %w", shortSHA(info.Onto), err)
	}
	rel, err := compare(info.Onto, tip)
	if err != nil {
		return out, fmt.Errorf("could not compare the recorded merge's base tip with %s: %w", baseRef, err)
	}
	if rel.Status != "behind" && rel.Status != "identical" {
		return out, fmt.Errorf("the recorded merge's base tip %s is not contained in %s (compare says %q); refusing the merge", shortSHA(info.Onto), baseRef, rel.Status)
	}
	// The merge base is the one the run used: the base of the two parents of
	// the merge, which the API reports for the same fixed commits. It is not
	// the merge base of the containment comparison above, which is between
	// the recorded tip and the base branch. Fixed inputs give a fixed merge
	// base, so this recomputes what the run merged over; a disagreement ends
	// in the tree check below.
	rel, err = compare(head, info.Onto)
	if err != nil {
		return out, fmt.Errorf("could not compare the branch with the recorded base tip: %w", err)
	}
	if rel.Status != "diverged" {
		return out, fmt.Errorf("the parents of the recorded merge are not diverged (compare says %q); refusing the merge", rel.Status)
	}
	if rel.MergeBase == "" || !isSHA(rel.MergeBase) {
		return out, errors.New("the compare API named no merge base for the recorded merge; refusing")
	}
	base := rel.MergeBase
	if err := fetchCommit(dir, base); err != nil {
		return out, fmt.Errorf("could not fetch the merge base %s: %w", shortSHA(base), err)
	}
	tree, files, conflicted, err := mergeTree(dir, base, head, info.Onto)
	if err != nil {
		return out, fmt.Errorf("could not recompute the recorded merge: %w", err)
	}
	kind := mergeKindClean
	if conflicted {
		kind = mergeKindConflicted
	}
	if kind != info.Kind {
		return out, fmt.Errorf("the recomputed merge is %s but the run recorded %s; refusing", kind, info.Kind)
	}
	if tree != info.Tree {
		return out, fmt.Errorf("the recomputed merge tree %s does not match the tree the run recorded (%s); refusing the merge", shortSHA(tree), shortSHA(info.Tree))
	}
	commit, err := commitMerge(dir, tree, head, info.Onto, fmt.Sprintf("Merge %s into %s", baseRef, branch))
	if err != nil {
		return out, err
	}
	if err := gitRun(dir, "reset", "--hard", commit); err != nil {
		return out, err
	}
	out = mergeOutcome{State: kind, Commit: commit, Tree: tree, Files: files}
	fmt.Printf("::notice::recreated the run's merge of %s (%s) into %s as %s; tree %s verified\n", baseRef, shortSHA(info.Onto), branch, shortSHA(commit), shortSHA(tree))
	return out, nil
}

// writeMergeState records what publishMerge did, for the workflow steps (the
// push condition and the report) and for the marker check. Only fixed words
// and numbers go in, so the file can be read with grep like pr-mode's output.
func writeMergeState(path string, out mergeOutcome) error {
	merged := out.State != "none"
	if err := os.WriteFile(path, []byte(fmt.Sprintf("state=%s\nmerged=%t\n", out.State, merged)), 0o644); err != nil {
		return err
	}
	if out.State == mergeKindConflicted {
		return os.WriteFile(path+".files", []byte(strings.Join(out.Files, "\n")+"\n"), 0o644)
	}
	return nil
}

// findConflictMarker reports the first line that opens or closes a conflict
// region. The bare "=======" separator is deliberately not a marker on its
// own: it is legal text, and an unresolved region always carries its two
// outer markers.
func findConflictMarker(content []byte) (line int, ok bool) {
	for i, text := range strings.Split(string(content), "\n") {
		if strings.HasPrefix(text, "<<<<<<<") || strings.HasPrefix(text, ">>>>>>>") {
			return i + 1, true
		}
	}
	return 0, false
}

// verifyMerge refuses the publish while a conflict marker remains in any file
// the recreated merge conflicted in (issue #57): the patch was supposed to
// resolve them, whatever else it did. A conflicted file the patch deleted
// counts as resolved.
func verifyMerge(dir, stateFile string) error {
	data, err := os.ReadFile(stateFile)
	if err != nil {
		return fmt.Errorf("could not read the merge state: %w", err)
	}
	state := ""
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "state="); ok {
			state = strings.TrimSpace(v)
		}
	}
	if state != mergeKindConflicted {
		return nil // nothing was conflicted; nothing to resolve
	}
	listData, err := os.ReadFile(stateFile + ".files")
	if err != nil {
		return fmt.Errorf("the merge was conflicted but its file list is missing: %w", err)
	}
	var marked []string
	for _, f := range strings.Split(strings.TrimRight(string(listData), "\n"), "\n") {
		if f == "" {
			continue
		}
		if !usableMergePath(f) {
			return fmt.Errorf("the merge state names an unusable conflicted path %q", f)
		}
		content, err := os.ReadFile(filepath.Join(dir, f))
		if errors.Is(err, os.ErrNotExist) {
			continue // resolved by deleting the file
		}
		if err != nil {
			return fmt.Errorf("could not read the conflicted file %s: %w", f, err)
		}
		if line, ok := findConflictMarker(content); ok {
			where := f
			if line > 0 {
				where = fmt.Sprintf("%s line %d", f, line)
			}
			marked = append(marked, where)
		}
	}
	if len(marked) > 0 {
		for _, m := range marked {
			fmt.Printf("::error::conflict marker left in %s\n", m)
		}
		return fmt.Errorf("%d conflicted file(s) still carry conflict markers; every conflict must be resolved before publishing", len(marked))
	}
	return nil
}

// runMergeDefault implements `harness merge-default` (issue #57):
//
//	harness merge-default run BASE_COMMIT_FILE MERGE_INFO_FILE
//	    run job, pr mode: merge the base branch tip into the branch before
//	    CodeAF works, commit it (with markers on a conflict), make it the base
//	    commit, and record it in merge-info.json for publish to recreate.
//	harness merge-default publish BASE_COMMIT_FILE MERGE_INFO_FILE STATE_FILE
//	    publish, pr mode: recreate the run's merge from the same two parents,
//	    checked by tree hash, or merge the base tip first when the run left no
//	    merge info; write the merge state for the workflow and the marker check.
//	harness merge-default verify STATE_FILE
//	    publish, after the patch: refuse while a conflict marker remains in a
//	    conflicted file.
//
// Every phase is a no-op outside pr mode, so review and issue runs are
// untouched; the workflow steps gate on the mode as well.
func runMergeDefault() error {
	if len(os.Args) < 3 {
		return errors.New("usage: harness merge-default run|publish|verify ...")
	}
	if envOr("MODE", "") != "pr" {
		return nil
	}
	phase, args := os.Args[2], os.Args[3:]
	branch, baseRef := os.Getenv("BRANCH"), os.Getenv("BASE")
	switch phase {
	case "run":
		if len(args) != 2 {
			return errors.New("usage: harness merge-default run BASE_COMMIT_FILE MERGE_INFO_FILE")
		}
		_, err := runMerge(".", branch, baseRef, args[0], args[1], ghCompare)
		return err
	case "publish":
		if len(args) != 3 {
			return errors.New("usage: harness merge-default publish BASE_COMMIT_FILE MERGE_INFO_FILE STATE_FILE")
		}
		_, err := publishMerge(".", branch, baseRef, args[0], args[1], args[2], ghCompare)
		return err
	case "verify":
		if len(args) != 1 {
			return errors.New("usage: harness merge-default verify STATE_FILE")
		}
		return verifyMerge(".", args[0])
	}
	return fmt.Errorf("unknown merge-default command %q", phase)
}

// mergeBriefSection renders the brief's paragraph about the base-branch merge
// (issue #57). A missing file is the normal case: the run merged nothing, and
// the brief says nothing. A conflicted merge that cannot be read is an error,
// because CodeAF would otherwise work on files carrying markers it was never
// told about.
func mergeBriefSection(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	info, err := readMergeInfo(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## The base branch was merged into this branch")
	if info.Kind == mergeKindConflicted {
		fmt.Fprintf(&b, " — resolve the conflicts\n\n")
		fmt.Fprintf(&b, "HEAD is now a merge commit that brings the base branch tip %s into this branch. "+
			"The merge conflicts in these files, which currently carry conflict markers:\n\n", shortSHA(info.Onto))
		for _, f := range info.Files {
			fmt.Fprintf(&b, "- `%s`\n", f)
		}
		b.WriteString("\nResolve every conflict in place: keep the intended code and delete every conflict marker line " +
			"(the lines starting with `<<<<<<<`, `=======` and `>>>>>>>`). The publish step refuses to push anything " +
			"while a conflict marker remains in any of these files. Your diff is measured against that merge commit, " +
			"so a file you leave untouched is not part of your patch — the markers must be resolved in the files themselves.\n")
	} else {
		fmt.Fprintf(&b, "\n\n")
		fmt.Fprintf(&b, "HEAD is now a merge commit that brings the base branch tip %s into this branch; the merge was clean, "+
			"so nothing conflicts. Your diff is measured against that merge commit: files the merge changed are part of the "+
			"branch now, not of your patch, unless you edit them.\n", shortSHA(info.Onto))
	}
	return strings.TrimRight(b.String(), "\n"), nil
}
