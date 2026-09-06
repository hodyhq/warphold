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
	{"targets", "endpoint", "TEXT NOT NULL DEFAULT ''"},
	// Nullable on purpose: NULL is "never probed", which is what every row
	// written before this column existed truly is.
	{"targets", "mirror_conditional_put", "INTEGER"},
	{"agents", "retired_at", "TEXT"},
}

// renamedSettings moves a settings row to a new key. It exists for one reason:
// a value that is sealed but was stored under a name outside the
// SealedSettingPrefix namespace is invisible to Reseal's `sealed\_%` sweep, so
// a passphrase rotation walks past it and its plaintext becomes unrecoverable.
// Renaming the row is the whole fix -- the ciphertext and its hex encoding are
// already what resealSettings expects.
//
// It is a copy-then-delete, so it is idempotent: after the first Open the old
// key is gone and the SELECT matches nothing.
var renamedSettings = []struct{ from, to string }{
	{"fleet_repo_password", "sealed_fleet_repo_password"},
}

// migrate adds every column in addedColumns that PRAGMA table_info says is
// missing, then applies renamedSettings. It is additive only: nothing is ever
// dropped, retyped or backfilled, so it is idempotent and safe to run on every
// Open.
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
	for _, r := range renamedSettings {
		// OR IGNORE, so a database that already holds the new key keeps it:
		// the new name is the one anything writes now, and it must win over a
		// stale row left by a downgrade-and-upgrade.
		if _, err := db.ExecContext(ctx,
			`INSERT OR IGNORE INTO settings(key,value) SELECT ?, value FROM settings WHERE key=?`, r.to, r.from); err != nil {
			return err
		}
		if _, err := db.ExecContext(ctx, `DELETE FROM settings WHERE key=?`, r.from); err != nil {
			return err
		}
	}
	return nil
}
