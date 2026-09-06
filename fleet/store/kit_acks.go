package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// SetKitAck records that an admin acknowledged holding a device's printed
// recovery kit. Re-acking overwrites: the useful fact is the most recent
// acknowledgement, not the first.
func (s *Store) SetKitAck(ctx context.Context, agentID string, adminID int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO kit_acks(agent_id,acknowledged_at,acknowledged_by) VALUES(?,?,?)
		 ON CONFLICT(agent_id) DO UPDATE SET acknowledged_at=excluded.acknowledged_at, acknowledged_by=excluded.acknowledged_by`,
		agentID, ts(at), adminID)

	return err
}

// KitAck returns when a device's kit was acknowledged, or nil if it was not.
func (s *Store) KitAck(ctx context.Context, agentID string) (*time.Time, error) {
	var at string
	switch err := s.db.QueryRowContext(ctx, `SELECT acknowledged_at FROM kit_acks WHERE agent_id=?`, agentID).Scan(&at); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, err
	}

	t := parseTS(at)

	return &t, nil
}

// KitAcks returns every acknowledgement by agent id, so the Devices list
// resolves the whole fleet in one query rather than one per row.
func (s *Store) KitAcks(ctx context.Context) (map[string]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT agent_id,acknowledged_at FROM kit_acks`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]time.Time{}
	for rows.Next() {
		var id, at string
		if err := rows.Scan(&id, &at); err != nil {
			return nil, err
		}

		out[id] = parseTS(at)
	}

	return out, rows.Err()
}
