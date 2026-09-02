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
	LastSeenAt, RevokedAt                        *time.Time
}

const agentCols = `id,name,hostname,os,arch,version,scope,group_id,bearer_hash,sealed_bundle,policy_etag,enrolled_at,last_seen_at,revoked_at`

func (s *Store) CreateAgent(ctx context.Context, a *Agent) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO agents(`+agentCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.Name, a.Hostname, a.OS, a.Arch, a.Version, a.Scope, a.GroupID, a.BearerHash, a.SealedBundle, a.PolicyETag, ts(a.EnrolledAt), tsp(a.LastSeenAt), tsp(a.RevokedAt))
	return err
}

func scanAgent(row interface{ Scan(...any) error }) (*Agent, error) {
	var a Agent
	var enrolled string
	var seen, revoked sql.NullString
	if err := row.Scan(&a.ID, &a.Name, &a.Hostname, &a.OS, &a.Arch, &a.Version, &a.Scope, &a.GroupID, &a.BearerHash, &a.SealedBundle, &a.PolicyETag, &enrolled, &seen, &revoked); err != nil {
		return nil, notFound(err)
	}
	a.EnrolledAt = parseTS(enrolled)
	a.LastSeenAt = parseTSP(seen)
	a.RevokedAt = parseTSP(revoked)
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
