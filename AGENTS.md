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

## Models and concurrency

- The synthetic.new subscription allows only 2 calls to each model at once.
  Calls past that wait in the harness proxy (`max_concurrent` in
  `.github/codeaf/models.json`), so more parallel work on one model only
  queues.
- Spread work that runs at the same time across models. CodeAF's seats are
  on different models (`.github/codeaf/codeaf-config.json`), and
  `task.parallel` there is no higher than the worker model's
  `max_concurrent`. Keep both true when changing models or seats.

## Repository layout

This is a monorepo: several independent applications live side by side, and
more will be added.

- `app/<name>/` holds one application each. Each is its own Go module with its own
  `go.mod` (and its own Go version), README and licence. Treat an app's
  directory as its boundary: a change for one app shouldn't touch another
  app's files unless the task says so.
- `.github/` holds the CodeAF harness and CI. CodeAF may change it when a task asks
  for that.
  - A change only under `.github/codeaf/` (the harness program and its
    config) is published like app code. It takes effect only once merged.
  - Any other change under `.github/` (workflows, `CODEOWNERS`) is published
    only after a maintainer approves the `codeaf-harness` environment
    deployment.

## Security rules for changing the harness

These apply to any change to `.github/` (workflows, `.github/codeaf/`), whoever
makes it. The CodeAF agent runs arbitrary shell commands as a separate,
unprivileged user of its own (see "Privilege separation" below), and its task
text can come from anyone who opens an issue. Assume it may be hostile,
whether through prompt injection or a compromised model.

### Caches and carried state

- Anything written during a job in which the agent ran is untrusted. That
  includes artifacts, files under `$RUNNER_TEMP`, and `$CODEAF_HOME`.
- The `run` job can't write the Actions cache: it has no writable token scope.
  Keep it that way. Only workflows that never run the agent write caches.
- Never carry binaries, toolchains, Go module or build caches, or scripts
  between runs. Build tools from pinned sources each run.
- CodeAF's state is carried between runs in one shared bundle
  (`.github/codeaf/state.go`, `codeaf-state.yml`): the memory db
  (`graph.db`), `router-events.jsonl`, the model ratings and the rolling 7-day
  credit spend (spend is credit at subscription rates on a rolling 7-day week;
  the account is on synthetic.new's Big plan).
  This was a deliberate decision. CodeAF puts saved memories from `graph.db`
  into every worker's prompt, so a poisoned memory would reach later runs.
  Hence the rules below:
  - Only the CodeAF state workflow writes the bundle, one merge at a time.
    Runs only hand over their state as an artifact.
  - Every packed run is checked before it is merged. The ledger is rebuilt
    from typed, bounded fields about allowed models. Events are kept only as
    bounded JSON objects. The db is accepted as an opaque SQLite file by its
    header, and is never opened by the harness. Runs from forks are ignored.
  - `state load` checks the bundle again before CodeAF starts, and fails the
    run if it isn't valid.
  - Overlapping runs don't overwrite each other. Spend is added. Ratings and
    events are merged. The db is taken only from a run that started from the
    current generation.
  - To roll back, delete the newest `codeaf-shared-v1-*` cache entries; the
    last five generations are kept.
- Never copy anything else between runs as-is. Carry only data that can be
  validated, and validate it on every load and merge.
- Validate numbers from the bundle and from run artifacts before using them;
  never paste them into `$GITHUB_OUTPUT` or `$GITHUB_ENV` raw. A spend total in
  the bundle that is negative or not a number fails the run. A run's own spend
  that can't be right counts as the whole per-run credit cap
  (`limits.run_credit_usd`).
- Record spend from the proxy's in-memory total, not from files the agent can
  edit. The harness falls back to the ledger file only if the proxy is gone.
  The proxy runs as the runner user and the agent as the `codeaf` user, so the
  agent can neither kill nor replace the proxy; before the user separation
  (issue #54) it could, which is why the rule stays. Runs that overlap
  each start from the weekly total as it was, so together they can pass the
  weekly credit. So for a hard budget, also set a spending limit on the
  synthetic.new account itself (overage is capped by `limits.overage_usd`,
  default 0).

### Privilege separation

- The agent runs as its own unprivileged user (`codeaf`), created fresh in
  every `run` job: no sudo, no privilege-carrying groups (not `docker`
  either -- its socket is a root equivalent on these runners). The working
  tree and `$CODEAF_HOME` are handed to it (chowned) for the run and taken
  back afterwards, `/home/runner` keeps its 0750 mode plus one traverse bit,
  and CodeAF starts in a session of its own (`setsid`).
- What the agent can reach now: what the `codeaf` user owns -- the working
  tree, `$CODEAF_HOME`, its home -- and what world permissions allow, which
  by design includes the brief, the proxy log and the diagnostics-destined
  files. What it cannot: signal, ptrace or read the runner's processes
  (`Runner.Worker`, the proxy); read or write runner-owned files beyond
  world permissions; use the runner's passwordless sudo; kill, replace or
  read the proxy's memory, its spend total or the cost ledger. The workflow
  asserts this before CodeAF starts (probes as `codeaf`: `kill -0` and
  `/proc/PID/environ` on the runner's worker, `sudo -n true`) and stops the
  run if any probe succeeds.
- What does not change: everything the agent writes is untrusted data, as
  the rest of this section and "Caches and carried state" say; `publish`
  still checks the patch, and the state merge still re-checks the bundle.
  The launch line passes the agent user only `CODEAF_BASE_URL`,
  `CODEAF_HOME` and `PATH` (sudo's env_reset drops the rest), and the
  synthetic.new key still reaches the proxy alone, on stdin.
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
  checks the patch's size and that its base commit matches. For a new
  branch whose base the API confirms the default branch contains, it
  instead merges the patch onto the default branch with `git apply --3way`,
  starting from the base only on a conflict. It applies it with
  `git apply`, which runs nothing and refuses `.git` paths, then refuses the
  result if any staged path (rename detection off) is under `.github/` but
  outside `.github/codeaf/`, unless a maintainer approved it. Keep it that
  way: never run, `source` or `eval` anything that came from `run`.
- `.github/codeaf/` may be published without approval only because every job
  that holds a secret or a write token builds the harness from the default
  branch. On a branch it is only tested by CI, read-only and without secrets.
  Keep both true. Never make a workflow build, run or read config from
  `.github/codeaf/` on a non-default ref in a job with secrets or write
  access.
- A patch that touches `.github/` outside `.github/codeaf/` is a patch that
  can change what CI runs with secrets. `publish` runs it in the
  `codeaf-harness` environment, which must
  require a maintainer's review, and then checks through the API that someone
  with admin or maintain access approved this run, since GitHub silently
  creates a missing environment with no reviewers. Only then is it applied and
  pushed. The `run` job's `touches_harness` output only routes the job; never
  treat its absence as proof that a patch leaves `.github/` alone.
- `codeaf-state.yml` runs no agent and handles no secret. It reads other runs'
  artifacts as untrusted data and holds only `actions: write`, for the
  artifacts and the cache.
- Approved harness patches are pushed as a GitHub App, never as a person.
  This is needed because `GITHUB_TOKEN` can't push workflow changes.
  - The app's `CODEAF_APP_ID` and `CODEAF_APP_PRIVATE_KEY` are secrets of the
    `codeaf-harness` environment only, and are read only after that approval
    check.
  - The key only signs the request for an installation token, which is
    scoped to this repository with `contents` and `workflows` write.
  - Don't widen the app's permissions or its installation, and don't use its
    key anywhere else.

### Artifacts

- On a public repository, any signed-in GitHub user can download workflow
  artifacts, and GitHub does not mask secrets inside them. Every artifact is
  public.
- Diagnostics are uploaded in plaintext, by choice: being able to see why a
  run failed matters more here than hiding its prompts. They hold the brief,
  transcripts, tool output and logs, so nothing secret may ever reach the
  `run` job's files or environment once the proxy has started. The
  token-shaped redaction in "Collect diagnostics" is a backstop, not a
  licence.

### Secrets, tokens and triggers

- Never put a secret in the environment of the step that runs CodeAF, or
  anywhere it can read it. The synthetic.new key goes only to the proxy, on
  stdin. Keep `persist-credentials: false` on checkouts, and push with the
  token only after CodeAF has exited. The launch line gives the agent user
  only the environment listed under "Privilege separation"; sudo's
  env_reset strips everything else, and no secret is ever passed through it.
- Don't add `pull_request_target`, and don't run PR-head code in a job that
  has secrets or write permissions.
- Keep the trust gate: the `codeaf` label (triage access or above) and
  `/codeaf` commands from OWNER/MEMBER/COLLABORATOR only. Pass issue and PR
  text to tools as data (files, environment variables), never into `${{ }}`
  expressions inside `run:` scripts.
- Pin every action to a full commit SHA. CodeAF's changes to `.github/`
  outside `.github/codeaf/` are published only after a maintainer's approval;
  never add a path that publishes them without one.
