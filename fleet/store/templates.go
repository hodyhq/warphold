package store

import (
	"context"
	"encoding/json"
	"time"
)

type Template struct {
	ID         int64
	Name       string
	Sources    []string
	PolicyJSON json.RawMessage
	CreatedAt  time.Time
}

const templateCols = `id,name,sources,policy_json,created_at`

func (s *Store) CreateTemplate(ctx context.Context, t *Template) (int64, error) {
	src, _ := json.Marshal(t.Sources)
	return s.exec(ctx, `INSERT INTO policy_templates(name,sources,policy_json,created_at) VALUES(?,?,?,?)`, t.Name, string(src), string(t.PolicyJSON), ts(time.Now()))
}

func (s *Store) UpdateTemplate(ctx context.Context, t *Template) error {
	src, _ := json.Marshal(t.Sources)
	_, err := s.db.ExecContext(ctx, `UPDATE policy_templates SET name=?,sources=?,policy_json=? WHERE id=?`, t.Name, string(src), string(t.PolicyJSON), t.ID)
	return err
}

func scanTemplate(row interface{ Scan(...any) error }) (*Template, error) {
	var t Template
	var src, pol, c string
	if err := row.Scan(&t.ID, &t.Name, &src, &pol, &c); err != nil {
		return nil, notFound(err)
	}
	_ = json.Unmarshal([]byte(src), &t.Sources)
	t.PolicyJSON = json.RawMessage(pol)
	t.CreatedAt = parseTS(c)
	return &t, nil
}

func (s *Store) Template(ctx context.Context, id int64) (*Template, error) {
	return scanTemplate(s.db.QueryRowContext(ctx, `SELECT `+templateCols+` FROM policy_templates WHERE id=?`, id))
}

func (s *Store) Templates(ctx context.Context) ([]Template, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+templateCols+` FROM policy_templates ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Template
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}
