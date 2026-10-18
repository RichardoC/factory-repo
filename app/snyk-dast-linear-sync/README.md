# snyk-dast-linear-sync

Sync Snyk DAST (formerly Probely) findings with Linear issues since there isn't
an official integration.

Repo:

```text
github.com/tesslio/snyk-dast-linear-sync
```

## What It Does

- Authenticates to Snyk DAST with a JWT API key.
- Reads all targets accessible to the API key (optionally filtered to one team).
- Normalizes Snyk DAST findings into one Linear issue per `target + finding`.
- Stores a stable fingerprint in a hidden metadata block in the Linear issue description.
- Creates missing Linear issues.
- Updates existing Linear issues when managed fields change.
- Ensures a configurable managed label is applied to all managed issues, unless label management is explicitly turned off.
- Moves stale issues to the configured resolved state when the finding is no longer present but the Snyk DAST target still exists.
- Cancels managed Linear issues when their Snyk DAST target no longer exists, such as after target deletion.
- Uses a local SQLite cache to skip unchanged findings and unchanged Linear issues on steady-state runs.
- Sets Linear due dates from Snyk DAST finding creation time using configurable per-severity offsets.

The fingerprint format is:

```text
snyk-dast:<target-id>:<finding-id>
```

## Running

Quickstart without cloning:

Create a local `.env`, then run directly with the repo path:

```bash
go run github.com/tesslio/snyk-dast-linear-sync/cmd/snyk-dast-linear-sync@latest --env-file .env --dry-run
```

Or install the binary:

```bash
go install github.com/tesslio/snyk-dast-linear-sync/cmd/snyk-dast-linear-sync@latest
snyk-dast-linear-sync --env-file .env --dry-run
```

Default usage is to pass a dotenv file explicitly:

```bash
go run ./cmd/snyk-dast-linear-sync --env-file .env --dry-run
```

That avoids shell-specific `source` behavior and is the recommended way to run the tool.

Dry run:

```bash
go run ./cmd/snyk-dast-linear-sync --env-file .env --dry-run
```

Verbose dry run (logs each planned create with its title, state, due date,
labels, and full description, and each planned update/resolve with its title,
state, due date, and labels — so you can see exactly what the ticket will look
like in Linear before anything is written):

```bash
go run ./cmd/snyk-dast-linear-sync --env-file .env --dry-run --verbose
```

`--verbose` also works with a live (non-dry-run) run to log every operation
as it happens.

Normal run:

```bash
go run ./cmd/snyk-dast-linear-sync --env-file .env
```

Installed binary:

```bash
snyk-dast-linear-sync --env-file .env
```

Bypass cache:

```bash
go run ./cmd/snyk-dast-linear-sync --env-file .env --bypass-cache
```

`--env-file` uses `github.com/joho/godotenv`, so the file can be a normal dotenv file and does not need to be sourced by your shell.

## Validation

Run these before every commit. `go fix ./...` may automatically rewrite code
(for example, modernising loop syntax), so run it first, review the resulting
`git diff`, and include any changes in the same commit. Repeat until
`git diff --exit-code` is clean.

```bash
go fix ./...
git diff --exit-code
go test ./...
go vet ./...
go fmt ./...
```

CI enforces this: the `go fix` check runs `go fix ./...` and then
`git diff --exit-code`, so any unfixed code will fail the build.

## Current Behavior

- Uses `github.com/guillermo/linear/linear-api` plus direct GraphQL mutations for Linear.
- Talks to the Snyk DAST (Probely) REST API directly with `Authorization: JWT <key>`.
- Stores a human-facing Snyk DAST UI link in the Linear description.
- Keeps the Snyk DAST REST API link as a secondary reference.
- Includes target, vulnerable URL, HTTP method, path, parameter, insertion point, CWE, and CVSS details when Snyk DAST provides them.
- Batches Linear create and update mutations to reduce request pressure.
- Retries and backs off on Linear rate limiting.
- Normalizes common Linear markdown rewrites when comparing descriptions so steady-state runs do not churn.

## Snyk DAST (Probely) API Permissions

The sync is **read-only against Snyk DAST** — it never writes back. There is
no built-in read-only role for Snyk DAST, so you must create a **custom role**
(Enterprise plan) with exactly these two permissions:

| Permission | ID | Why |
|---|---|---|
| **View Target** | `view_target` | Grants `GET /targets/`, `GET /findings/`, and `GET /definitions/`. The permission description is "List and view Target, Target Settings, Scans, Scheduled Scans, and Findings." Without it, all three endpoints silently return empty results. The `/definitions/` endpoint is used to determine whether each finding is a passive or active check (for `LINEAR_CHECK_TYPE_LABELS`). |
| **Correlation Viewer** | `correlation_viewer` | Grants `GET /targets/{id}/findings/{id}/integrations/snyk-sast/correlations/`, used to surface the Snyk Code source correlation (responsible repo/file) in each Linear ticket. |

**Scope:** Account (Global) so the sync can see all targets, or scope to specific
Teams/Targets and set `SNYK_DAST_TEAM` to match.

Permissions **not** required (the sync does not write back to Snyk DAST):
`change_finding`, `change_finding_state`, `start_scan`, `start_retest`,
`create_target`, `delete_target`, `change_target_settings`,
`correlation_admin`.

> **Symptom of missing `view_target`:** the sync logs `loaded Snyk DAST
> targets count=0` and `loaded Snyk DAST findings count=0` even though the UI
> shows findings. This is the API filtering out everything because the API key
> has no target visibility — findings are scoped under targets. A token with
> only `change_finding` + `correlation_viewer` (a common mistake) will see
> zero findings for this reason. Add `view_target` to fix it.

See the Snyk docs on [roles and scopes by plan](https://docs.snyk.io/scan-fix-and-prevent/scan-with-snyk/snyk-api-web/managing-account/roles-and-scopes-by-plan) and [understanding permissions](https://docs.snyk.io/scan-fix-and-prevent/scan-with-snyk/snyk-api-web/managing-account/understanding-permissions).

## Linear Permissions

This project is designed to work with:

- `Read`
- `Create issues`
- `Update issues`

It does not require label creation permissions.
If `LINEAR_MANAGED_LABEL` is enabled, the configured label must already exist in Linear.

If `LINEAR_COMMENTS=true` is enabled, the sync also posts change-summary comments on updated issues, which requires Linear comment creation permission. This is off by default.

## Managed Linear Description

Each managed issue contains:

- a heading with vulnerability title and severity
- target name/host and target URL immediately below
- the vulnerable request context (URL, method, path, parameter, insertion point)
- CWE and CVSS context when available
- the fix description when provided
- Snyk Code source correlation block when Snyk DAST has linked a Snyk Code source vulnerability (surfaces the responsible repo/file)
- Snyk DAST UI and API links grouped together
- target and finding IDs lower in the body for debugging and API work
- hidden metadata block

The metadata block is required for deduplication and safe updates:

```text
<!-- snyk-dast-linear-sync
fingerprint: snyk-dast:target-123:456
managed_labels: snyk-dast-automation,snyk-dast-api
-->
```

Changing or removing that block can cause duplicate issues or prevent updates from matching the correct Linear issue.

## Issue Format

The synced issue body is optimized for fast developer triage:

- heading first: vulnerability title plus severity
- target and vulnerable request context immediately below
- Snyk DAST UI and API links grouped together
- fix context next
- target and finding IDs lower in the body for debugging and API work

The synced title includes the most useful DAST context:

- the scanned target host (falling back to target name)
- the vulnerable request as `METHOD path`

## Managed Labels

`LINEAR_MANAGED_LABEL` controls the automation label applied to managed issues:

- default: `snyk-dast-automation`
- set to another label name to use that label instead
- set to `off` to disable label management

When label management is enabled, the sync:

- adds the configured label to newly created managed issues
- preserves unrelated existing labels
- removes the previously managed label if the configured label changes
- removes the previously managed label if label management is turned off

If the configured label does not exist in Linear, the run fails with a clear message telling the operator to create the label or disable label management.

`LINEAR_CHECK_TYPE_LABELS` optionally maps Snyk DAST check types to additional managed Linear labels. Snyk DAST has two check types — **passive** (response analysis, e.g. missing security headers) and **active** (exploit payloads, e.g. SQL injection, XSS) — which is the DAST equivalent of Snyk's multi-product tool labels (`snyk-code`, `snyk-open-source`, etc.). Passive and active findings have different triage workflows, so labeling them separately helps route issues to the right team.

- format: comma-separated `check_type:label` pairs
- example: `passive:snyk-dast-passive,active:snyk-dast-active`
- labels must already exist in Linear

`LINEAR_CHECK_TYPE_LABEL_DEFAULT` controls the fallback label for check types without an explicit mapping:

- default: `off`
- set to `off` to disable the fallback

`LINEAR_TARGET_TYPE_LABELS` optionally maps Snyk DAST target `type` values to additional managed Linear labels:

- format: comma-separated `target_type:label` pairs
- example: `api:snyk-dast-api,single:snyk-dast-web`
- labels must already exist in Linear

`LINEAR_TARGET_TYPE_LABEL_DEFAULT` controls the fallback label for target types without an explicit mapping:

- default: `off`
- set to `off` to disable the fallback

`LINEAR_PROTECTED_LABELS` lists labels this sync must never add to, or remove
from, an existing ticket. Use it for labels another actor owns as control state —
a triage bot, a dispatch harness, a human workflow — where deleting one silently
undoes that actor's decision.

- format: comma-separated label names
- example: `df:dispatch,df:dispatch-complete`
- default: empty (no labels protected)
- set to `off` to disable protection entirely, overriding any value in an env file

Protection is needed because Linear's `issueUpdate` replaces a ticket's whole
label set rather than applying a delta: every update echoes back the label set
this sync last saw, which deletes anything another actor added since the run's
opening snapshot. So when any label is protected:

- each update batch re-reads the current labels of the issues it is about to
  write, and carries protected labels over from that live read rather than from
  the snapshot. A protected label added since the snapshot is preserved; one
  removed since the snapshot is not resurrected.
- if that live read returns nothing for an issue, or returns a truncated page of
  its labels, the update is refused rather than proceeding — writing a label set
  that might drop a protected label is worse than skipping one issue for one run.
  The caller retries issues individually, so the rest of the batch still lands,
  and the refusal is counted in `failed_ops`, which fails the run.
- protected labels are never asserted from the managed set either, so a
  misconfiguration cannot stamp another actor's control label onto a ticket that
  does not carry one.
- new issues need no special handling: a create has no pre-existing label set to
  replace.

The live read costs one extra query per update batch, so it is skipped entirely
when no labels are protected.

`LINEAR_UNSUBSCRIBE_ACTOR` controls whether the Linear API actor should be kept off the subscriber list for managed issue creates and updates:

- default: `true`
- `true`: create issues without subscribing the actor, and preserve the current subscriber list unchanged on updates
- `false`: let Linear use its default subscriber behavior

This setting only controls the issue subscriber list. Linear will still record the API user as the issue creator, and the create mutation response may briefly include that creator in `subscribers` even when the persisted issue subscriber list is empty after a refresh.

`LINEAR_COMMENTS` controls whether the sync posts a change-summary comment on each updated Linear issue, explaining which managed fields changed and why (state, due date, labels, etc.):

- default: `false`
- `true`: post a comment after each successful update batch
- `false`: no comments; the Linear activity log still shows what changed

When enabled, this requires Linear comment creation permission in addition to the base `Update issues` permission.

## State Mapping

Snyk DAST (Probely) finding states map to Linear workflow states:

- `notfixed` / `retesting` -> `Todo`
- `accepted` with a future `expiration_date` -> `Todo` (SLA from acceptance expiry)
- `accepted` with a past `expiration_date` -> `Todo` (the acceptance has lapsed; the due date is the acceptance expiry plus the configured severity offset, the same basis used while the acceptance was in force). A date-only `expiration_date` covers the whole of that day, so the acceptance lapses at the end of it, not at midnight.
- `accepted` without an `expiration_date` -> `Cancelled`
- `invalid` -> `Cancelled`
- `fixed` -> `Done`
- missing finding in an existing Snyk DAST target -> `Done`
- missing finding because the Snyk DAST target no longer exists -> `Cancelled`

The configured Linear state names are resolved by name first, then by workflow type where possible.

These distinctions are intentional:

- A time-limited `accepted` finding is kept open in `Todo` because it requires attention once the acceptance expires. Its due date is calculated from the acceptance expiry so the SLA extends to the normal severity offset from when the acceptance ends.
- Once that acceptance has lapsed, the finding stays in `Todo` rather than being cancelled. A time-limited acceptance is a request to be reminded, so treating an expired one as a permanent acceptance would silently close the ticket at exactly the moment it needs attention.
- A permanently `accepted` (no expiry) or `invalid` finding is cancelled: it is no longer actionable.
- If a Snyk DAST finding disappears but the target still exists, the tool treats that as the finding being resolved and moves the Linear ticket to `Done`.
- If the Snyk DAST target itself is gone, the tool treats the managed Linear ticket as no longer actionable and moves it to `Cancelled`.

### Manual Non-Terminal State Override

If a user manually moves a managed issue to any non-terminal Linear state that differs from what the sync would set (e.g. `Todo` or `In Progress` when the configured open state is `Triage`), subsequent syncs will preserve the user's chosen state. This prevents the automation from dragging issues back to the configured state after intentional triage decisions.

Terminal states (`Done`, `Cancelled`) are never preserved — the sync always transitions to a terminal state when the Snyk DAST finding is fixed or permanently accepted/invalidated.

### Snapshot Scope

Each run loads a snapshot of the sync's own Linear tickets. The query is scoped
by team, then by an archive window, then by identity:

```text
team = LINEAR_TEAM_ID
  AND (not auto-archived OR auto-archived within the lookback window)
  AND (title starts with "Snyk DAST:" OR carries LINEAR_MANAGED_LABEL)
```

Both identity predicates are exact — a title prefix and a label name — and either
alone identifies a managed ticket, since the sync builds every title with the
prefix and re-asserts the managed label on every update. Keeping both means a
retitled ticket, or one whose label was removed by hand, is still found rather
than being invisible and duplicated. With `LINEAR_MANAGED_LABEL=off` the label
clause is omitted rather than matching nothing.

This matters when the Linear team is shared with another sync: the scope must
exclude the other tool's tickets. The fingerprint metadata block is deliberately
**not** used as a query predicate — `description: { contains: … }` did not bound
the result set (a run against a shared team loaded 59,866 issues for 7 findings,
effectively the whole team), and Linear does not document `description` as
supporting the string comparators at all. The metadata block remains the
authority for identity once an issue is loaded; it just cannot be trusted to
select which issues to load.

### Archived Linear Issues

Linear auto-archives closed issues after the team's configured inactivity
period, and archived issues are excluded from Linear's default issue query.
Snyk DAST keeps `fixed`, `accepted`, and `invalid` findings in its API
indefinitely, so if the sync cannot see its own archived tickets it considers
them missing and creates a fresh duplicate. The replacement is itself visible
until Linear archives it in turn, at which point the finding looks ticketless
again — so the duplication repeats once per archive cycle rather than on every
run, and the copies accumulate.

The issue snapshot therefore requests archived issues explicitly, bounded to
those auto-archived within `LINEAR_ARCHIVE_LOOKBACK_DAYS` (default 3650 — ten
years, i.e. effectively the whole archive). The sync treats archived issues as terminal and
does not update, resolve, or cancel them in place. If a finding whose ticket has
been archived becomes active again, a replacement ticket is created.

Creating a replacement is a deliberate choice rather than an API limitation.
Linear exposes an `issueUnarchive` mutation with no documented precondition, so
restoring the original ticket and updating it would be possible. That path was
considered and not taken: each recurrence gets a clean ticket with its own SLA
clock, rather than one ticket reused indefinitely across recurrences.

#### Why the window is effectively unbounded

`LINEAR_ARCHIVE_LOOKBACK_DAYS` bounds how long ago a ticket was **auto-archived**
— not how long your team's auto-archive period is. Linear expresses that period
in *months* (`Team.autoArchivePeriod`), and the two values are independent: a
short window is not made safe by a short period.

The window applies only to auto-archived issues, because it filters on
`autoArchivedAt`. Issues archived manually or through the API — including
trashed (deleted) ones — carry `archivedAt` without `autoArchivedAt`, so they
match the not-auto-archived filter arms and are returned regardless of age. That
is deliberate: including them can only prevent duplicates, never cause them, and
the pinned `linear-api` binding predates `IssueFilter.archivedAt` so bounding
them is not expressible today.

Because Snyk DAST retains `fixed`, `accepted`, and `invalid` findings
indefinitely, each of them keeps a live desired issue forever. So any *finite*
window eventually drops the ticket out of the snapshot, the finding looks
ticketless, and a duplicate closed ticket is created. Worse, the copies do not
converge: the original is no longer visible, so the duplicate-cancellation pass
cannot pair them up, and one extra closed ticket accumulates per cycle.

Worked example with a one-month auto-archive period and a 35-day window: the
ticket archives ~30 days after closing, stays visible for 35 more days, and is
recreated at roughly day 65 — then that copy repeats the cycle. About one
spurious ticket every two months, per permanently-terminal finding, forever.

Hence the ten-year default, which covers the whole archive in practice. The
setting remains configurable purely as a size/latency escape hatch: lowering it
shrinks the snapshot and speeds up each run, at the cost of reintroducing the
cycle above. The snapshot query pages 100 issues at a time and costs roughly
6,300 of Linear's 10,000 per-query complexity points, so widening the window
costs extra pages rather than risking a rejected query.

The alternative fix — not creating tickets for findings that are already
terminal and have no ticket — was considered and rejected: every Snyk DAST
finding should end up with a Linear ticket, including findings closed before the
sync first ran, so the ticket history is complete.

### Due Dates

Default due date offsets:

- critical -> 15 days
- high -> 30 days
- medium -> 45 days
- low -> 90 days

The base date is normally the Snyk DAST finding `created_at` timestamp. For
time-limited `accepted` findings, the base date is the acceptance expiry date,
so the due date extends to the normal severity SLA calculated from when the
acceptance expires.

Past due dates are left as-is (Linear renders them as overdue) rather than
floored to today, both to show how long an issue has exceeded its SLA and to
avoid daily cache churn.

## Cache Behavior

The cache lives in SQLite and stores:

- a schema signature for the managed issue format
- a normalized hash for each Snyk DAST finding
- a normalized hash for each managed Linear issue

On a normal run:

1. Load Snyk DAST findings.
2. Load the current Linear snapshot.
3. Skip fingerprints whose source hash and Linear hash both match the last successful run.
4. Apply creates, updates, and resolves for the rest.
5. Refresh the cache from the live post-write Linear snapshot.

The cache fast-path never suppresses a pending move into a terminal state
(`Done`/`Cancelled`). Use `--bypass-cache` to ignore the cache for a run and
rebuild it from live data.

## Configuration

Required:

- `SNYK_DAST_API_KEY`
- `LINEAR_API_KEY`
- `LINEAR_TEAM_ID`

Optional:

- `--env-file`
- `SNYK_DAST_API_BASE`
- `SNYK_DAST_APP_BASE`
- `SNYK_DAST_TEAM`
- `LINEAR_STATE_TODO`
- `LINEAR_STATE_BACKLOG`
- `LINEAR_STATE_DONE`
- `LINEAR_STATE_CANCELLED`
- `LINEAR_MANAGED_LABEL`
- `LINEAR_CHECK_TYPE_LABELS`
- `LINEAR_CHECK_TYPE_LABEL_DEFAULT`
- `LINEAR_TARGET_TYPE_LABELS`
- `LINEAR_TARGET_TYPE_LABEL_DEFAULT`
- `LINEAR_PROTECTED_LABELS`
- `LINEAR_UNSUBSCRIBE_ACTOR`
- `LINEAR_COMMENTS`
- `LINEAR_ARCHIVE_LOOKBACK_DAYS`
- `LINEAR_DUE_DAYS_CRITICAL`
- `LINEAR_DUE_DAYS_HIGH`
- `LINEAR_DUE_DAYS_MEDIUM`
- `LINEAR_DUE_DAYS_LOW`
- `SYNC_WORKERS`
- `SNYK_DAST_HTTP_CONCURRENCY`
- `LINEAR_HTTP_CONCURRENCY`
- `ERROR_LOG_FILE`
- `CACHE_DB_FILE`

See [.env.example](/.env.example).

## Diagnostic Tool

`cmd/investigate-duedate` is a helper for inspecting the due-date decision for a
single managed Linear issue. It loads the issue by identifier, finds the
matching Snyk DAST finding, and prints every plausible due-date scenario plus
the cache state:

```bash
go run ./cmd/investigate-duedate --env-file .env --issue SEC-12127
```

Add `--json` for machine-readable output.

## Exit Status

The sync exits non-zero when a state-changing Linear operation was left
permanently unapplied, so a scheduler or CI job notices the drift instead of
seeing a successful run that quietly dropped work.

"Permanently" is the key word. Batch mutations are retried per entry, and only
the outcome after that retry counts:

- a batch that fails and whose retry lands is a **successful** run — the
  operation applied, it just took two calls
- a create, update, resolve or duplicate-cancellation whose retry also failed, or
  which was deliberately left for the next run because reconciliation was
  impossible, is counted in `failed_ops` and **fails** the run
- `failed_comments` never fails the run: a missing change comment leaves the
  synced issue state correct, and comments are opt-in via `LINEAR_COMMENTS`

Both counts appear in the `sync complete` summary log either way, and that
summary is emitted before the exit status is decided.

## Logs

- Console logs show startup, load progress, work progress, cache refresh, and final summary.
- Error logs are appended to `ERROR_LOG_FILE`.
- Default error log path: `logs/snyk-dast-linear-sync-errors.log`

## More Detail

See [PROJECT.md](/PROJECT.md) for the project intent, architecture, sync rules, and operational model.
