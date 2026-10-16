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

- Applications live under `app/<name>/`, each its own Go module. Run
  `gofmt -l .`, `go vet ./...`, `go build ./...` and `go test -race ./...` from
  inside each module you change, and leave all four clean. CI runs the same
  checks for every module, using the Go version in that module's `go.mod`.
- Use the Go version in `go.mod`. Run `go mod tidy` after changing dependencies and
  commit `go.sum` along with `go.mod`.
- Tests must not need a live Kubernetes cluster. Use client-go's fake clientset, or
  envtest/kind behind a build tag that CI opts into explicitly.
- Back a performance claim with a benchmark (`go test -bench`), and give before and
  after numbers.

## Repository layout

This is a monorepo: several independent applications live side by side, and
more will be added.

- `app/<name>/` holds one application each. Each is its own Go module with its own
  `go.mod` (and its own Go version), README and licence. Treat an app's
  directory as its boundary: a change for one app shouldn't touch another
  app's files unless the task says so.
- `.github/` holds the CodeAF harness and CI. CodeAF may change it when a task asks
  for that, but such a patch is only published after a maintainer approves the
  `codeaf-harness` environment deployment, and the harness changes take effect
  only once merged.

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
- Untrusted state may be *stored* for audit or later review, as long as no
  run restores it automatically. For example, the encrypted diagnostics
  bundle holds CodeAF's `graph.db`, event log and raw ledger for 90 days. To
  start using such state, a person reviews a snapshot first and then adds the
  restore step deliberately, with validation, in a reviewed PR.
- Carry only data that can be fully validated, and validate it on every
  restore and save. Rebuild it from typed fields with bounds and an allowlist,
  as `harness ledger` does for the model-rating ledger. Never copy a file
  through as-is.
- Validate numbers from a cache before using them; never paste them into
  `$GITHUB_OUTPUT` or `$GITHUB_ENV` raw. A spend total that is negative or not
  a number fails the run.
- Record spend from the proxy's in-memory total, not from files the agent can
  edit. The harness falls back to the ledger file only if the proxy is gone,
  and the agent (same user) could even replace the proxy. Cached totals can
  also be reset by an attacker. So for a hard budget, also set a spending limit
  on the synthetic.new account itself.
- Runs triggered by issues and issue comments write caches in the default
  branch's scope, which every branch and PR can read. Treat those caches as
  shared.

### Privilege separation

- The job that runs CodeAF (`run`) gets no write token and no secret other
  than the proxy's key on stdin. Anything that needs write access (commit,
  push, PR, comments, CI dispatch) happens in `publish`, on a fresh runner.
- After CodeAF has run, nothing on that runner can be trusted: binaries,
  `.git` (hooks, config), `$GITHUB_ENV`, `$GITHUB_OUTPUT`, files. Never give a
  later step in that job a secret or a write token.
- Every job checks out the default branch explicitly
  (`ref: ${{ github.event.repository.default_branch }}`) and builds the harness
  from it. On `pull_request_review` events the default ref is the PR's merge
  commit, which is PR code.
- `publish` treats the run job's outputs and artifacts as untrusted data. It
  checks the patch's size and that its base commit matches, applies it with
  `git apply`, which runs nothing and refuses `.git` paths, then refuses the
  result if any staged path is under `.github/` unless a maintainer approved
  it. Keep it that way: never run, `source` or `eval` anything that came from
  `run`.
- A patch that touches `.github/` is a patch that can change what CI runs with
  secrets. `publish` runs it in the `codeaf-harness` environment, which must
  require a maintainer's review, and then checks through the API that someone
  with admin or maintain access approved this run, since GitHub silently
  creates a missing environment with no reviewers. Only then is it applied and
  pushed. The `run` job's `touches_harness` output only routes the job; never
  treat its absence as proof that a patch leaves `.github/` alone.
- `CODEAF_HARNESS_TOKEN` (needed because `GITHUB_TOKEN` can't push workflow
  changes) lives only as a secret of the `codeaf-harness` environment, and is
  read only after that approval check.

### Artifacts

- On a public repository, any signed-in GitHub user can download workflow
  artifacts, and GitHub does not mask secrets inside them. Treat every
  artifact as public.
- Anything that might hold transcripts, prompts, logs or CodeAF state must be
  encrypted before upload, with `gpg --recipient-file`, to the OpenPGP public
  key in the `CODEAF_ARTIFACT_PUBLIC_KEY` secret. That secret is the one
  exception to "no secrets after CodeAF runs", because it is a public key.
  The private key stays with the maintainer. If it is also kept as a
  `CODEAF_ARTIFACT_PRIVATE_KEY` secret for backup, no workflow may ever read
  it, since any job that decrypts would have nowhere private to put the
  result. If no key is configured, don't upload it; never fall back to
  plaintext. Only the patch, its base commit, CodeAF's result and the cost
  ledger, which are published on the PR anyway, go up unencrypted.

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
- Pin every action to a full commit SHA. CodeAF's `.github/` changes are
  published only after a maintainer's approval; never add a path that
  publishes them without one.
