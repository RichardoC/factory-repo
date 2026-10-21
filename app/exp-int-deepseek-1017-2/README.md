# jobq

A toy job queue used for CodeAF model experiments: a dispatcher (`Dispatch`)
that starts runs while there are free slots, a gate (`Decide`) that turns
each dispatch's inputs into a run, and an executor (`Execute`).

It is a fixture: model-experiment issues (label `codeaf-experiment`) copy it
to their own `app/exp-int-*` directory and add a feature there. Don't change
it while an experiment is running.

## Digest run

When the backlog is low, the dispatcher spends a spare slot on a **digest
run**, which summarises recent activity instead of working an issue:

- **Config.** `digest.min_backlog` (in `jobq.json`) defaults to 2 and must not
  be negative. The default applies when the key is unset or 0.
- **When.** At each tick `Dispatch` starts queued issues first, oldest first,
  then at most one digest run -- only when a slot is still free after the
  queued issues have been started, fewer issues are queued than
  `min_backlog`, and no digest run is already in progress
  (`State.DigestInFlight`, JSON `digest_in_flight`).
- **The dispatch.** A digest run's inputs are exactly `{"digest": "true"}` and
  it takes a slot like any other run.
- **The run.** `Decide` accepts only an exactly-`"true"` digest input that
  stands alone (`digest` together with `issue` or `review` is refused), and
  `Execute` performs it as `wrote digest`.
- **CLI.** `jobq dispatch CONFIG.json STATE.json` prints the digest dispatch
  line like any other; `jobq decide EVENT.json` for a digest event prints
  `mode=digest` with `run=true` and **no** `number=` line -- a digest run has no
  issue or PR number.
