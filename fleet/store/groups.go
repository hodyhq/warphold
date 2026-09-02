package store

import (
	"context"
	"time"
)

type Group struct {
	ID                   int64
	Name                 string
	TargetID, TemplateID int64
	CreatedAt            time.Time
}

func (s *Store) CreateGroup(ctx context.Context, g *Group) (int64, error) {
	return s.exec(ctx, `INSERT INTO groups(name,target_id,template_id,created_at) VALUES(?,?,?,?)`, g.Name, g.TargetID, g.TemplateID, ts(time.Now()))
}

func scanGroup(row interface{ Scan(...any) error }) (*Group, error) {
	var g Group
	var c string
	if err := row.Scan(&g.ID, &g.Name, &g.TargetID, &g.TemplateID, &c); err != nil {
		return nil, notFound(err)
	}
	g.CreatedAt = parseTS(c)
	return &g, nil
}

func (s *Store) Group(ctx context.Context, id int64) (*Group, error) {
	return scanGroup(s.db.QueryRowContext(ctx, `SELECT id,name,target_id,template_id,created_at FROM groups WHERE id=?`, id))
}

func (s *Store) Groups(ctx context.Context) ([]Group, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,target_id,template_id,created_at FROM groups ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Group
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *g)
	}
	return out, rows.Err()
}
