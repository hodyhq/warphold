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
	return s.exec(ctx, `INSERT INTO commands(agent_id,kind,source,created_at) VALUES(?,?,?,?)`, c.AgentID, c.Kind, c.Source, ts(c.CreatedAt))
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

// CommandAgentID returns the agent_id owning command id, or ErrNotFound.
func (s *Store) CommandAgentID(ctx context.Context, id int64) (string, error) {
	var agentID string
	err := s.db.QueryRowContext(ctx, `SELECT agent_id FROM commands WHERE id=?`, id).Scan(&agentID)
	if err != nil {
		return "", notFound(err)
	}
	return agentID, nil
}

// AckCommand marks command id acknowledged, scoped to agentID so one agent
// cannot ack (and so silently discard) another agent's command. Returns
// ErrNotFound if no matching, still-pending command exists for that agent.
func (s *Store) AckCommand(ctx context.Context, id int64, agentID string, at time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE commands SET acked_at=? WHERE id=? AND agent_id=? AND acked_at IS NULL`, ts(at), id, agentID)
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
