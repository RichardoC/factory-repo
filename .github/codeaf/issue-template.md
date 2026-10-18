## Problem

What is wrong or missing. One or two paragraphs is enough.

## Change

What to do about it. Keep it concrete: files to touch, behaviour to add or
change.

## Done when

Bullets, each a test name or a command and its expected output. These are the
acceptance criteria: the worker reports against them and the reviewer
re-runs them.

- TestSomething: `go test -run TestSomething ./...` passes.

## End-to-end check

The one command or run that proves the seams of the change work end to end —
across the units, not just inside them — and what it must print.

An `area:` label (`area:harness` or `area:app/<name>`) is required before the
queue will start this issue.
