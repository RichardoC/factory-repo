# jobq

A toy job queue used for CodeAF model experiments: a dispatcher (`Dispatch`)
that starts runs while there are free slots, a gate (`Decide`) that turns
each dispatch's inputs into a run, and an executor (`Execute`).

It is a fixture: model-experiment issues (label `codeaf-experiment`) copy it
to their own `app/exp-int-*` directory and add a feature there. Don't change
it while an experiment is running.

## Digest run

When the backlog is low, the dispatcher spends one spare slot on a digest
run, which summarises recent activity.

- Config: a `digest` section with `min_backlog` (`Config.Digest.MinBacklog`).
  It defaults to 2 when unset or 0; a negative value is an error from
  `loadConfig`/`withDefaults`, like the other settings.
- When: at each tick the dispatcher starts at most one digest run, and only
  if a slot is still free after the queued issues have been started, fewer
  issues are queued than `min_backlog`, and no digest run is already in
  progress (`State.DigestInFlight`, JSON `digest_in_flight`).
- Inputs: exactly `{"digest": "true"}`. A digest run takes a slot like any
  other run.
- The run: it is started like any other run and performed by `Execute`, which
  says `wrote digest`. It has no issue or PR number, and `jobq decide` prints
  `mode=digest` with no `number=` line.
- Validation: the `digest` input must be exactly `true`; `digest` together
  with `issue` or `review` is refused. Issue and review runs behave exactly
  as before.
