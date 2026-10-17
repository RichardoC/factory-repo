# snyk-linear-sync

Read [PROJECT.md](PROJECT.md) for the design contract and [README.md](README.md)
for configuration and behavior. When a change alters behavior, update both in
the same change.

## This repository is public

Everything here is public: code, comments, tests and fixtures, docs, commit
messages, branch names, PR titles and descriptions, and review comments. Merged
commit messages cannot be edited afterwards. Changes are often motivated by
what the sync did in a real deployment, but the details of that deployment stay
out of the repository.

Never include:

- Identifiers, titles, URLs or contents of real Linear tickets, teams or
  workspaces.
- Snyk organization IDs or slugs, project IDs or names, issue IDs or keys, or
  target file paths from a real organization.
- Names, paths, branches or commit SHAs of the repositories a deployment scans.
- Facts about a real incident: counts, dates and timelines ("N projects were
  deactivated on <date>"), CI run or job IDs, workflow names, log excerpts.
- How or where the sync is deployed, and the values it is configured with
  (state, label and team names, schedules, cache setup).
- People's names, and anything from conversations with vendors or customers.
- Credentials of any kind.

Instead, describe the behavior in general terms ("when Snyk deactivates a
project and later reactivates it, the reopened tickets ...") and build tests
from neutral fixtures: `owner/repo`, `project-a`, `issue-1`, `SNYK-100`,
`example.test` hosts, and made-up commit SHAs such as forty `1`s. Do not copy
real names into fixtures; if you come across one, replace it.

Before every push and before opening or editing a PR, reread the diff, the
commit messages and the PR description for anything in the list above.

## Checks

CI (`.github/workflows/ci.yaml`) runs these; run them before pushing:

```sh
go mod tidy && git diff --exit-code go.mod go.sum
gofmt -l .            # must print nothing
go fix ./... && git diff --exit-code
go vet ./...
go test ./...
go test -race ./...
```
