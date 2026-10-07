package store

import (
	"context"
	"errors"
	"time"
)

// ErrGroupInUse is returned by DeleteGroup when an unretired agent or a live
// enrollment token still references the group.
var ErrGroupInUse = errors.New("group is in use")

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
	var (
		g Group
		c string
	)
	if err := row.Scan(&g.ID, &g.Name, &g.TargetID, &g.TemplateID, &c); err != nil {
		return nil, notFound(err)
	}

	g.CreatedAt = parseTS(c)

	return &g, nil
}

func (s *Store) Group(ctx context.Context, id int64) (*Group, error) {
	return scanGroup(s.db.QueryRowContext(ctx, `SELECT id,name,target_id,template_id,created_at FROM groups WHERE id=? AND deleted_at IS NULL`, id))
}

func (s *Store) Groups(ctx context.Context) ([]Group, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,target_id,template_id,created_at FROM groups WHERE deleted_at IS NULL ORDER BY id`)
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
// validating that a new target/template exists.
//
// A target_id change is refused with ErrGroupInUse when any agent that still
// has a repository -- live, or revoked but not yet reaped -- enrolled through
// the group: that repository lives on whatever target was current at
// enrollment, so retargeting would silently orphan it. A retired agent (the
// reap job removed its repository) has nothing left to orphan, so it does not
// count. That check and the write are the
// same UPDATE statement: the WHERE guard is evaluated against each row's
// pre-update value of target_id, so a device enrolling between a check and a
// separate write can't slip through -- there is no separate write. Retargeting
// to the target_id already in place is never treated as a change, so it is
// allowed regardless of enrolled devices.
func (s *Store) UpdateGroup(ctx context.Context, id int64, name *string, targetID, templateID *int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE groups SET
		name=COALESCE(?,name), target_id=COALESCE(?,target_id), template_id=COALESCE(?,template_id)
		WHERE id=? AND deleted_at IS NULL
		AND (COALESCE(?,target_id)=target_id OR NOT EXISTS (SELECT 1 FROM agents WHERE group_id=? AND retired_at IS NULL))`,
		name, targetID, templateID, id, targetID, id)
	if err != nil {
		return err
	}

	n, err := res.RowsAffected()
	if err != nil {
		return err
	}

	if n == 1 {
		return nil
	}
	// 0 rows: either the group doesn't exist, or the guard above blocked a
	// real repoint. Whatever agent blocked it can't have vanished in this
	// gap -- DeleteGroup refuses to remove a group any unretired agent still
	// references -- so this read is safe without a transaction.
	if _, err := s.Group(ctx, id); err != nil {
		return err // ErrNotFound, or a real failure
	}

	return ErrGroupInUse
}

// DeleteGroup removes a group, refusing with ErrGroupInUse when an agent that
// still has a repository (live, or revoked but not yet reaped) or a live
// (unrevoked, unexpired, not used up) enrollment token references it. Stale
// tokens are deleted first -- their FK to groups has no ON DELETE clause, and
// a revoked, expired or used-up one no longer authorizes anything. The delete
// re-checks both conditions in the statement itself, so a row created between
// the cleanup and here cannot race it through.
//
// The delete stamps deleted_at rather than removing the row: a retired
// device's agents row is the fleet's history and keeps its group_id (no ON
// DELETE clause, and schema.sql only ever grows columns), so the row has to
// stay for that FK. Group and Groups treat a stamped group as gone.
func (s *Store) DeleteGroup(ctx context.Context, id int64, now time.Time) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM enrollment_tokens WHERE group_id=? AND (revoked_at IS NOT NULL OR expires_at<=? OR (max_uses>0 AND uses>=max_uses))`, id, ts(now)); err != nil {
		return err
	}

	res, err := s.db.ExecContext(ctx, `UPDATE groups SET deleted_at=? WHERE id=? AND deleted_at IS NULL
		AND NOT EXISTS (SELECT 1 FROM agents WHERE group_id=? AND retired_at IS NULL)
		AND NOT EXISTS (SELECT 1 FROM enrollment_tokens WHERE group_id=? AND revoked_at IS NULL AND expires_at>? AND (max_uses=0 OR uses<max_uses))`,
		ts(now), id, id, id, ts(now))
	if err != nil {
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
