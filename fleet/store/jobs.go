package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
	"unicode/utf8"
)

// Job is one scheduled unit of fleet work (spec §10). Kind is one of
// verify|test-restore|maintenance|mirror|stats|digest|reap; Status is one of
// pending|running|ok|error|skipped.
type Job struct {
	ID           int64
	Kind         string
	AgentID      string // empty for a fleet-wide job (mirror, digest)
	ScheduledFor time.Time
	StartedAt    *time.Time
	FinishedAt   *time.Time
	Status       string
	Detail       string
}

const jobCols = `id,kind,agent_id,scheduled_for,started_at,finished_at,status,detail`

// maxDetail bounds what a runner can write into a row: a job that fails with a
// megabyte of provider output must not bloat the DB or the jobs API.
const maxDetail = 4000

func scanJob(row interface{ Scan(...any) error }) (*Job, error) {
	var j Job
	var agent, started, finished sql.NullString
	var scheduled string
	if err := row.Scan(&j.ID, &j.Kind, &agent, &scheduled, &started, &finished, &j.Status, &j.Detail); err != nil {
		return nil, notFound(err)
	}
	j.AgentID, j.ScheduledFor = agent.String, parseTS(scheduled)
	j.StartedAt, j.FinishedAt = parseTSP(started), parseTSP(finished)
	return &j, nil
}

// nullAgent maps the empty agent id to NULL: jobs.agent_id has a foreign key to
// agents(id), which "" would violate.
func nullAgent(id string) any {
	if id == "" {
		return nil
	}
	return id
}

// EnqueueJob inserts a job and fills in j.ID. An empty Status is "pending".
func (s *Store) EnqueueJob(ctx context.Context, j *Job) (int64, error) {
	if j.Kind == "" {
		return 0, errors.New("job needs a kind")
	}
	status := j.Status
	if status == "" {
		status = "pending"
	}
	id, err := s.exec(ctx, `INSERT INTO jobs(kind,agent_id,scheduled_for,started_at,finished_at,status,detail) VALUES(?,?,?,?,?,?,?)`,
		j.Kind, nullAgent(j.AgentID), ts(j.ScheduledFor), tsp(j.StartedAt), tsp(j.FinishedAt), status, truncate(j.Detail, maxDetail))
	if err != nil {
		return 0, err
	}
	j.ID = id
	return id, nil
}

// ClaimDueJob marks the oldest due pending job 'running' and returns it, or
// ErrNotFound when nothing is due. The UPDATE ... RETURNING is one statement, so
// two claimers racing for the same row produce exactly one winner.
func (s *Store) ClaimDueJob(ctx context.Context, now time.Time) (*Job, error) {
	// The row is picked in a subselect because SQLite only supports LIMIT on an
	// UPDATE when it is built with SQLITE_ENABLE_UPDATE_DELETE_LIMIT.
	return scanJob(s.db.QueryRowContext(ctx, `UPDATE jobs SET status='running', started_at=?
		WHERE id = (SELECT id FROM jobs WHERE status='pending' AND scheduled_for<=? ORDER BY scheduled_for, id LIMIT 1)
		RETURNING `+jobCols, ts(now), ts(now)))
}

// FinishJob records the outcome of a run.
func (s *Store) FinishJob(ctx context.Context, id int64, at time.Time, status, detail string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET status=?, finished_at=?, detail=? WHERE id=?`,
		status, ts(at), truncate(detail, maxDetail), id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// RequeueStaleJobs returns jobs of one kind that have been 'running' since
// before cutoff to 'pending'. A crash leaves a claim behind; nothing else does,
// because the scheduler runs one job at a time in one process.
func (s *Store) RequeueStaleJobs(ctx context.Context, kind string, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET status='pending', started_at=NULL
		WHERE status='running' AND kind=? AND (started_at IS NULL OR started_at<?)`, kind, ts(cutoff))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// JobsForAgent returns an agent's most recent jobs, newest first.
func (s *Store) JobsForAgent(ctx context.Context, agentID string, limit int) ([]Job, error) {
	return s.jobs(ctx, `WHERE agent_id=? ORDER BY id DESC LIMIT ?`, agentID, jobLimit(limit))
}

// RecentJobs returns the most recent jobs of one kind, newest first; an empty
// kind means every kind.
func (s *Store) RecentJobs(ctx context.Context, kind string, limit int) ([]Job, error) {
	if kind == "" {
		return s.jobs(ctx, `ORDER BY id DESC LIMIT ?`, jobLimit(limit))
	}
	return s.jobs(ctx, `WHERE kind=? ORDER BY id DESC LIMIT ?`, kind, jobLimit(limit))
}

func jobLimit(limit int) int {
	if limit <= 0 {
		return 50
	}
	return limit
}

func (s *Store) jobs(ctx context.Context, where string, args ...any) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+jobCols+` FROM jobs `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Back up to a rune boundary: a detail string is provider or panic text and
	// a half-written rune would be invalid UTF-8 in the API's JSON.
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}
