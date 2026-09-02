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
}

const tokenCols = `id,token_hash,group_id,expires_at,max_uses,uses,revoked_at,created_by`

func (s *Store) CreateToken(ctx context.Context, t *Token) (int64, error) {
	return s.exec(ctx, `INSERT INTO enrollment_tokens(token_hash,group_id,expires_at,max_uses,uses,revoked_at,created_by,created_at) VALUES(?,?,?,?,0,NULL,?,?)`,
		t.Hash, t.GroupID, ts(t.ExpiresAt), t.MaxUses, t.CreatedBy, ts(time.Now()))
}

func scanToken(row interface{ Scan(...any) error }) (*Token, error) {
	var t Token
	var exp string
	var rev sql.NullString
	if err := row.Scan(&t.ID, &t.Hash, &t.GroupID, &exp, &t.MaxUses, &t.Uses, &rev, &t.CreatedBy); err != nil {
		return nil, notFound(err)
	}
	t.ExpiresAt = parseTS(exp)
	t.RevokedAt = parseTSP(rev)
	return &t, nil
}

func (s *Store) TokenByHash(ctx context.Context, h []byte) (*Token, error) {
	return scanToken(s.db.QueryRowContext(ctx, `SELECT `+tokenCols+` FROM enrollment_tokens WHERE token_hash=?`, h))
}

func (s *Store) IncrementTokenUses(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE enrollment_tokens SET uses=uses+1 WHERE id=?`, id)
	return err
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
