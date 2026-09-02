package store

import (
	"context"
	"database/sql"
	"time"
)

type Token struct {
	ID            int64
	Hash          []byte
	GroupID       int64
	ExpiresAt     time.Time
	MaxUses, Uses int
	RevokedAt     *time.Time
	CreatedBy     int64
	CreatedAt     time.Time
}

const tokenCols = `id,token_hash,group_id,expires_at,max_uses,uses,revoked_at,created_by,created_at`

func (s *Store) CreateToken(ctx context.Context, t *Token) (int64, error) {
	return s.exec(ctx, `INSERT INTO enrollment_tokens(token_hash,group_id,expires_at,max_uses,uses,revoked_at,created_by,created_at) VALUES(?,?,?,?,0,NULL,?,?)`,
		t.Hash, t.GroupID, ts(t.ExpiresAt), t.MaxUses, t.CreatedBy, ts(t.CreatedAt))
}

func scanToken(row interface{ Scan(...any) error }) (*Token, error) {
	var t Token
	var exp, created string
	var rev sql.NullString
	if err := row.Scan(&t.ID, &t.Hash, &t.GroupID, &exp, &t.MaxUses, &t.Uses, &rev, &t.CreatedBy, &created); err != nil {
		return nil, notFound(err)
	}
	t.ExpiresAt = parseTS(exp)
	t.CreatedAt = parseTS(created)
	t.RevokedAt = parseTSP(rev)
	return &t, nil
}

func (s *Store) TokenByHash(ctx context.Context, h []byte) (*Token, error) {
	return scanToken(s.db.QueryRowContext(ctx, `SELECT `+tokenCols+` FROM enrollment_tokens WHERE token_hash=?`, h))
}

// ConsumeToken counts one use of a token, but only if it is still usable, in
// a single conditional UPDATE so concurrent enrollments cannot both pass the
// check. It reports whether the use was counted. expires_at is RFC3339 UTC
// text, which sorts lexicographically, so the SQL comparison against ts(now)
// matches the Go one.
func (s *Store) ConsumeToken(ctx context.Context, id int64, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE enrollment_tokens SET uses=uses+1 WHERE id=? AND revoked_at IS NULL AND expires_at>? AND (max_uses=0 OR uses<max_uses)`, id, ts(now))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *Store) RevokeToken(ctx context.Context, id int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE enrollment_tokens SET revoked_at=? WHERE id=?`, ts(at), id)
	return err
}

func (s *Store) TokensForGroup(ctx context.Context, groupID int64) ([]Token, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+tokenCols+` FROM enrollment_tokens WHERE group_id=? ORDER BY id DESC`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Token
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}
