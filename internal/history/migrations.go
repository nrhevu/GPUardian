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
