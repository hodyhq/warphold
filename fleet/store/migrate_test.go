package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	_ "modernc.org/sqlite" // pure-Go driver, registers "sqlite"

	"github.com/kopia/kopia/fleet/store"
)

// prePlan3Schema is a frozen copy of schema.sql as it stood before Plan 3, so
// the additive migration is exercised against a real old database rather than
// against the schema it is shipped with.
const prePlan3Schema = `
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
CREATE INDEX IF NOT EXISTS reports_finished ON reports(finished_at);
CREATE TABLE IF NOT EXISTS commands (
  id INTEGER PRIMARY KEY, agent_id TEXT NOT NULL REFERENCES agents(id), kind TEXT NOT NULL,
  source TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, acked_at TEXT);
CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS sessions (
  id INTEGER PRIMARY KEY, token_hash BLOB NOT NULL UNIQUE,
  admin_id INTEGER NOT NULL REFERENCES admins(id) ON DELETE CASCADE,
  created_at TEXT NOT NULL, expires_at TEXT NOT NULL, revoked_at TEXT);
CREATE INDEX IF NOT EXISTS sessions_admin ON sessions(admin_id);
`

// openRaw creates a database with the frozen schema and returns its path.
func openRaw(t *testing.T, ddl string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fleet.db")
	db, err := sql.Open("sqlite", p)
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(ddl)
	require.NoError(t, err)
	return p
}

func columns(t *testing.T, path, table string) map[string]bool {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer db.Close()
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	require.NoError(t, err)
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		out[n] = true
	}
	require.NoError(t, rows.Err())
	return out
}

func TestMigrateAddsColumnsAndKeepsRows(t *testing.T) {
	p := openRaw(t, prePlan3Schema)

	db, err := sql.Open("sqlite", p)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO targets(id,name,kind,bucket,region,path,sealed_admin_key,object_lock_verified_at,created_at)
		VALUES(7,'old','b2','hody-backups','us-west-004','','sealed-bytes','2026-01-02T03:04:05Z','2026-01-01T00:00:00Z')`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	s, err := store.Open(p)
	require.NoError(t, err)

	tgt, err := s.Target(context.Background(), 7)
	require.NoError(t, err)
	require.Equal(t, "old", tgt.Name)
	require.Equal(t, "b2", tgt.Kind)
	require.Equal(t, "hody-backups", tgt.Bucket)
	require.Equal(t, "us-west-004", tgt.Region)
	require.Equal(t, []byte("sealed-bytes"), tgt.SealedAdminKey)
	require.NotNil(t, tgt.ObjectLockVerifiedAt)
	require.Empty(t, tgt.StorageMode, "new column defaults to empty on an existing row")
	require.Nil(t, tgt.SealedMirrorKey)
	require.NoError(t, s.Close())

	tc := columns(t, p, "targets")
	for _, c := range []string{"storage_mode", "mirror_kind", "mirror_bucket", "mirror_region", "sealed_mirror_key", "mirror_lock_verified_at"} {
		require.True(t, tc[c], "targets.%s should have been added", c)
	}
	require.True(t, columns(t, p, "agents")["retired_at"])

	// tables added by schema.sql on the same Open
	for _, table := range []string{"device_keys", "jobs", "kit_acks", "repo_stats"} {
		require.NotEmpty(t, columns(t, p, table), "table %s should exist", table)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	p := openRaw(t, prePlan3Schema)
	for i := range 3 {
		s, err := store.Open(p)
		require.NoError(t, err, "Open #%d", i)
		require.NoError(t, s.Close())
	}
	require.True(t, columns(t, p, "targets")["mirror_bucket"])
}
