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

Every run posts a report comment with CodeAF's outcome, a per-model cost table —
including each model's reasoning share (exact from reasoning tokens, or an estimate
labeled “(chars)” from character counts when the model does not report them) — and the
run's timing figures (wall time, time with a call in flight, tokens/s, longest call).
Three workflow artifacts are uploaded, none of them encrypted:
- `codeaf-output-*` (14 days): the patch, its base commit, CodeAF's JSON result
  and the call ledger (each call's start and duration, slot wait, reasoning tokens,
  and the reasoning/output character counts backing the estimate);
- `codeaf-diagnostics-*` (30 days): the brief, CodeAF's stderr, its run records
  (prompts, replies, tool output), call log and usage, and the proxy log;
- `codeaf-state-*` (14 days): what the run left in CodeAF's home, and what it
  cost, for the shared state below.

### Independent review of every PR (issue #32)

Every CodeAF run that opens or updates a pull request is followed by one
**review run** — a second CodeAF run in read-only review mode. The reviewer
sits on a different model from the worker that wrote the patch (worker
**GLM-5.3-Flash**, reviewer **DeepSeek-V4.1-Flash**, both from
[`models.json`](.github/codeaf/models.json)), so it does not share the same
blind spots. It reviews the PR's diff against the issue for correctness,
concurrency, error handling and whether the tests actually exercise the
claims, and it re-runs the benchmarks the patch claims.

It posts one findings comment on the PR, with a severity and a file:line for
each finding. When anything is high or medium, that comment doubles as a
`/codeaf fix` command and a fix run starts on the PR's branch, counted
against the same continuation cap as the auto-continue feature — **2 per PR**.
When nothing blocks, the PR gets the **`codeaf-review-passed`** label, so a
person or a merge rule can merge with confidence.

The review run never publishes anything: it has no write token, and `publish`
applies no patch from it. Starting it goes through the same gate, spend caps
and state merge as any other run. The workflow change itself needs a
maintainer's approval, like any other change under `.github/`.

## How it is wired

- [`.github/workflows/codeaf.yml`](.github/workflows/codeaf.yml) runs three jobs,
  each checked out at the default branch:
  - `gate` checks the event against the rules above (`harness gate`).
  - `run`, which has no write token, builds CodeAF at the commit pinned in
    [`codeaf-ref`](.github/codeaf/codeaf-ref), writes a brief from the
    issue/PR, runs `codeaf do` headless and hands over a patch. The agent
    runs as its own unprivileged user -- created fresh each run with no
    sudo, handed the working tree and CodeAF's home for the run, started in
    a session of its own -- so it can neither signal nor read the runner
    (`Runner.Worker`, the pricing proxy) or use the runner's sudo; the
    workflow proves that with probes before the agent starts.
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
    [`models.json`](.github/codeaf/models.json), at synthetic.new's **subscription
    rates** — the prices that draw down the weekly credit, so CodeAF's estimates and
    model routing see what usage really costs against the quota; the same file also
    carries each model's API list rates (`api_*`), what overage past the quota costs
    (both checked 2026-10-17, [synthetic.new/pricing](https://synthetic.new/pricing));
  - adds `usage.cost` to every response, which is where CodeAF's spend accounting
    reads cost from;
  - refuses any other model, and returns HTTP 402 once the run reaches its per-run
    credit cap (**$5**, `limits.run_credit_usd`) or the week's credit is gone
    (`limits.weekly_credit_usd`, from the shared state); with `overage_usd` 0 (the
    default) it stops there rather than pay API prices;
  - asks synthetic.new for the subscription state once at proxy start
    (`GET /v2/quotas` — the call does not count against the subscription's limits),
    logs the raw response into the diagnostics with no key, shows its fields in the
    report, and refuses a call early when the quota reports exhausted and overage is
    0. The response's units are undocumented, so the parse is defensive and an
    unfamiliar shape never fails a run;

  The synthetic.new key (`SYNTHETIC_API_KEY` secret) is held only by the proxy. It is
  passed in on stdin, not through the environment.
- Model choice: the seats are spread across models, because the synthetic.new
  subscription allows only **2 calls to each model at once**:
  - the worker runs on **GLM-5.3-Flash** ($0.10 in / $0.25 out per M tokens at
    subscription rates);
  - the planner and checker run on **DeepSeek-V4.1-Flash** ($0.06 / $0.30).
  - CodeAF runs at most 2 tasks at once (`task.parallel`), so the workers
    fit in GLM-5.3-Flash's 2 slots.
  - The proxy enforces the limit per model (`max_concurrent` in
    `models.json`): a call past it waits for a slot instead of reaching
    synthetic.new. Each wait is written to the proxy log and the call
    ledger, so the diagnostics show whether a run was held up by it.
  - Escalation is switched off for now. CodeAF's cascade
    ([`panel.json`](.github/codeaf/panel.json)) would escalate to
    **DeepSeek-V4.1-Flash** ($0.06 / $0.30) and then **GLM-5.3** ($0.40 / $2.20),
    but it has two bugs:
    - it treats a reply made only of tool calls as empty, and fails every run;
    - its pinned rungs all call the seat model, so it never escalates.
  - [`codeaf.yml`](.github/workflows/codeaf.yml) explains how to turn it back on
    once CodeAF is fixed.

### Web search through the proxy (issue #58)

The proxy also serves CodeAF's web searches: `POST /v2/search` on `127.0.0.1`,
forwarded to [synthetic.new's search API](https://dev.synthetic.new/docs/synthetic/search).
CodeAF calls it with `harness search "<query>" [--limit N]` (the `harness`
binary is on the agent's `PATH`) and gets plain text back — numbered blocks
with title, URL, published date and the result text, capped per result. The
request is validated before forwarding (a non-empty `query` of at most 1,000
characters; only `query` is forwarded) and the response is capped at 1 MiB.
The key stays inside the proxy, as above — CodeAF never sees it. The query
itself does leave the runner and reach synthetic.new (zero data retention per
its docs); the price is undocumented, so each search is recorded in the ledger
with kind `search` and cost 0 until it is priced, and the report shows a "web
searches" row when a run made any. `limits.searches_per_run` (50 in
[`models.json`](.github/codeaf/models.json)) caps the searches one run may
forward; past it the proxy answers HTTP 429. Search results are untrusted text
from the internet — data, not instructions (see
[`AGENTS.md`](AGENTS.md#web-search)).

### Billing model (Big plan, issue #59)

The synthetic.new account is on the **Big** plan ($90/mo). Usage draws down a
**weekly credit** at the subscription rates, and the credit regenerates 2% of its
total every 202 minutes (~3.4 h) — a full refill in about a week. Past the quota,
usage is billed at the **API list prices**, up to `limits.overage_usd`; at 0 (the
default) the factory stops rather than pay beyond the $90. The Big plan's weekly
credit is not published, so `weekly_credit_usd` (22, an estimate from `/v2/quotas`) is a conservative
**assumption** — calibrate it from `GET /v2/quotas`'s `subscription.limit` once
its units are known. `run_credit_usd` (5) caps one run; the weekly credit and the
overage cap are shared by every run of the week through the shared state, and are
sized so several overlapping runs (`task.parallel` 2) can share the week.

What the harness does with it:
- the catalog prices CodeAF sees are the subscription rates — the credit;
  `api_input`/`api_cached`/`api_output` in the same file price the overage;
- the shared state carries the rolling 7-day credit spend; `harness state load`
  outputs the day's and the week's figure, and the queue starts a run only while the
  rolling week is under 75% of the weekly credit (`CODEAF_WEEK_SPENT_USD` — an
  unknown spend starts nothing);
- the proxy refuses at the weekly credit unless `overage_usd` > 0, and prices
  past-quota calls at the API rates against the overage cap;
- at proxy start it calls `GET /v2/quotas` (the call does not count against the
  subscription's limits), logs the raw body into the diagnostics with no key, shows
  it in the report, and refuses when the quota reports exhausted and overage is 0;
  the parse is defensive — unfamiliar fields never fail a run.

CodeAF's own config maps onto this as closely as its settings allow: CodeAF has no
weekly-budget key, and `daily_budget_usd`'s window is a single run's lifetime, so
it is set to 20 (the weekly credit, the closest match) with `models.crew.cap` 20;
`task_cap` 5 matches `run_credit_usd`.

**Fast and Infinite modes (beta) — follow-up only, nothing enabled.** Fast mode is
not enabled on the account and is not requested: the goal is more tickets, not
faster ones. Infinite mode is a proposed follow-up: its requests never count against
the rate limit, run outside the quota but much slower, and are DeepSeek-V4.1-Flash
only — a way to add review/planner capacity without using the weekly credit. A
request opts in per request: per the API docs (checked 2026-10-17,
[dev.synthetic.new/docs/api/models](https://dev.synthetic.new/docs/api/models)), by
sending the model as the `infinite:` alias (`infinite:small:vision` resolves to
DeepSeek-V4.1-Flash), which requires `stream: true` and a Big plan. The `:flex`
model suffix suggested in the issue is not documented anywhere reachable.

## The queue

Issues labelled **`codeaf-queued`** are the backlog. Every 10 minutes (and on
demand, via *Run workflow*)
[`codeaf-queue.yml`](.github/workflows/codeaf-queue.yml) asks the harness
(`harness next`) to start the next run: the oldest queued issue —
**`codeaf-priority`** jumps the queue — for which all of these hold:

- fewer than `queue.max_parallel_runs` (in
  [`models.json`](.github/codeaf/models.json), default 2) `codeaf.yml` runs
  are queued or in progress — issue runs, PR continuations and review runs
  alike;
- no area conflict with an in-flight run: areas are `app/<name>/` per app
  and the harness `.github/`, taken from `area:` labels or paths in the
  issue body; an issue with no area conflicts with everything;
- every issue named in a "Depends on #N" line in its body is closed;
- the rolling week's credit spend in the shared state is under 75% of
  `limits.weekly_credit_usd` in [`models.json`](.github/codeaf/models.json) — an
  unknown spend starts nothing;
- the issue is queued and not already active.

A run is started by adding the **`codeaf`** label — the same trust gate as a
person adding it — and then by dispatching
[`codeaf.yml`](.github/workflows/codeaf.yml) with the issue number. The
dispatch step exists because events raised with `GITHUB_TOKEN` (labelling
included) do not trigger other workflows; `workflow_dispatch` is the one event
it can start. The queue starts runs only up to the parallel-run limit, and
never two on the same area, so queued work overlaps only where those rules
allow ([running several tickets at once](#running-several-tickets-at-once-issue-60)).
Runs started by hand (a label or a `/codeaf` comment) can still overlap with
them; they are not held in a single
concurrency group, because GitHub would cancel all but the newest pending run
in it and silently drop a command.

Manual controls: *Run workflow* on either workflow — `codeaf-queue.yml` runs
the queue's checks now, `codeaf.yml` starts an issue when given its number.
Known failure modes:
- a "Depends on #N" line naming an open pull request blocks too (the issues
  API lists PRs);
- if the dispatch step fails after labelling, the issue keeps `codeaf` but
  nothing runs — a maintainer runs `codeaf.yml` by hand with the issue number;
- a self-dependency or a dependency cycle blocks the issue until its body is
  edited;
- two queue ticks in quick succession can miss a just-dispatched run in the
  runs list, and the runs then share lane 0 — the per-model per-run slots
  keep the account limit safe regardless.

### Running several tickets at once (issue #60)

The queue starts up to **`queue.max_parallel_runs`** runs at once (in
[`models.json`](.github/codeaf/models.json), default 2). Everything
`codeaf.yml` runs counts toward that limit while it is queued or in
progress: issue runs, continuations of runs that stopped early, and review
runs alike.

**Lanes.** Each run the queue starts gets the lowest free lane, 0 through
`max_parallel_runs - 1`. The lane picks the run's worker seat model from the
allowed list in `models.json`, as `models[lane mod count]`, so two
concurrent runs rarely compete for one model's call slots; lane 0 keeps
today's default seat, GLM-5.3-Flash. The lane travels to the run as a
digits-only `workflow_dispatch` input, validated by the gate and validated
again by `seat-config` inside the run job — a missing or malformed lane
means lane 0. The queue reads lanes back from run titles, which read
`CodeAF #N (lane L)`; titles are untrusted data, and a title that does not
parse counts as lane 0.

**The per-model limit stays account-wide.** synthetic.new allows 2 calls to
each model at once across the whole account, and every run has its own
proxy, so two runs could put 4 calls on one model. Instead each run gets a
slice of every model's `max_concurrent`: per-run slots =
`max_concurrent / max_parallel_runs`, at least 1, with CodeAF's
`task.parallel` set to match. The proxy enforces the slice, and `loadConfig`
refuses a `max_parallel_runs` that would overflow a model, so runs sharing
one model never sum past the account's 2. Manual runs (a label or a
`/codeaf` comment) carry no lane and still overlap without coordination; the
slice caps them at 1 call per model per run.

**Area conflicts.** The queue does not start a run on an area another run is
already working. Areas are `app/<name>/` for each app and the harness
(`.github/`), taken from `area:harness` and `area:app/<name>` labels or from
paths in the issue body. An issue with no area conflicts with everything,
and so does an in-flight run whose area is unknown.

**The budget.** Unchanged: the queue holds new starts once the shared
rolling-week spend reaches 75% of `limits.weekly_credit_usd`, and all
concurrent runs draw that budget down together.

**Pricing.** At the subscription rates, DeepSeek-V4.1-Flash has the cheapest
input price (0.06 USD/M tokens), so a non-zero lane can spend less credit
per ticket — but it is a weaker model (`role: mid`), so the rotation trades
a little quality on busy days for no model contention.

**Where to see it.** The queue job's log lists the runs in progress, the
lanes in use, and why each candidate started or was held.

### Model experiments

To tell what is down to the model from what is down to the harness, the
same task can be run with different seat models. An experiment issue carries
the `codeaf-experiment` label and runs only when dispatched with a `profile`
input naming one of the profiles in `.github/codeaf/experiments.json`:

```sh
gh workflow run codeaf.yml -f issue=N -f lane=0 -f profile=kimi
```

A profile pins the worker, planner and checker seats to models from
`models.json`; the gate and `seat-config` both validate the name. Labels and
`/codeaf` comments never start an experiment issue, and a profile on any
other issue is refused. A profile run gets no independent review and no
continuation, so each result is that profile's unaided work. Models with
role `experiment` (Kimi-K3) are allowed by the proxy but never picked as a
lane's worker.

### Runs that stop early (issue #31)

A run can stop before it is finished: CodeAF reports `stop: deadline` (it used
its 100 minutes), `incomplete`, or a budget stop — the per-run credit cap, or the
weekly credit with `overage_usd` 0, in [`models.json`](.github/codeaf/models.json).
The patch is still published, as a
**draft** pull request whose body says the run stopped early, why, and that the
work is unverified: after the push, `publish` asks the harness
([`main.go`](.github/codeaf/main.go), `harness pr-mode`) how the run ended.

For a stop a continuation can help (`deadline`, `incomplete` and the budget
family; other stops, an `error` say, leave the draft to a person), the workflow
then posts `/codeaf continue: <reason>` on that PR and dispatches
[`codeaf.yml`](.github/workflows/codeaf.yml) with its new `pr` input, which
starts a run on the PR's branch. The continuation's brief carries the previous
run's report and what the issue the PR closes still requires. The chain is
capped at **2 continuations per issue**, counted from the PR's own CodeAF
report comments — the first report is the run that opened the PR, not a
continuation. At the cap the PR gets a comment and stays a draft for a person
(a maintainer can still continue it by hand with a `/codeaf` comment). A run
that ends with `stop: done` marks the PR ready for review.

### Bringing the base branch in (issue #57)

A pull request that conflicts with the default branch used to stay dirty
forever. The run job hands `publish` a patch against the PR branch's head,
`publish` applies it and pushes a plain commit, and nothing ever made a
merge commit -- so the branch never contained `main`, and a person had to
resolve the conflict by hand. In PR mode the run job now merges the PR base
branch's tip into the PR head **before** CodeAF works. A clean merge is
committed as an ordinary merge commit. A conflicting merge is committed
too, with the conflict markers left in the files: the run's base commit
becomes that merge commit, and the brief names the conflicted files and
tells CodeAF to remove every conflict marker (the lines starting with
`<<<<<<<`, `=======` and `>>>>>>>`). The merge commit exists only in the
run job's checkout -- the run has no write token and never pushes.

`publish`, which treats everything from `run` as untrusted data, recreates
the merge commit itself from the same two parents (the PR head and the
base tip) and refuses unless the tree hash it computes matches the tree
recorded in the run's `merge-info.json` artifact (its fields: a kind of
`clean` or `conflicted`, commit, base, onto, tree, files). It then applies
the patch -- the patch is what removes the markers -- and refuses the
whole publish if any conflict marker is left in the previously conflicted
files. When there is no `merge-info.json` (the run died before merging,
say), `publish` keeps a fallback: it merges the base tip first, then
applies the patch. The invariants do not move: `git apply` is still the
only thing applied from `run`, the `.github/` approval check still applies
to the staged patch diff only, and every new commit still descends from
the PR head -- no force-push.

One limit: if the base branch changed anything under `.github/workflows/`
since the PR branched, the base is **not** brought in, by the run job or by
`publish`. GitHub refuses a `GITHUB_TOKEN` push that carries workflow
changes, so the patch is pushed alone, as before, and the PR stays behind
its base until a maintainer updates the branch.

### Keeping CodeAF current (issue #35)

The CodeAF version is pinned by a full commit SHA in
[`codeaf-ref`](.github/codeaf/codeaf-ref), a config file under `.github/codeaf/`
rather than a workflow variable: [`codeaf.yml`](.github/workflows/codeaf.yml)
reads it from the default-branch checkout and refuses to run if the file is
missing or not exactly 40 hex characters. A bump is now a one-line change to
that file, which publishes like app code and needs no maintainer approval —
unlike the old hardcoded `CODEAF_REF` env in the workflow, which made every
bump a workflow edit that needs one.

Every Monday morning [`codeaf-ref-check.yml`](.github/workflows/codeaf-ref-check.yml)
(scheduled, and on demand via *Run workflow*) compares the pin with upstream
`Agent-Field/codeaf` `main` (`harness ref-check`). When upstream has moved, it
opens — or updates, if one exists — **one** issue, labelled
**`codeaf-ref-update`** and **`codeaf-queued`** for the queue, listing the
upstream commits since the pin and asking CodeAF to say which of the
workarounds documented in this README they make unnecessary, and to bump the
pin and remove those workarounds only if a test run on a small task succeeds.
When the pin has caught up, the check closes the issue again.

A bump can also be done by hand: edit the pin file to the new full commit SHA.
Removing workflow-level workarounds is a workflow change and still needs a
maintainer's approval (the `codeaf-harness` environment, see
*Limits worth knowing* below).

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
  environment refuses the patch rather than publishing it.
  - **Workflow changes without a maintainer.** A patch whose only such
    changes are top-level workflow files is published anyway if it passes
    the deterministic workflow policy (`harness workflow-policy`). The
    policy lets the patched workflows do nothing the default branch's
    can't:
    - no new triggers, permissions or environments;
    - no new or edited steps that reference secrets;
    - no new or unpinned actions;
    - no new `github.event` interpolation;
    - no new or deleted workflow files.

    The PR is labelled `codeaf-workflow-change`. The independent review run,
    on another model with a workflow-security checklist, must pass it before
    it is merged, and it is never merged automatically. Anything the policy
    refuses, such as an edit to a step that holds a secret, still waits for a
    maintainer.

  One-off setup:
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
  - the rolling 7-day credit spend — credit at subscription rates, from the runs
    merged so far. The weekly credit regenerates continuously (2% of the total every
    ~202 minutes), and this rolling sum is what stands in for the refill; `state
    load` outputs both the day's and the week's figure.

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
  - [`models.json`](.github/codeaf/models.json): the spend caps (`limits`: the plan,
    the weekly credit, the per-run credit and the overage cap) and the allowed models
    with their prices (USD per million tokens: the `input`/`output`/`cache_read`
    subscription rates draw down the weekly credit, `api_input`/`api_cached`/`api_output`
    are the API list rates overage is billed at), how many calls
    each may have in flight (`max_concurrent`), and the reasoning efforts each
    model accepts (`efforts`; a request for an unlisted effort goes to the next
    one the model lists, `enabled: false` to its lowest, so nothing is sent raw
    that the model does not list — see *GLM reasoning effort* below);
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

### GLM reasoning effort (issue #24)

GLM returns empty answers at `low`, so a request for an unlisted effort goes to the next
one the model lists; `low` is never sent to GLM. Measured 2026-10-17 on a real multi-step Go edit-and-test task through the
live proxy, 3 repeats per case (results.md, issue #24): `medium` matched `high` on correctness
for both models, saving ~19.3% median completion tokens and ~17.8% median wall s on
glm-5.3-flash, ~35.9% and ~32.1% on glm-5.3. Both now send `low`/`minimal`/`medium` as
`medium`: glm-5.3 cleared the 20% threshold, and the owner chose `medium` for glm-5.3-flash
too, since about 72% of its output is reasoning (the report's reasoning-share column).

| model | effort | median completion tokens | median wall s | success |
|---|---|---|---|---|
| glm-5.3-flash | medium | 993 | 19.8 | 3/3 |
| glm-5.3-flash | high | 1231 | 24.1 | 3/3 |
| glm-5.3 | medium | 854 | 17.8 | 3/3 |
| glm-5.3 | high | 1332 | 26.3 | 3/3 |

### GLM reasoning share (issue #25)

synthetic.new never sends `completion_tokens_details.reasoning_tokens` for GLM, so its
reasoning-token column reads `—` and the exact reasoning share is unavailable. GLM does
stream its reasoning as `reasoning_content` (and returns it in the non-streamed `message`),
so the proxy counts those characters — plus the visible `content` and tool-call argument
characters — out of the chunk/response decode it already performs, records them in the
ledger as `reasoning_chars`/`output_chars`, and the report renders such models' reasoning
share as an estimate labeled “(chars)” (`reasoning ÷ (reasoning + visible output)`).
Models that do report reasoning tokens keep the exact token-based share. Character counts
are bytes, not runes; the proportion — not the absolute figure — is what the estimate uses.
