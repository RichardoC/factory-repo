# factory-repo

An experiment in running [CodeAF](https://github.com/Agent-Field/CodeAF) entirely inside
GitHub: issues go in, CodeAF does the work in GitHub Actions, and pull requests come out.

## Using it

| You do | CodeAF does |
|---|---|
| Add the **`codeaf`** label to an issue | Works the issue on branch `codeaf/issue-N` and opens a PR (labelled `codeaf`) |
| Comment `/codeaf <instructions>` on a `codeaf`-labelled PR | Continues on the PR's branch with your instructions, the review comments and the conversation |
| Submit a **Request changes** review on a `codeaf`-labelled PR | Same as above, using the review as the instructions |
| Comment `/codeaf <instructions>` on a `codeaf`-labelled issue | Re-runs the issue (on its existing branch if there is one) |

Nothing else triggers it. Only people with triage access can add labels, and `/codeaf`
commands are obeyed only from repository owners, members and collaborators, so
outside contributors cannot spend the API budget. PRs from forks are ignored.

Every run posts a report comment with CodeAF's outcome and a per-model cost table.
The full brief, CodeAF's JSON result, the call ledger and logs are uploaded as a
workflow artifact.

## How it is wired

- [`.github/workflows/codeaf.yml`](.github/workflows/codeaf.yml) checks the event
  against the rules above (`harness gate`), builds CodeAF at a pinned
  commit, writes a brief from the issue/PR, runs `codeaf do` headless, then commits,
  pushes, opens or updates the PR, and dispatches [CI](.github/workflows/ci.yml).
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
    UTC day reaches **$20**. The day's total is carried between runs in the Actions
    cache.

  The synthetic.new key (`SYNTHETIC_API_KEY` secret) is held only by the proxy. It is
  passed in on stdin, not through the environment.
- Model choice: every seat (worker, planner, checker) starts on
  **GLM-5.3-Flash** ($0.15 in / $0.50 out per M tokens). CodeAF's cascade
  ([`panel.json`](.github/codeaf/panel.json)) escalates a call to
  **DeepSeek-V4.1-Flash** ($0.60 / $1.20) and then **GLM-5.3** ($1.40 / $4.40) only
  when a verifier catches a failure.

## Limits worth knowing

- The agent has a shell on the runner, so treat it as having the same access as any
  CI job. It gets no GitHub token: the checkout does not keep credentials, and the
  workflow pushes after CodeAF exits. Edits it makes under `.github/` are discarded.
- CodeAF runs in a job with no write access (`run`). A separate job on a fresh
  runner (`publish`) checks and applies the resulting patch, then commits,
  pushes, opens the PR and posts the report. Nothing CodeAF could have tampered
  with is executed with a write token.
- Caches are treated as untrusted, because the agent can write to them. Only
  CodeAF's model-rating ledger is carried between runs, rebuilt from validated
  numbers each time. CodeAF itself is rebuilt from source on every run, and the
  daily spend total is validated before use.
- Artifacts on a public repo can be downloaded by any signed-in GitHub user. The
  diagnostics (brief, transcripts, logs, and CodeAF's memory db, event log and
  raw ledger, which are kept for audit and never restored) are therefore encrypted
  with [age](https://age-encryption.org) to the `CODEAF_ARTIFACT_PUBLIC_KEY`
  secret and any keys in
  [`artifact-recipients.txt`](.github/codeaf/artifact-recipients.txt). They are
  not uploaded at all until a key is configured. Decrypt with
  `age -d -i private-key.txt diagnostics.tar.gz.age | tar xz`. The private key's
  `CODEAF_ARTIFACT_PRIVATE_KEY` secret is a backup only: GitHub secrets can't be
  read back, and no workflow uses it, so keep your own copy to decrypt.
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
    allowed models with their prices (USD per million tokens);
  - [`panel.json`](.github/codeaf/panel.json): the escalation order;
  - [`codeaf-config.json`](.github/codeaf/codeaf-config.json): CodeAF's own settings
    (seat models, budgets, telemetry and Model Pool off), installed as its profile
    config.

  Keep the three consistent when changing models. The only CodeAF settings left in
  the environment are the endpoint (`CODEAF_BASE_URL`) and the panel path
  (`CODEAF_MODELS`), because CodeAF reads those from nowhere else.
