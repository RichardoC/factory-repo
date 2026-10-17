# factory-repo

An experiment in running [CodeAF](https://github.com/Agent-Field/CodeAF) entirely inside
GitHub: issues go in, CodeAF does the work in GitHub Actions, and pull requests come out.

It is set up as a monorepo:
- applications live under `app/<name>/`, each its own Go module with its own README
  and licence;
- CI checks every module separately;
- the first application here is [kube-sqlite3-vfs](app/kube-sqlite3-vfs/README.md), a SQLite VFS that stores database files in Kubernetes ConfigMaps;
- the CodeAF harness and CI live in `.github/`.

## Using it

| You do | CodeAF does |
|---|---|
| Add the **`codeaf`** label to an issue | Works the issue on a new branch `codeaf/issue-N-<run id>` cut from the default branch and opens its own PR (labelled `codeaf`), so re-running never collides with an earlier run |
| Comment `/codeaf <instructions>` on a `codeaf`-labelled PR | Continues on the PR's branch with your instructions, the review comments and the conversation |
| Submit a **Request changes** review on a `codeaf`-labelled PR | Same as above, using the review as the instructions |
| Comment `/codeaf <instructions>` on a `codeaf`-labelled issue | Re-runs the issue on a new branch and opens a new PR; earlier runs' branches and PRs are left alone |

Nothing else triggers it. Only people with triage access can add labels, and `/codeaf`
commands are obeyed only from repository owners, members and collaborators, so
outside contributors cannot spend the API budget. PRs from forks are ignored.

Every run posts a report comment with CodeAF's outcome, a per-model cost table and
the run's timing figures (wall time, time with a call in flight, tokens/s, longest call).
Three workflow artifacts are uploaded, none of them encrypted:
- `codeaf-output-*` (14 days): the patch, its base commit, CodeAF's JSON result
  and the call ledger (each call's start and duration, slot wait and reasoning tokens);
- `codeaf-diagnostics-*` (30 days): the brief, CodeAF's stderr, its run records
  (prompts, replies, tool output), call log and usage, and the proxy log;
- `codeaf-state-*` (14 days): what the run left in CodeAF's home, and what it
  cost, for the shared state below.

## How it is wired

- [`.github/workflows/codeaf.yml`](.github/workflows/codeaf.yml) runs three jobs,
  each checked out at the default branch:
  - `gate` checks the event against the rules above (`harness gate`).
  - `run`, which has no write token, builds CodeAF at a pinned commit, writes a
    brief from the issue/PR, runs `codeaf do` headless and hands over a patch.
  - `publish`, on a fresh runner, checks and applies the patch, then commits,
    pushes, opens or updates the PR and dispatches [CI](.github/workflows/ci.yml).
    Pushes made with `GITHUB_TOKEN` do not trigger CI on their own.
- [`.github/codeaf/main.go`](.github/codeaf/main.go) is the harness: a standalone,
  standard-library-only Go program, with tests, that the workflow builds on every run.
  It decides whether an event may run (`gate`), writes the brief (`brief`), and renders
  the cost report (`report`). As `harness proxy` it runs on `127.0.0.1` for the
  length of the job and sits between CodeAF and [synthetic.new](https://synthetic.new).
  It:
  - serves CodeAF an OpenRouter-style `/models` catalog containing only the models in
    [`models.json`](.github/codeaf/models.json), at synthetic.new's usage-based prices,
    so that CodeAF's estimates and model routing see real prices;
  - adds `usage.cost` to every response, which is where CodeAF's spend accounting
    reads cost from;
  - refuses any other model, and returns HTTP 402 once the run reaches **$5** or the
    UTC day reaches **$40**. The day's total comes from the shared state.

  The synthetic.new key (`SYNTHETIC_API_KEY` secret) is held only by the proxy. It is
  passed in on stdin, not through the environment.
- Model choice: the seats are spread across models, because the synthetic.new
  subscription allows only **2 calls to each model at once**:
  - the worker runs on **GLM-5.3-Flash** ($0.15 in / $0.50 out per M tokens);
  - the planner and checker run on **DeepSeek-V4.1-Flash** ($0.60 / $1.20).
  - CodeAF runs at most 2 tasks at once (`task.parallel`), so the workers
    fit in GLM-5.3-Flash's 2 slots.
  - The proxy enforces the limit per model (`max_concurrent` in
    `models.json`): a call past it waits for a slot instead of reaching
    synthetic.new. Each wait is written to the proxy log and the call
    ledger, so the diagnostics show whether a run was held up by it.
  - Escalation is switched off for now. CodeAF's cascade
    ([`panel.json`](.github/codeaf/panel.json)) would escalate to
    **DeepSeek-V4.1-Flash** ($0.60 / $1.20) and then **GLM-5.3** ($1.40 / $4.40),
    but it has two bugs:
    - it treats a reply made only of tool calls as empty, and fails every run;
    - its pinned rungs all call the seat model, so it never escalates.
  - [`codeaf.yml`](.github/workflows/codeaf.yml) explains how to turn it back on
    once CodeAF is fixed.

## Limits worth knowing

- The agent has a shell on the runner, so treat it as having the same access as any
  CI job. It gets no GitHub token: the checkout does not keep credentials, and the
  workflow pushes after CodeAF exits.
- CodeAF may change `.github/` (the harness, CI and workflows) when a task asks
  for it.
  - A change only to `.github/codeaf/` (the harness program, `models.json`,
    `codeaf-config.json`) is published like app code. Every job that holds a
    secret or a write token builds the harness from `main`, so the change
    takes effect only once merged.
  - Any other change under `.github/` (workflows, `CODEOWNERS`) controls what
    CI runs with secrets. For those, `publish` runs in the
    **`codeaf-harness`** environment and waits for a maintainer
  to approve it in the run's page (GitHub emails the reviewers) before anything
  is pushed. It also checks the approval through the API, so a misconfigured
  environment refuses the patch rather than publishing it. One-off setup:
  - *Settings → Environments → New environment* `codeaf-harness`:
    - **Required reviewers**: yourself. Leave *Prevent self-review* off, since
      the run is started by you.
    - **Deployment branches**: `main` only.
  - Create a GitHub App to push approved harness patches, so they don't
    go out as you. `GITHUB_TOKEN` can't push changes to
    `.github/workflows/`, so without the app only changes elsewhere under
    `.github/` can be published.
    1. *Settings → Developer settings → GitHub Apps → New GitHub App*, for
       example "factory-repo CodeAF harness".
       - Untick *Webhook → Active*.
       - Repository permissions: *Contents* read and write, *Workflows* read
         and write. Nothing else.
       - *Only on this account*.
    2. Install it on `RichardoC/factory-repo` only.
    3. Generate a private key, then add two secrets to the `codeaf-harness`
       environment:
       - `CODEAF_APP_ID`: the app's ID;
       - `CODEAF_APP_PRIVATE_KEY`: the whole `.pem` file.
    4. Delete the downloaded key file.

    After approval, `publish` uses the key to mint a token that lasts an hour,
    for this repository only, with just those two permissions. It pushes with
    that token, so the push shows as `<app name>[bot]`.
- CodeAF runs in a job with no write access (`run`). A separate job on a fresh
  runner (`publish`) checks and applies the resulting patch, then commits,
  pushes, opens the PR and posts the report. Nothing CodeAF could have tampered
  with is executed with a write token.
- **Shared state.** Every run starts from one shared bundle:
  - CodeAF's memory db, router events and model ratings;
  - the day's spend.

  [`codeaf-state.yml`](.github/workflows/codeaf-state.yml) is the only writer:
  - It merges each run's `codeaf-state-*` artifact into a new generation of
    the bundle, saved in the Actions cache as `codeaf-shared-v1-g<N>-…`. The
    last five generations are kept.
  - The CodeAF workflow's `handoff` job starts it as soon as CodeAF has
    finished. It runs one merge at a time, and each merge picks up every run
    not merged yet.
  - When runs overlap, spend is added and ratings and events are merged. The
    memory db comes only from a run that started from the latest generation.

  Everything is checked before it is merged, and again before CodeAF sees it
  (see [`AGENTS.md`](AGENTS.md#caches-and-carried-state)). Saved memories reach
  CodeAF's prompts. So to reset the state, delete the `codeaf-shared-v1-*`
  caches (*Actions → Caches*); to roll back, delete only the newest ones.
- CodeAF itself is rebuilt from source on every run; nothing executable is
  carried between runs.
- **Diagnostics are public.** Artifacts on a public repo can be downloaded by
  any signed-in GitHub user, and the diagnostics are not encrypted. They hold
  the brief, prompts, model replies and tool output, so never put anything
  secret in an issue or in the `run` job. Token-shaped strings are redacted
  before upload, as a backstop. The `CODEAF_ARTIFACT_PUBLIC_KEY` and
  `CODEAF_ARTIFACT_PRIVATE_KEY` secrets are no longer used and can be deleted.
- The rules for future changes are in
  [`AGENTS.md`](AGENTS.md#security-rules-for-changing-the-harness).
- Protecting `main`: [`CODEOWNERS`](.github/CODEOWNERS) makes @RichardoC the owner of
  everything. Pair it with a ruleset on `main` that requires a PR, a Code Owner review,
  approval of the most recent push, and the `CI / go` check. A `github-actions[bot]`
  approval then never counts, even with the setting below enabled. The workflow itself
  never approves or merges anything.
- Repository setting needed: *Settings → Actions → General → Allow GitHub Actions to
  create and approve pull requests* (for `gh pr create` with `GITHUB_TOKEN`).
- Configuration lives in files, not workflow variables:
  - [`models.json`](.github/codeaf/models.json): the spend caps (`limits`) and the
    allowed models with their prices (USD per million tokens) and how many calls
    each may have in flight (`max_concurrent`);
  - [`panel.json`](.github/codeaf/panel.json): the escalation order (not used for
    now, see above);
  - [`codeaf-config.json`](.github/codeaf/codeaf-config.json): CodeAF's own settings
    (seat models, how many tasks run at once, budgets, telemetry and Model Pool
    off), installed as its profile config.

  Keep the three consistent when changing models. Put seats that run at the
  same time on different models, and keep `task.parallel` no higher than the
  worker model's `max_concurrent`. The only CodeAF setting left in
  the environment is the endpoint (`CODEAF_BASE_URL`), because CodeAF reads it from
  nowhere else. The panel path (`CODEAF_MODELS`) would be the second one.
