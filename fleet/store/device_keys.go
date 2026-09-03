package store

import (
	"context"
	"database/sql"
	"time"
)

// DeviceKey is a gateway credential issued to one device. SealedSecret holds
// the secret access key sealed with the fleet key; it is never logged and never
// leaves the store in the clear except through the gateway's own lookup.
type DeviceKey struct {
	AccessKeyID  string
	AgentID      string
	SealedSecret []byte
	Prefix       string
	ReadOnly     bool
	CreatedAt    time.Time
	DisabledAt   *time.Time
}

const deviceKeyCols = `access_key_id,agent_id,sealed_secret,prefix,read_only,created_at,disabled_at`

// CreateDeviceKey inserts a gateway key.
func (s *Store) CreateDeviceKey(ctx context.Context, k *DeviceKey) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO device_keys(`+deviceKeyCols+`) VALUES(?,?,?,?,?,?,?)`,
		k.AccessKeyID, k.AgentID, k.SealedSecret, k.Prefix, k.ReadOnly, ts(k.CreatedAt), tsp(k.DisabledAt))
	return err
}

func scanDeviceKey(row interface{ Scan(...any) error }) (*DeviceKey, error) {
	var k DeviceKey
	var created string
	var disabled sql.NullString
	if err := row.Scan(&k.AccessKeyID, &k.AgentID, &k.SealedSecret, &k.Prefix, &k.ReadOnly, &created, &disabled); err != nil {
		return nil, notFound(err)
	}
	k.CreatedAt = parseTS(created)
	k.DisabledAt = parseTSP(disabled)
	return &k, nil
}

// DeviceKey looks up an active key by access key id. A disabled key is
// ErrNotFound: a revoked device must be indistinguishable from an unknown one.
func (s *Store) DeviceKey(ctx context.Context, accessKeyID string) (*DeviceKey, error) {
	return scanDeviceKey(s.db.QueryRowContext(ctx,
		`SELECT `+deviceKeyCols+` FROM device_keys WHERE access_key_id=? AND disabled_at IS NULL`, accessKeyID))
}

// DeviceKeysForAgent returns every key of an agent, disabled ones included, so
// passphrase rotation can re-seal them all.
func (s *Store) DeviceKeysForAgent(ctx context.Context, agentID string) ([]DeviceKey, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+deviceKeyCols+` FROM device_keys WHERE agent_id=? ORDER BY created_at, access_key_id`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DeviceKey
	for rows.Next() {
		k, err := scanDeviceKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *k)
	}
	return out, rows.Err()
}

// DisableDeviceKeysForAgent disables every still-active key of an agent and
// returns how many it disabled.
func (s *Store) DisableDeviceKeysForAgent(ctx context.Context, agentID string, at time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE device_keys SET disabled_at=? WHERE agent_id=? AND disabled_at IS NULL`, ts(at), agentID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
