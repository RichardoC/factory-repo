# Issue #137 — the workflow part, for a maintainer

The queue now schedules review and continuation runs itself, so `publish` no
longer starts them with `gh workflow run`. The harness change (`.github/codeaf/`)
is in this pull request; the two workflow edits below are the part that needs a
maintainer, because the `publish` job's steps hold `github.token` and the
workflow policy as tightened by the owner requires a maintainer to apply them.

Apply the two patches from the repository root:

```
git apply .github/codeaf/issue-137-workflow/codeaf.yml.patch
git apply .github/codeaf/issue-137-workflow/codeaf-queue.yml.patch
```

Both are generated against the current `main` and apply cleanly (`git apply
--check` passes). They change only `.github/workflows/`.

## What they do

`codeaf.yml`:

- `run-name` (line 31): when `inputs.review` is set, render
  `CodeAF review #{review}{ (lane L)}` — the shape `runReviewRe` in `queue.go`
  reads back, and the shape the queue's `runs.json` mirror writes (issue #137).
- The `publish` step creates the two labels, best effort, right after it has
  the PR number: `codeaf-review-needed` and `codeaf-continue-needed`. Both names
  are the harness's own constants (`labelReviewNeeded`, `labelContinueNeeded`).
- `publish`'s continuation action (`continue-plan` returned `continue`): instead
  of `gh workflow run codeaf.yml -f pr=...`, it now only labels the PR
  `codeaf-continue-needed`; the queue picks it up and dispatches it with the
  `pr` input and a free lane.
- `publish`'s independent-review step: instead of `gh workflow run codeaf.yml -f
  review=...`, it labels the PR `codeaf-review-needed`.
- `publish`'s review-report `fix` action: instead of `gh workflow run codeaf.yml
  -f pr=...`, it labels the PR `codeaf-continue-needed` (a fix run is a
  continuation).

The `queue-next` job (which ticks `codeaf-queue.yml` after a run) is unchanged:
it already starts the queue once this run is over.

`codeaf-queue.yml`:

- `env`: the two new label names, kept identical to the harness's constants.
- The "Start queued issues while there are free slots" loop reads the new
  `kind=` line from `harness next`, and dispatches by kind: `-f review=$N -f
  lane=$LANE` for `review`, `-f pr=$N -f lane=$LANE` for `continue`, `-f
  issue=$N -f lane=$LANE` for `issue`. After dispatch it removes the matching
  `*-needed` label, and mirrors the run into `runs.json` under the title
  `codeaf.yml` will actually give it (`CodeAF review #N (lane L)` for a review,
  `CodeAF #N (lane L)` otherwise), so the next pick sees its lane and — for a
  continuation — its areas.

## Why not leave the dispatches in `publish`

A dispatch from `publish` carries no lane, so every such run lands on lane 0
while queued issue runs are also on lane 0; the queue cannot tell a review from
a continuation; and the run bypasses the queue's spend threshold and lane
capacity. Labelling instead lets the queue make the decision it already makes
for issues, under `max_parallel_runs` and the spend threshold.
