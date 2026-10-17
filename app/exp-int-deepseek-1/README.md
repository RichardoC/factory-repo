# jobq

A toy job queue used for CodeAF model experiments: a dispatcher (`Dispatch`)
that starts runs while there are free slots, a gate (`Decide`) that turns
each dispatch's inputs into a run, and an executor (`Execute`).

This copy is a model-experiment variant (deepseek, copy 1). On top of the
seed it adds the **digest run**: when the backlog is low, the dispatcher uses
a spare slot for a run that summarises recent activity.

- `digest.min_backlog` (default 2) is the backlog below which a digest may
  run; a negative value is an error from `loadConfig`/`withDefaults`.
- At each tick the dispatcher starts at most one digest run, and only if a
  slot is still free after the queued issues, fewer issues are queued than
  `min_backlog`, and `State.DigestInFlight` (`digest_in_flight`) is false.
- A digest run's inputs are exactly `{"digest": "true"}`. `decide` accepts the
  `digest` input only when it is exactly `true` and it is exclusive with
  `issue` and `review`; a digest has no number, so `jobq decide` prints
  `mode=digest` with no `number=` line, and performing it says `wrote digest`.
