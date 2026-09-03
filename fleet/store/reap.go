package store

import (
	"context"
	"time"
)

// ScheduleReap queues the removal of a revoked device's repository, which is
// the second phase of revocation (D6): the gateway key dies immediately, the
// repository is kept until the retention window closes.
//
// The row is written here and consumed by the M5 job runner. This file holds
// only the reap insert on purpose - the general `jobs` accessors are a
// separate concern and live in their own file.
func (s *Store) ScheduleReap(ctx context.Context, agentID string, at time.Time) (int64, error) {
	return s.exec(ctx,
		`INSERT INTO jobs(kind,agent_id,scheduled_for,status) VALUES('reap',?,?,'pending')`,
		agentID, ts(at))
}

// PendingReaps returns the scheduled times of an agent's queued reap jobs,
// oldest first. It is the read side of ScheduleReap and nothing more; the
// general jobs query surface belongs with the rest of the jobs accessors.
func (s *Store) PendingReaps(ctx context.Context, agentID string) ([]time.Time, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT scheduled_for FROM jobs WHERE kind='reap' AND agent_id=? AND status='pending' ORDER BY scheduled_for`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []time.Time
	for rows.Next() {
		var at string
		if err := rows.Scan(&at); err != nil {
			return nil, err
		}
		out = append(out, parseTS(at))
	}
	return out, rows.Err()
}
