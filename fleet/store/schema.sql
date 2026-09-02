CREATE TABLE IF NOT EXISTS admins (
  id INTEGER PRIMARY KEY, email TEXT NOT NULL UNIQUE, pw_hash TEXT NOT NULL,
  role TEXT NOT NULL DEFAULT 'owner', created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS targets (
  id INTEGER PRIMARY KEY, name TEXT NOT NULL, kind TEXT NOT NULL, bucket TEXT NOT NULL DEFAULT '',
  region TEXT NOT NULL DEFAULT '', path TEXT NOT NULL DEFAULT '', sealed_admin_key BLOB,
  object_lock_verified_at TEXT, created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS policy_templates (
  id INTEGER PRIMARY KEY, name TEXT NOT NULL, sources TEXT NOT NULL, policy_json TEXT NOT NULL, created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS groups (
  id INTEGER PRIMARY KEY, name TEXT NOT NULL, target_id INTEGER NOT NULL REFERENCES targets(id),
  template_id INTEGER NOT NULL REFERENCES policy_templates(id), created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS agents (
  id TEXT PRIMARY KEY, name TEXT NOT NULL, hostname TEXT NOT NULL, os TEXT NOT NULL, arch TEXT NOT NULL,
  version TEXT NOT NULL DEFAULT '', scope TEXT NOT NULL, group_id INTEGER NOT NULL REFERENCES groups(id),
  bearer_hash BLOB NOT NULL UNIQUE, sealed_bundle BLOB NOT NULL, policy_etag TEXT NOT NULL DEFAULT '',
  enrolled_at TEXT NOT NULL, last_seen_at TEXT, revoked_at TEXT);
CREATE TABLE IF NOT EXISTS enrollment_tokens (
  id INTEGER PRIMARY KEY, token_hash BLOB NOT NULL UNIQUE, group_id INTEGER NOT NULL REFERENCES groups(id),
  expires_at TEXT NOT NULL, max_uses INTEGER NOT NULL DEFAULT 1, uses INTEGER NOT NULL DEFAULT 0,
  revoked_at TEXT, created_by INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS reports (
  id INTEGER PRIMARY KEY, agent_id TEXT NOT NULL REFERENCES agents(id), task_id TEXT NOT NULL, kind TEXT NOT NULL,
  source TEXT NOT NULL DEFAULT '', started_at TEXT NOT NULL, finished_at TEXT NOT NULL, status TEXT NOT NULL,
  bytes INTEGER NOT NULL DEFAULT 0, files INTEGER NOT NULL DEFAULT 0, snapshot_id TEXT NOT NULL DEFAULT '',
  stderr TEXT NOT NULL DEFAULT '', UNIQUE(agent_id, task_id));
CREATE INDEX IF NOT EXISTS reports_agent_finished ON reports(agent_id, finished_at DESC);
CREATE TABLE IF NOT EXISTS commands (
  id INTEGER PRIMARY KEY, agent_id TEXT NOT NULL REFERENCES agents(id), kind TEXT NOT NULL,
  source TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, acked_at TEXT);
CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS sessions (
  id INTEGER PRIMARY KEY, token_hash BLOB NOT NULL UNIQUE,
  admin_id INTEGER NOT NULL REFERENCES admins(id) ON DELETE CASCADE,
  created_at TEXT NOT NULL, expires_at TEXT NOT NULL, revoked_at TEXT);
CREATE INDEX IF NOT EXISTS sessions_admin ON sessions(admin_id);
