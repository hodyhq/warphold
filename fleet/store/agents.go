package store

import (
	"context"
	"database/sql"
	"time"
)

type Agent struct {
	ID, Name, Hostname, OS, Arch, Version, Scope string
	GroupID                                      int64
	BearerHash, SealedBundle                     []byte
	PolicyETag                                   string
	EnrolledAt                                   time.Time
	LastSeenAt, RevokedAt, RetiredAt             *time.Time
}

const agentCols = `id,name,hostname,os,arch,version,scope,group_id,bearer_hash,sealed_bundle,policy_etag,enrolled_at,last_seen_at,revoked_at,retired_at`

func (s *Store) CreateAgent(ctx context.Context, a *Agent) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO agents(`+agentCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.Name, a.Hostname, a.OS, a.Arch, a.Version, a.Scope, a.GroupID, a.BearerHash, a.SealedBundle, a.PolicyETag, ts(a.EnrolledAt), tsp(a.LastSeenAt), tsp(a.RevokedAt), tsp(a.RetiredAt))
	return err
}

func scanAgent(row interface{ Scan(...any) error }) (*Agent, error) {
	var a Agent
	var enrolled string
	var seen, revoked, retired sql.NullString
	if err := row.Scan(&a.ID, &a.Name, &a.Hostname, &a.OS, &a.Arch, &a.Version, &a.Scope, &a.GroupID, &a.BearerHash, &a.SealedBundle, &a.PolicyETag, &enrolled, &seen, &revoked, &retired); err != nil {
		return nil, notFound(err)
	}
	a.EnrolledAt = parseTS(enrolled)
	a.LastSeenAt = parseTSP(seen)
	a.RevokedAt = parseTSP(revoked)
	a.RetiredAt = parseTSP(retired)
	return &a, nil
}

func (s *Store) Agent(ctx context.Context, id string) (*Agent, error) {
	return scanAgent(s.db.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE id=?`, id))
}

func (s *Store) AgentByBearerHash(ctx context.Context, h []byte) (*Agent, error) {
	return scanAgent(s.db.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE bearer_hash=?`, h))
}

func (s *Store) Agents(ctx context.Context) ([]Agent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+agentCols+` FROM agents ORDER BY enrolled_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

func (s *Store) TouchAgent(ctx context.Context, id string, at time.Time, version, etag string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agents SET last_seen_at=?,version=?,policy_etag=? WHERE id=?`, ts(at), version, etag, id)
	return err
}

func (s *Store) RevokeAgent(ctx context.Context, id string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agents SET revoked_at=? WHERE id=?`, ts(at), id)
	return err
}

// SetAgentBundle replaces an agent's sealed bundle. Enrollment inserts the
// agent row before it provisions -- device_keys.agent_id references it -- and
// fills the bundle in once provisioning has produced one.
func (s *Store) SetAgentBundle(ctx context.Context, id string, sealed []byte) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agents SET sealed_bundle=? WHERE id=?`, sealed, id)
	return err
}

// DeleteAgent removes an agent and its gateway keys. It exists for one caller:
// unwinding an enrollment that failed after the agent row was inserted. A
// device that finished enrolling is revoked, never deleted.
func (s *Store) DeleteAgent(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM device_keys WHERE agent_id=?`, id); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM agents WHERE id=?`, id)

	return err
}

// RetireAgent records that the reap job has removed a revoked agent's
// repository. The row stays: it is the fleet's history, and repo_stats and
// reports reference it. It only ever moves from unset to set, so a second reap
// of the same agent cannot rewrite the date the data actually went.
func (s *Store) RetireAgent(ctx context.Context, id string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agents SET retired_at=? WHERE id=? AND retired_at IS NULL`, ts(at), id)
	return err
}
