package store

import (
	"context"
	"database/sql"
)

// addedColumns is the fixed list of columns added to tables that predate them.
// schema.sql is CREATE TABLE IF NOT EXISTS throughout, which creates new tables
// on an existing database but silently skips new columns on an existing table.
//
// Every entry is a compile-time constant, so interpolating it into the DDL (the
// only way SQLite takes a column name) introduces no injection surface.
var addedColumns = []struct{ table, column, decl string }{
	{"targets", "storage_mode", "TEXT NOT NULL DEFAULT ''"},
	{"targets", "mirror_kind", "TEXT NOT NULL DEFAULT ''"},
	{"targets", "mirror_bucket", "TEXT NOT NULL DEFAULT ''"},
	{"targets", "mirror_region", "TEXT NOT NULL DEFAULT ''"},
	{"targets", "sealed_mirror_key", "BLOB"},
	{"targets", "mirror_lock_verified_at", "TEXT"},
	{"agents", "retired_at", "TEXT"},
}

// migrate adds every column in addedColumns that PRAGMA table_info says is
// missing. It is additive only: nothing is ever dropped, retyped or backfilled,
// so it is idempotent and safe to run on every Open.
func migrate(ctx context.Context, db *sql.DB) error {
	for _, c := range addedColumns {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=?`, c.table, c.column).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		if _, err := db.ExecContext(ctx, `ALTER TABLE `+c.table+` ADD COLUMN `+c.column+` `+c.decl); err != nil {
			return err
		}
	}
	return nil
}
