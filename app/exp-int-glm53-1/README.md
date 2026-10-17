# jobq

A toy job queue used for CodeAF model experiments: a dispatcher (`Dispatch`)
that starts runs while there are free slots, a gate (`Decide`) that turns
each dispatch's inputs into a run, and an executor (`Execute`).

It is a fixture: model-experiment issues (label `codeaf-experiment`) copy it
to their own `app/exp-int-*` directory and add a feature there. Don't change
it while an experiment is running.

## The digest run (this experiment's feature)

When the backlog is low, spare slots go to a digest run, which summarises
recent activity. `digest.min_backlog` (default 2) is the queue size below
which the dispatcher starts one digest run per tick, on a free slot, when no
digest run is already in flight (`digest_in_flight` in the dispatch state).
A digest run's inputs are exactly `{"digest": "true"}`; the gate refuses any
other `digest` value, and `digest` together with `issue` or `review`. A digest
run has no number: `jobq decide` prints `mode=digest` with no `number=` line,
and performing it says `wrote digest`.
