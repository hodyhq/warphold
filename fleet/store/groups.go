package store

import (
	"context"
	"errors"
	"time"

	"modernc.org/sqlite"
)

// ErrGroupInUse is returned by DeleteGroup when a non-revoked agent or a live
// enrollment token still references the group.
var ErrGroupInUse = errors.New("group is in use")

// sqliteConstraint is SQLite's primary result code for any constraint
// violation (unique, not-null, check or foreign key); Error.Code() carries an
// extended code in the high bits, so a caller must mask them off to compare
// against this. See https://www.sqlite.org/rescode.html#constraint.
const sqliteConstraint = 19

type Group struct {
	ID                   int64
	Name                 string
	TargetID, TemplateID int64
	CreatedAt            time.Time
}

func (s *Store) CreateGroup(ctx context.Context, g *Group) (int64, error) {
	return s.exec(ctx, `INSERT INTO groups(name,target_id,template_id,created_at) VALUES(?,?,?,?)`, g.Name, g.TargetID, g.TemplateID, ts(g.CreatedAt))
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

// UpdateGroup applies a partial update to a group: name, target_id and
// template_id are each left unchanged when nil. The caller is responsible for
// validating a new target/template exists and, for a target_id change, that
// the group has no enrolled devices -- this only writes the row.
func (s *Store) UpdateGroup(ctx context.Context, id int64, name *string, targetID, templateID *int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE groups SET name=COALESCE(?,name), target_id=COALESCE(?,target_id), template_id=COALESCE(?,template_id) WHERE id=?`,
		name, targetID, templateID, id)
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

// GroupHasAgents reports whether any agent -- revoked or not -- has ever
// enrolled through this group. Its repository lives on whatever target was
// current at enrollment time, so retargeting the group would silently orphan
// that data even for a device that was later revoked; this is what gates a
// target_id change in UpdateGroup, separately from the narrower check
// DeleteGroup makes.
func (s *Store) GroupHasAgents(ctx context.Context, id int64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agents WHERE group_id=?`, id).Scan(&n)
	return n > 0, err
}

// DeleteGroup removes a group, refusing with ErrGroupInUse when a non-revoked
// agent or a live (unrevoked, unexpired) enrollment token still references
// it. Stale tokens are deleted first -- their FK to groups has no ON DELETE
// clause, so a revoked or expired one would otherwise block the DELETE below
// even though it no longer authorizes anything. The final DELETE re-checks
// the same two conditions in the statement itself, so a row created between
// the cleanup and here cannot race the delete through.
//
// A group that a device once enrolled through and was later revoked from
// still has an agents row pointing at it -- agents.group_id has no ON DELETE
// clause and schema.sql only ever grows columns, so that FK cannot be
// relaxed. SQLite reports that as a constraint-violation error rather than
// simply not matching the WHERE guard above, so it is translated to
// ErrGroupInUse here too: from the caller's side it is the same "group is in
// use" story, just discovered a statement later.
func (s *Store) DeleteGroup(ctx context.Context, id int64, now time.Time) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM enrollment_tokens WHERE group_id=? AND (revoked_at IS NOT NULL OR expires_at<=?)`, id, ts(now)); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM groups WHERE id=?
		AND NOT EXISTS (SELECT 1 FROM agents WHERE group_id=? AND revoked_at IS NULL)
		AND NOT EXISTS (SELECT 1 FROM enrollment_tokens WHERE group_id=? AND revoked_at IS NULL AND expires_at>?)`,
		id, id, id, ts(now))
	if err != nil {
		var sqliteErr *sqlite.Error
		if errors.As(err, &sqliteErr) && sqliteErr.Code()&0xff == sqliteConstraint {
			return ErrGroupInUse
		}
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 1 {
		return nil
	}
	if _, err := s.Group(ctx, id); err != nil {
		return err // ErrNotFound, or a real failure
	}
	return ErrGroupInUse
}
