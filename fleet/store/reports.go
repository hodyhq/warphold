package store

import (
	"context"
	"errors"
	"time"
)

type Report struct {
	ID                            int64
	AgentID, TaskID, Kind, Source string
	StartedAt, FinishedAt         time.Time
	Status                        string
	Bytes, Files                  int64
	SnapshotID, Stderr            string
}

const reportCols = `id,agent_id,task_id,kind,source,started_at,finished_at,status,bytes,files,snapshot_id,stderr`

// AddReport inserts a report; a duplicate (agent_id, task_id) returns the existing id.
func (s *Store) AddReport(ctx context.Context, r *Report) (int64, error) {
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO reports(agent_id,task_id,kind,source,started_at,finished_at,status,bytes,files,snapshot_id,stderr) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		r.AgentID, r.TaskID, r.Kind, r.Source, ts(r.StartedAt), ts(r.FinishedAt), r.Status, r.Bytes, r.Files, r.SnapshotID, r.Stderr)
	if err != nil {
		return 0, err
	}
	var id int64
	err = s.db.QueryRowContext(ctx, `SELECT id FROM reports WHERE agent_id=? AND task_id=?`, r.AgentID, r.TaskID).Scan(&id)
	return id, err
}

func scanReport(row interface{ Scan(...any) error }) (*Report, error) {
	var r Report
	var st, fi string
	if err := row.Scan(&r.ID, &r.AgentID, &r.TaskID, &r.Kind, &r.Source, &st, &fi, &r.Status, &r.Bytes, &r.Files, &r.SnapshotID, &r.Stderr); err != nil {
		return nil, notFound(err)
	}
	r.StartedAt, r.FinishedAt = parseTS(st), parseTS(fi)
	return &r, nil
}

func (s *Store) ReportsForAgent(ctx context.Context, agentID string, limit int) ([]Report, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+reportCols+` FROM reports WHERE agent_id=? ORDER BY finished_at DESC, id DESC LIMIT ?`, agentID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Report
	for rows.Next() {
		r, err := scanReport(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// LastOKReport returns the newest successful snapshot report for an agent, or
// nil. Only kind='snapshot' counts: health means "this device has a recent
// backup", and the agent reports every command it applies (including a
// pause/resume that runs no backup at all) as kind='command', status='ok'.
func (s *Store) LastOKReport(ctx context.Context, agentID string) (*Report, error) {
	r, err := scanReport(s.db.QueryRowContext(ctx, `SELECT `+reportCols+` FROM reports WHERE agent_id=? AND status='ok' AND kind='snapshot' ORDER BY finished_at DESC, id DESC LIMIT 1`, agentID))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return r, err
}

// LatestReports returns the newest report per agent.
func (s *Store) LatestReports(ctx context.Context) (map[string]Report, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+reportCols+` FROM reports r WHERE id = (SELECT id FROM reports WHERE agent_id=r.agent_id ORDER BY finished_at DESC, id DESC LIMIT 1)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Report{}
	for rows.Next() {
		r, err := scanReport(rows)
		if err != nil {
			return nil, err
		}
		out[r.AgentID] = *r
	}
	return out, rows.Err()
}

// LastOKReports returns the newest successful snapshot finish time per agent,
// for agents that have one. Same kind='snapshot' rule as LastOKReport (a
// pause/resume command reports ok but backs nothing up), batched: the agent
// list renders one health value per row and must not run a query per agent.
func (s *Store) LastOKReports(ctx context.Context) (map[string]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT agent_id,finished_at FROM reports r WHERE id = (SELECT id FROM reports WHERE agent_id=r.agent_id AND status='ok' AND kind='snapshot' ORDER BY finished_at DESC, id DESC LIMIT 1)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var agentID, fi string
		if err := rows.Scan(&agentID, &fi); err != nil {
			return nil, err
		}
		out[agentID] = parseTS(fi)
	}
	return out, rows.Err()
}
