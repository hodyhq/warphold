package store

import (
	"context"
	"database/sql"
	"time"
)

// Session is one signed-in admin browser. The cookie the browser holds is not
// stored: token_hash is the SHA-256 of it, so a stolen database still cannot
// be replayed as a login.
type Session struct {
	ID        int64
	TokenHash []byte
	AdminID   int64
	CreatedAt time.Time
	ExpiresAt time.Time
	RevokedAt *time.Time
}

const sessionCols = `id,token_hash,admin_id,created_at,expires_at,revoked_at`

// CreateSession records a new session and returns its id.
func (s *Store) CreateSession(ctx context.Context, tokenHash []byte, adminID int64, now, expires time.Time) (int64, error) {
	return s.exec(ctx, `INSERT INTO sessions(token_hash,admin_id,created_at,expires_at) VALUES(?,?,?,?)`,
		tokenHash, adminID, ts(now), ts(expires))
}

func scanSession(row interface{ Scan(...any) error }) (*Session, error) {
	var v Session
	var created, expires string
	var revoked sql.NullString
	if err := row.Scan(&v.ID, &v.TokenHash, &v.AdminID, &created, &expires, &revoked); err != nil {
		return nil, notFound(err)
	}
	v.CreatedAt, v.ExpiresAt, v.RevokedAt = parseTS(created), parseTS(expires), parseTSP(revoked)
	return &v, nil
}

// SessionByHash looks a session up by the SHA-256 of its cookie. Revoked and
// expired sessions are returned as they are: the caller decides, so it can
// tell "never existed" from "no longer valid".
func (s *Store) SessionByHash(ctx context.Context, h []byte) (*Session, error) {
	return scanSession(s.db.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM sessions WHERE token_hash=?`, h))
}

// RevokeSession kills one session. Already-revoked rows keep their first
// revocation time, so a replayed logout cannot move it.
func (s *Store) RevokeSession(ctx context.Context, id int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET revoked_at=? WHERE id=? AND revoked_at IS NULL`, ts(at), id)
	return err
}

// RevokeSessionsForAdmin kills every live session of one admin.
func (s *Store) RevokeSessionsForAdmin(ctx context.Context, adminID int64, at time.Time) error {
	return s.RevokeSessionsForAdminExcept(ctx, adminID, 0, at)
}

// RevokeSessionsForAdminExcept kills every live session of one admin but the
// one identified by exceptID (0 to spare none). A password change uses it to
// sign the other browsers out without signing itself out.
func (s *Store) RevokeSessionsForAdminExcept(ctx context.Context, adminID, exceptID int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET revoked_at=? WHERE admin_id=? AND id<>? AND revoked_at IS NULL`, ts(at), adminID, exceptID)
	return err
}
