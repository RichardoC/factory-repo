# AGENTS.md

Instructions for any coding agent working in this repository, including CodeAF
(which loads this file automatically) and Claude Code.

## Repository hygiene

- Don't commit caches, build output or tool scratch files: `__pycache__/`, `*.pyc`,
  compiled binaries, coverage profiles, editor and OS files, and similar. If a tool
  creates one, delete it, or add it to `.gitignore` when it will keep coming back.
- Don't commit secrets, tokens, `.env` files or logs that may contain them.
- Keep diffs focused on the task. Don't reformat or rename code you aren't otherwise
  changing.

## Go

- Run `gofmt -l .`, `go vet ./...`, `go build ./...` and `go test -race ./...` before
  you finish, and leave all four clean. CI runs the same checks.
- Use the Go version in `go.mod`. Run `go mod tidy` after changing dependencies and
  commit `go.sum` along with `go.mod`.
- Tests must not need a live Kubernetes cluster. Use client-go's fake clientset, or
  envtest/kind behind a build tag that CI opts into explicitly.
- Back a performance claim with a benchmark (`go test -bench`), and give before and
  after numbers.

## Repository layout

- `.github/` holds the CodeAF harness and CI. Agents running under the CodeAF workflow
  can't change it: edits there are discarded.
