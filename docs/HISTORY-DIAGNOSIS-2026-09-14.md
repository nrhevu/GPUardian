# History: claimed sessions remain active and Alpha telemetry stalls

Resolved in production on 2026-09-14; see [the hotfix report](HISTORY-FIX-2026-09-14.md).

Investigated on 2026-09-14, approximately 17:35–17:40 Asia/Ho_Chi_Minh.
Gateway image: `ghcr.io/nrhevu/gpuardian:35f2e2dd4b61`.

## Findings

Two ingestion defects explain the reported symptoms. The gateway pod is
healthy and the Alpha daemon is running. The Schedule UI shows current GPU
usage while History is stale; those views use different data paths.

### Completed claimed sessions remain active after a job changes session

`internal/history/ingest.go`, `applyJob`, changes `jobs.session_id` to the
session referenced by the newest job event. A job first observed as claimed
can later carry a reservation group, moving its primary association to the
reservation while retaining the earlier association in `job_sessions`.

The finalization query only runs when the currently selected session is
`claimed_run`, and only considers `jobs.session_id`. It does not recompute
previously linked claimed sessions. The read path counts jobs through both
the primary association and `job_sessions`, so the UI can show a finished
job under a claimed session whose `finalized_at_ms` is still NULL. A claimed
session with NULL `finalized_at_ms` is always classified as active.

Read-only SQL against the live database confirmed these visible claimed
sessions have linked jobs, all of which have `finished_at_ms` set:

| Node | Incorrectly active sessions |
| --- | ---: |
| gpu-onenexus-alpha-MI355x8 | 8 |
| gpu-onenexus-beta-mi355x8 | 10 |
| gpu-sonle-mi350x8 | 3 |

For example, Alpha session `sess_c87da38ca1499fc3ee8ebc04` still appears
active, but its job `exec_129ef429227377ca2da30015` finished at
`2026-09-13T22:17:07Z` with reason `process_gone`. The job now points to a
reservation, and the old claimed association remains in `job_sessions`.

### A timestamp constraint blocks new Alpha telemetry

The live Alpha cursor and maximum committed event sequence both remain
`76720`. Last successful History sync is `2026-09-13T22:20:32Z`, or
**05:20:32 on 14 September in Vietnam**. Beta and Son were syncing at the
time of inspection.

Read-only inspection of Alpha's telemetry outbox over SSH confirmed:

- Event `76721` is `job.started` at `2026-09-13T22:20:36.198728939Z`.
- Its authorization is the same as the earlier claimed session above.
- Earlier jobs for that claim have moved to a reservation.
- The daemon continued producing events: sequence `85784` was present at
  `2026-09-14T10:37:32Z`.

The claimed aggregate UPDATE computes a new `starts_at_ms` from its current
jobs, but computes `expires_at_ms` with the old row's `starts_at_ms` in the
same SQL statement:

```sql
starts_at_ms = COALESCE((SELECT MIN(started_at_ms) FROM jobs WHERE session_id=?), starts_at_ms),
expires_at_ms = MAX(starts_at_ms+1,
  COALESCE((SELECT MAX(COALESCE(finished_at_ms,root_exited_at_ms,updated_at_ms,started_at_ms))
            FROM jobs WHERE session_id=?), expires_at_ms))
```

If the only current job is the newly started job, its start and update times
are equal. The new start becomes that time and the end also becomes that
time, violating `CHECK(expires_at_ms > starts_at_ms)`.

Reproduced locally with the unmodified UPDATE extracted from
`internal/history/ingest.go`, using an in-memory SQLite fixture matching this
transition. Result:

```text
CHECK constraint failed: expires_at_ms > starts_at_ms
```

This error was reproduced locally, not read from production logs. Production
evidence establishes the matching session state, the next event and the
unchanging cursor. `ApplyPageWithOwners` rolls back the page on an event
failure. The collector advances its cursor only after a successful apply,
then retries failures with backoff, repeatedly reaching the same event.

`recordHistoryAttempt` stores only a failure count and retry time in memory;
it does not log the error or persist it to `node_sync_state.sync_error`.
This explains why an empty gateway error log and empty `sync_error` do not
prove that History collection is healthy.

## Remediation required

1. Compute the claimed start/end from the same aggregate and enforce
   `end >= computed_start + 1ms` so valid telemetry cannot violate the
   timestamp constraint.
2. Recompute every affected claimed session through its historical job
   associations when job updates/finishes arrive, including when the current
   primary association is a reservation. Preserve sessions with unfinished
   sibling jobs.
3. Repair existing stale claimed finalization from the stored associations
   and job timestamps after a consistent backup; do not mark every old
   claimed session completed based only on age.
4. Surface collector errors and last successful sync so stalled History is
   distinguishable from a healthy live node snapshot.
5. After deploying a tested fix, verify Alpha advances beyond `76720`,
   ingests current jobs, and drains the backlog. Restarting the same image
   alone does not fix the deterministic SQL failure.

The daemon retains telemetry for approximately 24 hours. Prolonged ingestion
failure can therefore lose replayable events; resetting the cursor to skip
the failing page is not a data-preserving repair.

## Inspection scope

- Used the existing signed-in Chrome UI for History and Schedule.
- Used a temporary Python pod with the state PVC mounted read-only; ran
  SELECT statements only, without reading credential values.
- Read Alpha daemon status and bounded outbox metadata over existing SSH.
- An attempted credential-based node API diagnostic was rejected by
  automatic approval review without a rationale. It was not executed;
  the findings above were established through the other read-only checks.
- No application code, production database, deployment image, replica count,
  daemon configuration or GPU workload was changed. The temporary diagnostic
  pod was removed after inspection.
