# jobq

A toy job queue used for CodeAF model experiments: a dispatcher (`Dispatch`)
that starts runs while there are free slots, a gate (`Decide`) that turns
each dispatch's inputs into a run, and an executor (`Execute`).

It is a fixture: model-experiment issues (label `codeaf-experiment`) copy it
to their own `app/exp-int-*` directory and add a feature there. Don't change
it while an experiment is running.
