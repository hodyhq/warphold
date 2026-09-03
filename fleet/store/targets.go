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
	StorageMode                            string // "disk" or "cloud"
	MirrorKind, MirrorBucket, MirrorRegion string
	SealedMirrorKey                        []byte
	MirrorLockVerifiedAt                   *time.Time
}

const targetCols = `id,name,kind,bucket,region,path,sealed_admin_key,object_lock_verified_at,created_at,storage_mode,mirror_kind,mirror_bucket,mirror_region,sealed_mirror_key,mirror_lock_verified_at`

func (s *Store) CreateTarget(ctx context.Context, t *Target) (int64, error) {
	return s.exec(ctx, `INSERT INTO targets(name,kind,bucket,region,path,sealed_admin_key,object_lock_verified_at,created_at,storage_mode,mirror_kind,mirror_bucket,mirror_region,sealed_mirror_key,mirror_lock_verified_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.Name, t.Kind, t.Bucket, t.Region, t.Path, t.SealedAdminKey, tsp(t.ObjectLockVerifiedAt), ts(t.CreatedAt),
		t.StorageMode, t.MirrorKind, t.MirrorBucket, t.MirrorRegion, t.SealedMirrorKey, tsp(t.MirrorLockVerifiedAt))
}

func scanTarget(row interface{ Scan(...any) error }) (*Target, error) {
	var t Target
	var olv, mlv sql.NullString
	var c string
	if err := row.Scan(&t.ID, &t.Name, &t.Kind, &t.Bucket, &t.Region, &t.Path, &t.SealedAdminKey, &olv, &c,
		&t.StorageMode, &t.MirrorKind, &t.MirrorBucket, &t.MirrorRegion, &t.SealedMirrorKey, &mlv); err != nil {
		return nil, notFound(err)
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

func (s *Store) SetTargetObjectLockVerified(ctx context.Context, id int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE targets SET object_lock_verified_at=? WHERE id=?`, ts(at), id)
	return err
}
