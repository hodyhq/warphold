package store

import (
	"context"
	"database/sql"
	"time"
)

type Command struct {
	ID                    int64
	AgentID, Kind, Source string
	CreatedAt             time.Time
	AckedAt               *time.Time
}

func (s *Store) AddCommand(ctx context.Context, c *Command) (int64, error) {
	return s.exec(ctx, `INSERT INTO commands(agent_id,kind,source,created_at) VALUES(?,?,?,?)`, c.AgentID, c.Kind, c.Source, ts(time.Now()))
}

func (s *Store) PendingCommands(ctx context.Context, agentID string) ([]Command, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,agent_id,kind,source,created_at,acked_at FROM commands WHERE agent_id=? AND acked_at IS NULL ORDER BY id`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Command
	for rows.Next() {
		var c Command
		var created string
		var acked sql.NullString
		if err := rows.Scan(&c.ID, &c.AgentID, &c.Kind, &c.Source, &created, &acked); err != nil {
			return nil, err
		}
		c.CreatedAt, c.AckedAt = parseTS(created), parseTSP(acked)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) AckCommand(ctx context.Context, id int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE commands SET acked_at=? WHERE id=?`, ts(at), id)
	return err
}
