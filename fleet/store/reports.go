package store

import (
	"context"
	"errors"
	"time"
)

// Report is one run an agent reported. The JSON tags match the column names,
// which is what the Fleet UI reads. Nothing decodes a Report - agents post
// agent/poll.Report, which carries its own tags - which matters because
// compound PascalCase names such as "AgentID" no longer decode into these
// fields: encoding/json's case-insensitive fallback does not bridge the
// underscore in "agent_id". See TestReportJSONWireShape.
type Report struct {
	ID         int64     `json:"id"`
	AgentID    string    `json:"agent_id"`
	TaskID     string    `json:"task_id"`
	Kind       string    `json:"kind"`
	Source     string    `json:"source"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Status     string    `json:"status"`
	Bytes      int64     `json:"bytes"`
	Files      int64     `json:"files"`
	SnapshotID string    `json:"snapshot_id"`
	Stderr     string    `json:"stderr"`
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

// ReportsSince returns every report that finished at or after since, oldest
// first. The overview endpoint reads one window (30 days) and derives the
// per-day strips, the 24 h buckets and the latest failure from it in Go,
// rather than issuing a query per agent or per day.
//
// Timestamps are stored as fixed-width RFC3339 in UTC (see store.ts), so the
// string comparison below is a chronological one and uses the same ordering
// as every other report query.
func (s *Store) ReportsSince(ctx context.Context, since time.Time) ([]Report, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+reportCols+` FROM reports WHERE finished_at>=? ORDER BY finished_at, id`, ts(since))
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
