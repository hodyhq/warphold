package store

import (
	"context"
	"database/sql"
	"time"
)

// RepoStat is one agent's repository size row (spec §5, repo_stats). The stats
// job fills the first three counters; the mirror job fills the last two.
type RepoStat struct {
	AgentID       string
	CollectedAt   time.Time
	LogicalBytes  int64
	StoredBytes   int64
	BlobCount     int64
	MirroredAt    *time.Time
	MirroredBytes int64
}

const repoStatCols = `agent_id,collected_at,logical_bytes,stored_bytes,blob_count,mirrored_at,mirrored_bytes`

// RepoStat returns an agent's stats row, or ErrNotFound.
func (s *Store) RepoStat(ctx context.Context, agentID string) (*RepoStat, error) {
	var r RepoStat
	var collected string
	var mirrored sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT `+repoStatCols+` FROM repo_stats WHERE agent_id=?`, agentID).
		Scan(&r.AgentID, &collected, &r.LogicalBytes, &r.StoredBytes, &r.BlobCount, &mirrored, &r.MirroredBytes)
	if err != nil {
		return nil, notFound(err)
	}
	r.CollectedAt, r.MirroredAt = parseTS(collected), parseTSP(mirrored)
	return &r, nil
}

// SetMirrored records an agent's offsite progress, leaving the size counters
// alone: the mirror job and the stats job own different halves of the row.
func (s *Store) SetMirrored(ctx context.Context, agentID string, at time.Time, bytes int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO repo_stats(agent_id,collected_at,mirrored_at,mirrored_bytes) VALUES(?,?,?,?)
		ON CONFLICT(agent_id) DO UPDATE SET mirrored_at=excluded.mirrored_at, mirrored_bytes=excluded.mirrored_bytes`,
		agentID, ts(at), ts(at), bytes)
	return err
}
