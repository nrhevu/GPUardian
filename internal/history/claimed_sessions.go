package history

// jobs.session_id can move to a reservation, while
// job_sessions retains the historical association used by the History reader.
const claimedSessionAggregateStart = `
WITH aggregates AS (
  SELECT r.session_id,
    COALESCE(MIN(j.started_at_ms),r.starts_at_ms) AS starts_ms,
    COALESCE(MAX(COALESCE(j.finished_at_ms,j.root_exited_at_ms,j.updated_at_ms,j.started_at_ms)),r.expires_at_ms) AS ends_ms,
    SUM(j.finished_at_ms IS NULL) AS unfinished,
    MAX(j.finished_at_ms) AS finished_ms,
    MAX(j.updated_at_ms) AS updated_ms
  FROM reservation_sessions r
  JOIN job_sessions js ON js.session_id=r.session_id
  JOIN jobs j ON j.node_id=js.node_id AND j.job_id=js.job_id
  WHERE r.kind='claimed_run' AND r.provisioning=0
`

const claimedSessionAggregateEnd = `
  GROUP BY r.session_id
)
UPDATE reservation_sessions AS r SET
  starts_at_ms=a.starts_ms,
  expires_at_ms=MAX(a.starts_ms+1,a.ends_ms),
  finalized_at_ms=CASE WHEN a.unfinished>0 THEN NULL ELSE a.finished_ms END,
  updated_at_ms=MAX(r.updated_at_ms,a.updated_ms)
FROM aggregates a WHERE r.session_id=a.session_id;
`

const refreshJobClaimedSessionsSQL = claimedSessionAggregateStart + `
  AND r.session_id IN (SELECT session_id FROM job_sessions WHERE node_id=? AND job_id=?)
` + claimedSessionAggregateEnd
