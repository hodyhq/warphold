package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// SealedSettingPrefix marks a setting whose value is sealed with the fleet key.
// Rotation re-seals every setting whose key starts with it, so a new sealed
// setting (the SMTP password, say) is covered without touching this file.
//
// The value of such a setting is hex, because settings.value is TEXT and a
// secretbox ciphertext is not valid UTF-8. Reseal fails loudly on a value that
// is not hex rather than writing back something it cannot decode.
const SealedSettingPrefix = "sealed_"

// ErrSealedNotHex marks a sealed setting whose value is not hex, so the API
// can name the setting instead of answering "internal error" on a fleet that
// will refuse to rotate until someone finds it.
var ErrSealedNotHex = errors.New("sealed settings must be hex-encoded")

// SealSaltSetting holds the hex salt the fleet key is derived from.
const SealSaltSetting = "seal_salt"

// Reseal re-seals every sealed value in the store in one transaction and
// writes newSalt (hex) to the seal salt setting. reseal is called with each
// stored ciphertext and must return the value sealed under the new key.
//
// With dryRun the transaction is rolled back after the same work, so the
// counts are exactly the ones a real rotation would report and nothing is
// written. The counts are per row: a target holding both an admin and a mirror
// key counts once.
//
// The caller must hold the sealing key exclusively for the whole call:
// anything that seals with the old key while this runs would be left behind.
func (s *Store) Reseal(ctx context.Context, newSalt string, dryRun bool, reseal func([]byte) ([]byte, error)) (map[string]int, error) {
	// Checked before anything else: a salt that is not hex would commit a
	// store no restart can match against a pending key file.
	if raw, err := hex.DecodeString(newSalt); err != nil || len(raw) == 0 {
		return nil, fmt.Errorf("the new sealing salt must be non-empty hex: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit, and the dry run's whole point

	counts := map[string]int{"agents": 0, "targets": 0, "device_keys": 0, "settings": 0}

	if err := resealBlobs(ctx, tx, counts, "agents", `SELECT id, sealed_bundle FROM agents`,
		`UPDATE agents SET sealed_bundle=? WHERE id=?`, reseal); err != nil {
		return nil, err
	}

	if err := resealTargets(ctx, tx, counts, reseal); err != nil {
		return nil, err
	}

	if err := resealBlobs(ctx, tx, counts, "device_keys", `SELECT access_key_id, sealed_secret FROM device_keys`,
		`UPDATE device_keys SET sealed_secret=? WHERE access_key_id=?`, reseal); err != nil {
		return nil, err
	}

	if err := resealSettings(ctx, tx, counts, reseal); err != nil {
		return nil, err
	}

	if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		SealSaltSetting, newSalt); err != nil {
		return nil, err
	}

	if dryRun {
		return counts, nil
	}

	return counts, tx.Commit()
}

// row is one sealed blob and the primary key it belongs to.
type sealedRow struct {
	id     string
	sealed []byte
}

// resealBlobs handles the one-blob-per-row tables. Every row is read before
// any is written: the store runs on a single connection, so an open cursor and
// an UPDATE cannot overlap.
func resealBlobs(ctx context.Context, tx *sql.Tx, counts map[string]int, name, selectQ, updateQ string, reseal func([]byte) ([]byte, error)) error {
	rows, err := tx.QueryContext(ctx, selectQ)
	if err != nil {
		return err
	}

	var found []sealedRow

	for rows.Next() {
		var r sealedRow
		if err := rows.Scan(&r.id, &r.sealed); err != nil {
			rows.Close()
			return err
		}

		found = append(found, r)
	}

	rows.Close()

	if err := rows.Err(); err != nil {
		return err
	}

	for _, r := range found {
		if len(r.sealed) == 0 {
			continue
		}

		next, err := reseal(r.sealed)
		if err != nil {
			return fmt.Errorf("%s %s: %w", name, r.id, err)
		}

		if _, err := tx.ExecContext(ctx, updateQ, next, r.id); err != nil {
			return err
		}

		counts[name]++
	}

	return nil
}

func resealTargets(ctx context.Context, tx *sql.Tx, counts map[string]int, reseal func([]byte) ([]byte, error)) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, sealed_admin_key, sealed_mirror_key FROM targets`)
	if err != nil {
		return err
	}

	type target struct {
		id            int64
		admin, mirror []byte
	}

	var found []target

	for rows.Next() {
		var t target
		if err := rows.Scan(&t.id, &t.admin, &t.mirror); err != nil {
			rows.Close()
			return err
		}

		found = append(found, t)
	}

	rows.Close()

	if err := rows.Err(); err != nil {
		return err
	}

	for _, t := range found {
		admin, mirror := t.admin, t.mirror
		if len(admin) == 0 && len(mirror) == 0 {
			continue
		}

		if len(admin) > 0 {
			if admin, err = reseal(admin); err != nil {
				return fmt.Errorf("target %d admin key: %w", t.id, err)
			}
		}

		if len(mirror) > 0 {
			if mirror, err = reseal(mirror); err != nil {
				return fmt.Errorf("target %d mirror key: %w", t.id, err)
			}
		}

		if _, err := tx.ExecContext(ctx, `UPDATE targets SET sealed_admin_key=?, sealed_mirror_key=? WHERE id=?`,
			nullBlob(admin), nullBlob(mirror), t.id); err != nil {
			return err
		}

		counts["targets"]++
	}

	return nil
}

// nullBlob keeps an absent key NULL rather than turning it into an empty blob.
func nullBlob(b []byte) any {
	if len(b) == 0 {
		return nil
	}

	return b
}

func resealSettings(ctx context.Context, tx *sql.Tx, counts map[string]int, reseal func([]byte) ([]byte, error)) error {
	// LIKE reads _ as a single-character wildcard, so the prefix is escaped:
	// only keys starting with the literal "sealed_" are re-sealed.
	rows, err := tx.QueryContext(ctx, `SELECT key, value FROM settings WHERE key LIKE ? ESCAPE '\'`,
		strings.ReplaceAll(SealedSettingPrefix, "_", `\_`)+"%")
	if err != nil {
		return err
	}

	var found []sealedRow

	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			rows.Close()
			return err
		}

		raw, err := hex.DecodeString(value)
		if err != nil {
			rows.Close()
			return fmt.Errorf("%w: setting %s: %w", ErrSealedNotHex, key, err)
		}

		found = append(found, sealedRow{id: key, sealed: raw})
	}

	rows.Close()

	if err := rows.Err(); err != nil {
		return err
	}

	for _, r := range found {
		if len(r.sealed) == 0 {
			continue
		}

		next, err := reseal(r.sealed)
		if err != nil {
			return fmt.Errorf("setting %s: %w", r.id, err)
		}

		if _, err := tx.ExecContext(ctx, `UPDATE settings SET value=? WHERE key=?`, hex.EncodeToString(next), r.id); err != nil {
			return err
		}

		counts["settings"]++
	}

	return nil
}
