# jobq

A toy job queue used for CodeAF model experiments: a dispatcher (`Dispatch`)
that starts runs while there are free slots, a gate (`Decide`) that turns
each dispatch's inputs into a run, and an executor (`Execute`).

This copy adds a digest run: when the backlog is below `digest.min_backlog`
(default 2) and a slot is free after the queued issues, the dispatcher starts
one run with inputs `{"digest": "true"}`; it is skipped while a digest run is
already in progress (`digest_in_flight` in the dispatcher state).

It is a fixture: model-experiment issues (label `codeaf-experiment`) copy it
to their own `app/exp-int-*` directory and add a feature there. Don't change
it while an experiment is running.
