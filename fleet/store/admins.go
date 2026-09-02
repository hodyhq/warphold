package store

import (
	"context"
	"time"
)

type Admin struct {
	ID        int64
	Email     string
	PWHash    string
	Role      string
	CreatedAt time.Time
}

func (s *Store) CreateAdmin(ctx context.Context, email, pwHash string, at time.Time) (int64, error) {
	return s.exec(ctx, `INSERT INTO admins(email,pw_hash,role,created_at) VALUES(?,?,'owner',?)`, email, pwHash, ts(at))
}

const adminCols = `id,email,pw_hash,role,created_at`

func scanAdmin(row interface{ Scan(...any) error }) (*Admin, error) {
	var a Admin
	var c string
	if err := row.Scan(&a.ID, &a.Email, &a.PWHash, &a.Role, &c); err != nil {
		return nil, notFound(err)
	}
	a.CreatedAt = parseTS(c)
	return &a, nil
}

func (s *Store) AdminByEmail(ctx context.Context, email string) (*Admin, error) {
	return scanAdmin(s.db.QueryRowContext(ctx, `SELECT `+adminCols+` FROM admins WHERE email=?`, email))
}

func (s *Store) AdminByID(ctx context.Context, id int64) (*Admin, error) {
	return scanAdmin(s.db.QueryRowContext(ctx, `SELECT `+adminCols+` FROM admins WHERE id=?`, id))
}

func (s *Store) Admins(ctx context.Context) ([]Admin, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+adminCols+` FROM admins ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Admin
	for rows.Next() {
		a, err := scanAdmin(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}
