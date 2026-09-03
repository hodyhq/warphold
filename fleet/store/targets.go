package store

import (
	"context"
	"database/sql"
	"time"
)

type Target struct {
	ID                   int64
	Name, Kind           string
	Bucket, Region, Path string
	SealedAdminKey       []byte
	ObjectLockVerifiedAt *time.Time
	CreatedAt            time.Time

	// Hosted targets (kind == "hosted") and the optional B2 mirror.
	StorageMode string // "disk" or "cloud"

	// Endpoint is the S3-compatible host a cloud-direct target writes to. It
	// is derived from the region for B2 and stored, so a later reconnect uses
	// the endpoint the bucket was verified against rather than re-deriving it.
	Endpoint                               string
	MirrorKind, MirrorBucket, MirrorRegion string
	SealedMirrorKey                        []byte
	MirrorLockVerifiedAt                   *time.Time

	// MirrorConditionalPut records what the mirror bucket's provider answered
	// to the conditional-write probe: nil when it was never asked (no mirror,
	// or a row written before the column existed), false for a provider that
	// has no If-None-Match at all (B2). A mirror-only target is allowed to be
	// false - the Fleet server is its single writer - so the flag is a fact for
	// the UI to show, not a gate.
	MirrorConditionalPut *bool
}

const targetCols = `id,name,kind,bucket,region,path,sealed_admin_key,object_lock_verified_at,created_at,storage_mode,endpoint,mirror_kind,mirror_bucket,mirror_region,sealed_mirror_key,mirror_lock_verified_at,mirror_conditional_put`

func (s *Store) CreateTarget(ctx context.Context, t *Target) (int64, error) {
	return s.exec(ctx, `INSERT INTO targets(name,kind,bucket,region,path,sealed_admin_key,object_lock_verified_at,created_at,storage_mode,endpoint,mirror_kind,mirror_bucket,mirror_region,sealed_mirror_key,mirror_lock_verified_at,mirror_conditional_put)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.Name, t.Kind, t.Bucket, t.Region, t.Path, t.SealedAdminKey, tsp(t.ObjectLockVerifiedAt), ts(t.CreatedAt),
		t.StorageMode, t.Endpoint, t.MirrorKind, t.MirrorBucket, t.MirrorRegion, t.SealedMirrorKey, tsp(t.MirrorLockVerifiedAt),
		boolp(t.MirrorConditionalPut))
}

func scanTarget(row interface{ Scan(...any) error }) (*Target, error) {
	var t Target
	var olv, mlv sql.NullString
	var mcp sql.NullBool
	var c string
	if err := row.Scan(&t.ID, &t.Name, &t.Kind, &t.Bucket, &t.Region, &t.Path, &t.SealedAdminKey, &olv, &c,
		&t.StorageMode, &t.Endpoint, &t.MirrorKind, &t.MirrorBucket, &t.MirrorRegion, &t.SealedMirrorKey, &mlv, &mcp); err != nil {
		return nil, notFound(err)
	}
	if mcp.Valid {
		t.MirrorConditionalPut = &mcp.Bool
	}
	t.ObjectLockVerifiedAt = parseTSP(olv)
	t.MirrorLockVerifiedAt = parseTSP(mlv)
	t.CreatedAt = parseTS(c)
	return &t, nil
}

func (s *Store) Target(ctx context.Context, id int64) (*Target, error) {
	return scanTarget(s.db.QueryRowContext(ctx, `SELECT `+targetCols+` FROM targets WHERE id=?`, id))
}

func (s *Store) Targets(ctx context.Context) ([]Target, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+targetCols+` FROM targets ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Target
	for rows.Next() {
		t, err := scanTarget(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// SetTargetMirror attaches or replaces a target's mirror. It never clears one:
// every field is written together, from a mirror that has just re-verified.
func (s *Store) SetTargetMirror(ctx context.Context, t *Target) error {
	_, err := s.db.ExecContext(ctx, `UPDATE targets SET mirror_kind=?,mirror_bucket=?,mirror_region=?,sealed_mirror_key=?,mirror_lock_verified_at=?,mirror_conditional_put=? WHERE id=?`,
		t.MirrorKind, t.MirrorBucket, t.MirrorRegion, t.SealedMirrorKey, tsp(t.MirrorLockVerifiedAt), boolp(t.MirrorConditionalPut), t.ID)
	return err
}

func (s *Store) SetTargetObjectLockVerified(ctx context.Context, id int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE targets SET object_lock_verified_at=? WHERE id=?`, ts(at), id)
	return err
}
