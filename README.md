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
  against the rules above (via `.github/codeaf/ghctx.py`), builds CodeAF at a pinned
  commit, writes a brief from the issue/PR, runs `codeaf do` headless, then commits,
  pushes, opens or updates the PR, and dispatches [CI](.github/workflows/ci.yml).
  Pushes made with `GITHUB_TOKEN` do not trigger CI on their own.
- [`.github/codeaf/proxy.py`](.github/codeaf/proxy.py) runs on `127.0.0.1` for the
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
- Repository setting needed: *Settings → Actions → General → Allow GitHub Actions to
  create and approve pull requests* (for `gh pr create` with `GITHUB_TOKEN`).
- To change prices or models, edit `models.json` and `panel.json` together. Prices
  are USD per million tokens.
