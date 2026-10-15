# AGENTS.md

Instructions for any coding agent working in this repository, including CodeAF
(which loads this file automatically) and Claude Code.

## Repository hygiene

- Don't commit caches, build output or tool scratch files: `__pycache__/`, `*.pyc`,
  compiled binaries, coverage profiles, editor and OS files, and similar. If a tool
  creates one, delete it, or add it to `.gitignore` when it will keep coming back.
- Don't commit secrets, tokens, `.env` files or logs that may contain them.
- Keep diffs focused on the task. Don't reformat or rename code you aren't otherwise
  changing.

## Go

- Run `gofmt -l .`, `go vet ./...`, `go build ./...` and `go test -race ./...` before
  you finish, and leave all four clean. CI runs the same checks.
- Use the Go version in `go.mod`. Run `go mod tidy` after changing dependencies and
  commit `go.sum` along with `go.mod`.
- Tests must not need a live Kubernetes cluster. Use client-go's fake clientset, or
  envtest/kind behind a build tag that CI opts into explicitly.
- Back a performance claim with a benchmark (`go test -bench`), and give before and
  after numbers.

## Repository layout

- `.github/` holds the CodeAF harness and CI. Agents running under the CodeAF workflow
  can't change it: edits there are discarded.

## Security rules for changing the harness

These apply to any change to `.github/` (workflows, `.github/codeaf/`), whoever
makes it. The CodeAF agent runs arbitrary shell commands as the job's user, and
its task text can come from anyone who opens an issue. Assume it may be
hostile, whether through prompt injection or a compromised model.

### Caches and carried state

- Anything written during a job in which the agent ran is untrusted. That
  includes Actions caches, artifacts, files under `$RUNNER_TEMP`, and
  `$CODEAF_HOME`. The agent may also be able to reach the cache service from inside the
  job, so it can overwrite cache entries, not just the files the workflow
  saves.
- Never carry anything between runs that a later run will execute or put in
  front of the model. That rules out caching binaries, toolchains, Go module
  or build caches, scripts, CodeAF's memory database (`graph.db`), event logs,
  transcripts, briefs and notes. Build tools from pinned sources each run.
- Carry only data that can be fully validated, and validate it on every
  restore and save. Rebuild it from typed fields with bounds and an allowlist,
  as `harness ledger` does for the model-rating ledger. Never copy a file
  through as-is.
- Validate numbers from a cache before using them; never paste them into
  `$GITHUB_OUTPUT` or `$GITHUB_ENV` raw. A spend total that is negative or not
  a number fails the run.
- Record spend from the proxy's in-memory total, not from files the agent can
  edit. Cached totals can be reset by an attacker, so for a hard budget also
  set a spending limit on the synthetic.new account itself.
- Runs triggered by issues and issue comments write caches in the default
  branch's scope, which every branch and PR can read. Treat those caches as
  shared.

### Secrets, tokens and triggers

- Never put a secret in the environment of the step that runs CodeAF, or
  anywhere it can read it. The synthetic.new key goes only to the proxy, on
  stdin. Keep `persist-credentials: false` on checkouts, and push with the
  token only after CodeAF has exited.
- Don't add `pull_request_target`, and don't run PR-head code in a job that
  has secrets or write permissions.
- Keep the trust gate: the `codeaf` label (triage access or above) and
  `/codeaf` commands from OWNER/MEMBER/COLLABORATOR only. Pass issue and PR
  text to tools as data (files, environment variables), never into `${{ }}`
  expressions inside `run:` scripts.
- Pin every action to a full commit SHA. Keep `.github/` changes out of
  CodeAF's commits; the workflow discards them.
