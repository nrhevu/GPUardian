package history

const migrationV1 = `
CREATE TABLE IF NOT EXISTS schema_migrations (
  version INTEGER PRIMARY KEY,
  checksum TEXT NOT NULL,
  applied_at_ms INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS nodes (
  node_id TEXT PRIMARY KEY,
  hostname TEXT NOT NULL DEFAULT '',
  last_server_id TEXT NOT NULL,
  first_seen_at_ms INTEGER NOT NULL,
  last_seen_at_ms INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS node_sync_state (
  server_id TEXT PRIMARY KEY,
  node_id TEXT NOT NULL REFERENCES nodes(node_id),
  stream_id TEXT NOT NULL,
  cursor TEXT NOT NULL DEFAULT '',
  last_seq INTEGER NOT NULL DEFAULT 0,
  last_sync_at_ms INTEGER,
  sync_error TEXT NOT NULL DEFAULT '',
  gap_at_ms INTEGER
);
CREATE TABLE IF NOT EXISTS ingested_event_ids (
  node_id TEXT NOT NULL,
  stream_id TEXT NOT NULL,
  seq INTEGER NOT NULL,
  ingested_at_ms INTEGER NOT NULL,
  PRIMARY KEY(node_id, stream_id, seq)
);
CREATE TABLE IF NOT EXISTS reservation_sessions (
  session_id TEXT PRIMARY KEY,
  node_id TEXT NOT NULL REFERENCES nodes(node_id),
  server_id TEXT NOT NULL,
  server_name TEXT NOT NULL,
  group_id TEXT NOT NULL,
  owner_username TEXT NOT NULL COLLATE NOCASE,
  owner_editable INTEGER NOT NULL DEFAULT 0,
  purpose TEXT NOT NULL DEFAULT '',
  source TEXT NOT NULL CHECK(source IN ('web','cli')),
  created_at_ms INTEGER NOT NULL,
  starts_at_ms INTEGER NOT NULL,
  expires_at_ms INTEGER NOT NULL,
  revoked_at_ms INTEGER,
  finalized_at_ms INTEGER,
  history_quality TEXT NOT NULL DEFAULT 'complete' CHECK(history_quality IN ('complete','partial')),
  provisioning INTEGER NOT NULL DEFAULT 0,
  updated_at_ms INTEGER NOT NULL,
  UNIQUE(node_id, group_id),
  CHECK(expires_at_ms > starts_at_ms)
);
CREATE INDEX IF NOT EXISTS sessions_start_idx ON reservation_sessions(starts_at_ms DESC, session_id DESC);
CREATE INDEX IF NOT EXISTS sessions_owner_idx ON reservation_sessions(owner_username, starts_at_ms DESC);
CREATE INDEX IF NOT EXISTS sessions_server_idx ON reservation_sessions(server_id, starts_at_ms DESC);
CREATE TABLE IF NOT EXISTS session_gpus (
  session_id TEXT NOT NULL REFERENCES reservation_sessions(session_id) ON DELETE CASCADE,
  gpu INTEGER NOT NULL,
  reservation_id TEXT NOT NULL,
  PRIMARY KEY(session_id, gpu),
  UNIQUE(session_id, reservation_id)
);
CREATE TABLE IF NOT EXISTS authorization_scopes (
  node_id TEXT NOT NULL REFERENCES nodes(node_id),
  authorization_id TEXT NOT NULL,
  session_id TEXT REFERENCES reservation_sessions(session_id),
  mode TEXT NOT NULL,
  holder TEXT NOT NULL,
  selector TEXT NOT NULL DEFAULT '',
  command_json TEXT NOT NULL DEFAULT '[]',
  created_at_ms INTEGER NOT NULL,
  expires_at_ms INTEGER,
  ended_at_ms INTEGER,
  end_reason TEXT NOT NULL DEFAULT '',
  PRIMARY KEY(node_id, authorization_id)
);
CREATE TABLE IF NOT EXISTS jobs (
  node_id TEXT NOT NULL REFERENCES nodes(node_id),
  job_id TEXT NOT NULL,
  session_id TEXT NOT NULL REFERENCES reservation_sessions(session_id) ON DELETE CASCADE,
  authorization_id TEXT NOT NULL,
  source TEXT NOT NULL CHECK(source IN ('gpuardian_run','authorized_process')),
  mode TEXT NOT NULL,
  holder TEXT NOT NULL,
  command_json TEXT NOT NULL DEFAULT '[]',
  started_at_ms INTEGER,
  root_exited_at_ms INTEGER,
  finished_at_ms INTEGER,
  start_precision TEXT NOT NULL DEFAULT '',
  finish_precision TEXT NOT NULL DEFAULT '',
  exit_code INTEGER,
  end_reason TEXT NOT NULL DEFAULT '',
  updated_at_ms INTEGER NOT NULL,
  PRIMARY KEY(node_id, job_id)
);
CREATE INDEX IF NOT EXISTS jobs_session_idx ON jobs(session_id, started_at_ms, job_id);
CREATE TABLE IF NOT EXISTS job_gpus (
  node_id TEXT NOT NULL,
  job_id TEXT NOT NULL,
  gpu INTEGER NOT NULL,
  PRIMARY KEY(node_id, job_id, gpu),
  FOREIGN KEY(node_id, job_id) REFERENCES jobs(node_id, job_id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS gpu_minute_rollups (
  session_id TEXT NOT NULL REFERENCES reservation_sessions(session_id) ON DELETE CASCADE,
  gpu INTEGER NOT NULL,
  minute_ms INTEGER NOT NULL,
  observed_ms INTEGER NOT NULL DEFAULT 0,
  busy_ms INTEGER NOT NULL DEFAULT 0,
  utilization_integral REAL NOT NULL DEFAULT 0,
  memory_integral REAL NOT NULL DEFAULT 0,
  memory_observed_ms INTEGER NOT NULL DEFAULT 0,
  peak_memory_bytes INTEGER,
  valid_samples INTEGER NOT NULL DEFAULT 0,
  missing_samples INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(session_id, gpu, minute_ms)
);
CREATE TABLE IF NOT EXISTS session_gpu_summaries (
  session_id TEXT NOT NULL REFERENCES reservation_sessions(session_id) ON DELETE CASCADE,
  gpu INTEGER NOT NULL,
  observed_ms INTEGER NOT NULL DEFAULT 0,
  busy_ms INTEGER NOT NULL DEFAULT 0,
  utilization_integral REAL NOT NULL DEFAULT 0,
  memory_integral REAL NOT NULL DEFAULT 0,
  memory_observed_ms INTEGER NOT NULL DEFAULT 0,
  peak_memory_bytes INTEGER,
  valid_samples INTEGER NOT NULL DEFAULT 0,
  missing_samples INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(session_id, gpu),
  FOREIGN KEY(session_id, gpu) REFERENCES session_gpus(session_id, gpu) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS session_results (
  session_id TEXT PRIMARY KEY REFERENCES reservation_sessions(session_id) ON DELETE CASCADE,
  outcome TEXT,
  note TEXT NOT NULL DEFAULT '',
  version INTEGER NOT NULL DEFAULT 0,
  updated_at_ms INTEGER,
  CHECK(outcome IS NULL OR outcome IN ('success','partial','failed','aborted'))
);
CREATE TABLE IF NOT EXISTS session_artifacts (
  session_id TEXT NOT NULL REFERENCES session_results(session_id) ON DELETE CASCADE,
  position INTEGER NOT NULL,
  label TEXT NOT NULL,
  url TEXT NOT NULL,
  PRIMARY KEY(session_id, position)
);
`

const migrationV2 = `
CREATE TABLE IF NOT EXISTS authorization_sessions (
  node_id TEXT NOT NULL,
  authorization_id TEXT NOT NULL,
  session_id TEXT NOT NULL REFERENCES reservation_sessions(session_id) ON DELETE CASCADE,
  PRIMARY KEY(node_id, authorization_id, session_id)
);
CREATE INDEX IF NOT EXISTS authorization_sessions_session_idx ON authorization_sessions(session_id, authorization_id);
INSERT OR IGNORE INTO authorization_sessions(node_id,authorization_id,session_id)
  SELECT node_id,authorization_id,session_id FROM authorization_scopes WHERE session_id IS NOT NULL;
CREATE TABLE IF NOT EXISTS job_sessions (
  node_id TEXT NOT NULL,
  job_id TEXT NOT NULL,
  session_id TEXT NOT NULL REFERENCES reservation_sessions(session_id) ON DELETE CASCADE,
  PRIMARY KEY(node_id, job_id, session_id),
  FOREIGN KEY(node_id,job_id) REFERENCES jobs(node_id,job_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS job_sessions_session_idx ON job_sessions(session_id, job_id);
INSERT OR IGNORE INTO job_sessions(node_id,job_id,session_id)
  SELECT node_id,job_id,session_id FROM jobs;
CREATE TABLE IF NOT EXISTS managed_key_sync_state (
  server_id TEXT PRIMARY KEY,
  snapshot_id TEXT NOT NULL DEFAULT '',
  synced_at_ms INTEGER,
  sync_error TEXT NOT NULL DEFAULT ''
);
`

const migrationV3 = `
ALTER TABLE reservation_sessions ADD COLUMN kind TEXT NOT NULL DEFAULT 'reservation'
  CHECK(kind IN ('reservation','claimed_run'));
CREATE INDEX IF NOT EXISTS sessions_kind_idx ON reservation_sessions(kind, starts_at_ms DESC);
`

const migrationV4 = `
CREATE TABLE IF NOT EXISTS node_gpu_minute_rollups (
  node_id TEXT NOT NULL REFERENCES nodes(node_id) ON DELETE CASCADE,
  gpu INTEGER NOT NULL,
  minute_ms INTEGER NOT NULL,
  observed_ms INTEGER NOT NULL DEFAULT 0,
  busy_ms INTEGER NOT NULL DEFAULT 0,
  utilization_integral REAL NOT NULL DEFAULT 0,
  valid_samples INTEGER NOT NULL DEFAULT 0,
  missing_samples INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(node_id, gpu, minute_ms)
);
INSERT OR IGNORE INTO node_gpu_minute_rollups(
  node_id,gpu,minute_ms,observed_ms,busy_ms,utilization_integral,valid_samples,missing_samples
)
SELECT r.node_id,g.gpu,g.minute_ms,SUM(g.observed_ms),SUM(g.busy_ms),SUM(g.utilization_integral),
  SUM(g.valid_samples),SUM(g.missing_samples)
FROM gpu_minute_rollups g
JOIN reservation_sessions r ON r.session_id=g.session_id
WHERE r.kind='reservation'
GROUP BY r.node_id,g.gpu,g.minute_ms;
CREATE INDEX IF NOT EXISTS node_gpu_rollups_time_idx ON node_gpu_minute_rollups(minute_ms);
`

const migrationV5 = `
ALTER TABLE jobs ADD COLUMN runtime_uid INTEGER;
ALTER TABLE jobs ADD COLUMN runtime_username TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN runtime_container_id TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN runtime_docker_container_name TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN runtime_kubernetes_namespace TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN runtime_kubernetes_pod_name TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN runtime_kubernetes_container_name TEXT NOT NULL DEFAULT '';
`

const migrationV6 = `
CREATE INDEX IF NOT EXISTS sessions_purpose_idx ON reservation_sessions(purpose COLLATE NOCASE);
CREATE VIRTUAL TABLE IF NOT EXISTS session_purpose_fts USING fts5(
  session_id UNINDEXED,
  purpose,
  tokenize='trigram'
);
DELETE FROM session_purpose_fts;
INSERT INTO session_purpose_fts(session_id,purpose)
  SELECT session_id,purpose FROM reservation_sessions;
CREATE TRIGGER IF NOT EXISTS session_purpose_fts_insert AFTER INSERT ON reservation_sessions BEGIN
  INSERT INTO session_purpose_fts(session_id,purpose) VALUES(new.session_id,new.purpose);
END;
CREATE TRIGGER IF NOT EXISTS session_purpose_fts_update AFTER UPDATE OF session_id,purpose ON reservation_sessions BEGIN
  DELETE FROM session_purpose_fts WHERE session_id=old.session_id;
  INSERT INTO session_purpose_fts(session_id,purpose) VALUES(new.session_id,new.purpose);
END;
CREATE TRIGGER IF NOT EXISTS session_purpose_fts_delete AFTER DELETE ON reservation_sessions BEGIN
  DELETE FROM session_purpose_fts WHERE session_id=old.session_id;
END;
`

const migrationV7 = `
CREATE TEMP TABLE claimed_session_merge (
  old_session_id TEXT PRIMARY KEY,
  canonical_session_id TEXT NOT NULL,
  canonical_group_id TEXT NOT NULL
);
INSERT INTO claimed_session_merge(old_session_id,canonical_session_id,canonical_group_id)
SELECT r.session_id,
  (SELECT MIN(r2.session_id)
   FROM reservation_sessions r2
   JOIN jobs j2 ON j2.session_id=r2.session_id
   WHERE r2.kind='claimed_run' AND r2.node_id=r.node_id AND j2.authorization_id=j.authorization_id),
  'claimed-auth:' || j.authorization_id
FROM reservation_sessions r
JOIN jobs j ON j.session_id=r.session_id
WHERE r.kind='claimed_run' AND j.authorization_id<>''
GROUP BY r.session_id
HAVING COUNT(DISTINCT j.authorization_id)=1;

CREATE TEMP TABLE claimed_gpu_merge AS
SELECT m.canonical_session_id,g.gpu
FROM claimed_session_merge m
JOIN session_gpus g ON g.session_id=m.old_session_id
GROUP BY m.canonical_session_id,g.gpu;
CREATE TEMP TABLE claimed_summary_merge AS
SELECT m.canonical_session_id,s.gpu,SUM(s.observed_ms) AS observed_ms,SUM(s.busy_ms) AS busy_ms,
  SUM(s.utilization_integral) AS utilization_integral,SUM(s.memory_integral) AS memory_integral,
  SUM(s.memory_observed_ms) AS memory_observed_ms,MAX(s.peak_memory_bytes) AS peak_memory_bytes,
  SUM(s.valid_samples) AS valid_samples,SUM(s.missing_samples) AS missing_samples
FROM claimed_session_merge m
JOIN session_gpu_summaries s ON s.session_id=m.old_session_id
GROUP BY m.canonical_session_id,s.gpu;
CREATE TEMP TABLE claimed_rollup_merge AS
SELECT m.canonical_session_id,g.gpu,g.minute_ms,SUM(g.observed_ms) AS observed_ms,SUM(g.busy_ms) AS busy_ms,
  SUM(g.utilization_integral) AS utilization_integral,SUM(g.memory_integral) AS memory_integral,
  SUM(g.memory_observed_ms) AS memory_observed_ms,MAX(g.peak_memory_bytes) AS peak_memory_bytes,
  SUM(g.valid_samples) AS valid_samples,SUM(g.missing_samples) AS missing_samples
FROM claimed_session_merge m
JOIN gpu_minute_rollups g ON g.session_id=m.old_session_id
GROUP BY m.canonical_session_id,g.gpu,g.minute_ms;

INSERT OR IGNORE INTO job_sessions(node_id,job_id,session_id)
SELECT j.node_id,j.job_id,m.canonical_session_id
FROM jobs j JOIN claimed_session_merge m ON m.old_session_id=j.session_id;
INSERT OR IGNORE INTO authorization_sessions(node_id,authorization_id,session_id)
SELECT a.node_id,a.authorization_id,m.canonical_session_id
FROM authorization_sessions a JOIN claimed_session_merge m ON m.old_session_id=a.session_id;
UPDATE authorization_scopes
SET session_id=(SELECT m.canonical_session_id FROM claimed_session_merge m WHERE m.old_session_id=authorization_scopes.session_id)
WHERE session_id IN (SELECT old_session_id FROM claimed_session_merge);
UPDATE jobs
SET session_id=(SELECT m.canonical_session_id FROM claimed_session_merge m WHERE m.old_session_id=jobs.session_id)
WHERE session_id IN (SELECT old_session_id FROM claimed_session_merge);

UPDATE reservation_sessions AS canonical
SET created_at_ms=(SELECT MIN(r.created_at_ms) FROM reservation_sessions r JOIN claimed_session_merge m ON m.old_session_id=r.session_id WHERE m.canonical_session_id=canonical.session_id),
  starts_at_ms=(SELECT MIN(r.starts_at_ms) FROM reservation_sessions r JOIN claimed_session_merge m ON m.old_session_id=r.session_id WHERE m.canonical_session_id=canonical.session_id),
  expires_at_ms=(SELECT MAX(r.expires_at_ms) FROM reservation_sessions r JOIN claimed_session_merge m ON m.old_session_id=r.session_id WHERE m.canonical_session_id=canonical.session_id),
  finalized_at_ms=CASE
    WHEN EXISTS(SELECT 1 FROM reservation_sessions r JOIN claimed_session_merge m ON m.old_session_id=r.session_id WHERE m.canonical_session_id=canonical.session_id AND r.finalized_at_ms IS NULL) THEN NULL
    ELSE (SELECT MAX(r.finalized_at_ms) FROM reservation_sessions r JOIN claimed_session_merge m ON m.old_session_id=r.session_id WHERE m.canonical_session_id=canonical.session_id)
  END,
  history_quality=CASE
    WHEN EXISTS(SELECT 1 FROM reservation_sessions r JOIN claimed_session_merge m ON m.old_session_id=r.session_id WHERE m.canonical_session_id=canonical.session_id AND r.history_quality='partial') THEN 'partial'
    ELSE 'complete'
  END,
  updated_at_ms=(SELECT MAX(r.updated_at_ms) FROM reservation_sessions r JOIN claimed_session_merge m ON m.old_session_id=r.session_id WHERE m.canonical_session_id=canonical.session_id),
  group_id=(SELECT m.canonical_group_id FROM claimed_session_merge m WHERE m.canonical_session_id=canonical.session_id LIMIT 1)
WHERE canonical.session_id IN (SELECT canonical_session_id FROM claimed_session_merge);

UPDATE reservation_sessions
SET provisioning=1
WHERE session_id IN (SELECT old_session_id FROM claimed_session_merge WHERE old_session_id<>canonical_session_id);

DELETE FROM session_gpus WHERE session_id IN (SELECT canonical_session_id FROM claimed_session_merge);
INSERT INTO session_gpus(session_id,gpu,reservation_id)
SELECT canonical_session_id,gpu,'claimed-merged:' || gpu FROM claimed_gpu_merge;
INSERT INTO session_gpu_summaries(session_id,gpu,observed_ms,busy_ms,utilization_integral,memory_integral,
  memory_observed_ms,peak_memory_bytes,valid_samples,missing_samples)
SELECT canonical_session_id,gpu,observed_ms,busy_ms,utilization_integral,memory_integral,
  memory_observed_ms,peak_memory_bytes,valid_samples,missing_samples FROM claimed_summary_merge;
DELETE FROM gpu_minute_rollups WHERE session_id IN (SELECT canonical_session_id FROM claimed_session_merge);
INSERT INTO gpu_minute_rollups(session_id,gpu,minute_ms,observed_ms,busy_ms,utilization_integral,memory_integral,
  memory_observed_ms,peak_memory_bytes,valid_samples,missing_samples)
SELECT canonical_session_id,gpu,minute_ms,observed_ms,busy_ms,utilization_integral,memory_integral,
  memory_observed_ms,peak_memory_bytes,valid_samples,missing_samples FROM claimed_rollup_merge;

DROP TABLE claimed_rollup_merge;
DROP TABLE claimed_summary_merge;
DROP TABLE claimed_gpu_merge;
DROP TABLE claimed_session_merge;
`
