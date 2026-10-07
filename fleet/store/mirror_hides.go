package store

import (
	"context"
	"fmt"
	"time"
)

// ReserveMirrorHides reserves n mirror hides for one target's device at at,
// but only if the hides recorded at or after since, plus n, stay within limit. The
// read, the check and the insert share one transaction (and the store has a
// single connection), so two concurrent runs cannot both pass on the same
// count. Rows older than since are pruned. It returns the count already in
// the window and whether the reservation was made.
func (s *Store) ReserveMirrorHides(ctx context.Context, targetID int64, device string, at, since time.Time, n, limit int) (int, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, fmt.Errorf("mirror hides: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	if _, err := tx.ExecContext(ctx, `DELETE FROM mirror_hides WHERE hidden_at < ?`, ts(since)); err != nil {
		return 0, false, fmt.Errorf("pruning mirror hides: %w", err)
	}

	var earlier int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(n),0) FROM mirror_hides WHERE target_id=? AND device=? AND hidden_at >= ?`,
		targetID, device, ts(since)).Scan(&earlier); err != nil {
		return 0, false, fmt.Errorf("reading mirror hides: %w", err)
	}

	if earlier+n > limit {
		return earlier, false, nil
	}

	if _, err := tx.ExecContext(ctx, `INSERT INTO mirror_hides(target_id,device,hidden_at,n) VALUES(?,?,?,?)`,
		targetID, device, ts(at), n); err != nil {
		return 0, false, fmt.Errorf("recording mirror hides: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("recording mirror hides: %w", err)
	}

	return earlier, true, nil
}
