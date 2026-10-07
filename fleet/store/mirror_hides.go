package store

import (
	"context"
	"time"
)

// AddMirrorHides records n mirror objects hidden for one target's device at
// at, and drops rows older than before, which is the oldest the guard reads.
func (s *Store) AddMirrorHides(ctx context.Context, targetID int64, device string, at, before time.Time, n int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	if _, err := tx.ExecContext(ctx, `DELETE FROM mirror_hides WHERE hidden_at < ?`, ts(before)); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `INSERT INTO mirror_hides(target_id,device,hidden_at,n) VALUES(?,?,?,?)`,
		targetID, device, ts(at), n); err != nil {
		return err
	}

	return tx.Commit()
}

// MirrorHidesSince is how many objects were hidden for one target's device at
// or after since.
func (s *Store) MirrorHidesSince(ctx context.Context, targetID int64, device string, since time.Time) (int, error) {
	var n int

	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(n),0) FROM mirror_hides WHERE target_id=? AND device=? AND hidden_at >= ?`,
		targetID, device, ts(since)).Scan(&n)

	return n, err
}
