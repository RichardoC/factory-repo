# snyk-dast-linear-sync

Added by the factory when this app was imported from
github.com/tesslio/snyk-dast-linear-sync, which has no agent guidance of its
own. It follows its sibling app's rules (`app/snyk-linear-sync/AGENTS.md`).

Read [PROJECT.md](PROJECT.md) for the design contract and
[README.md](README.md) for configuration and behavior. When a change alters
behavior, update both in the same change.

## This repository is public

Everything here is public: code, comments, tests and fixtures, docs, commit
messages, branch names, PR titles and descriptions, and review comments.

Never include:

- Identifiers, titles, URLs or contents of real Linear tickets, teams or
  workspaces.
- Snyk organization IDs or slugs, project, target or scan IDs, finding IDs,
  or the URLs, hosts and paths of applications a deployment scans.
- Facts about a real incident: counts, dates and timelines, CI run or job
  IDs, log excerpts.
- How or where the sync is deployed, and the values it is configured with.
- People's names, and credentials of any kind.

Describe behavior in general terms and build tests from neutral fixtures:
`project-a`, `issue-1`, `SNYK-100`, `example.test` hosts, made-up IDs.

A few real-looking identifiers and incident figures predate these rules in the
upstream code and docs. Leave them as they are: don't copy them into new code,
tests, docs, commits or issues, and don't add more like them.

## Decisions to keep

- `SNYK_DAST_TEAM` scopes the sync on purpose. A target the team filter
  excludes is treated like a target that no longer exists, so its existing
  tickets are cancelled. That is intended (owner's decision, 2026-10-18); don't
  change it or propose changing it.

## Checks

Run these in this directory before pushing:

```sh
go mod tidy && git diff --exit-code go.mod go.sum
gofmt -l .            # must print nothing
go fix ./... && git diff --exit-code
go vet ./...
go test -race ./...
```
