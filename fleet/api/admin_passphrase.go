package api

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"os"

	"github.com/kopia/kopia/fleet/seal"
	"github.com/kopia/kopia/fleet/store"
)

// pendingKeySuffix names the new key file a rotation writes before its
// transaction, and renames over seal.key after it commits.
const pendingKeySuffix = ".new"

// minPassphraseLen is the floor for a *new* sealing passphrase. It is higher
// than activation's 8 because rotation is the one chance to raise the bar on a
// fleet that was activated with a weak one.
const minPassphraseLen = 12

// ErrWrongPassphrase is returned when current does not derive the loaded key.
var ErrWrongPassphrase = errors.New("current sealing passphrase is wrong")

// ErrWeakPassphrase is returned when the new passphrase is too short.
var ErrWeakPassphrase = errors.New("the new sealing passphrase needs 12+ characters")

// RotatePassphrase re-seals every sealed value under a key derived from next
// and a fresh salt, and reports how many rows it re-sealed per table.
//
// The ordering is what makes a crash survivable, and it is the reverse of
// Activate's for the same reason - the key file is the last thing that moves:
//
//  1. verify current derives the loaded key, so a wrong guess writes nothing;
//  2. write the new key to seal.key.new, tagged with its salt;
//  3. ONE transaction: re-seal every sealed value and write the new salt;
//  4. commit;
//  5. rename seal.key.new over seal.key and swap the in-memory key.
//
// A crash before 4 leaves the old key file and the old ciphertexts, and the
// pending file's salt does not match the stored one, so the next Open deletes
// it. A crash between 4 and 5 leaves the new ciphertexts and a pending file
// whose salt does match, so the next Open finishes the rename. Either way the
// fleet comes back consistent (see recoverPendingKey).
//
// With dryRun the transaction is rolled back and no file is touched; the
// counts are the ones a real rotation would report.
func (s *Server) RotatePassphrase(ctx context.Context, current, next string, dryRun bool) (map[string]int, error) {
	if len(next) < minPassphraseLen {
		return nil, ErrWeakPassphrase
	}

	// Exclusive for the whole rotation: every handler that seals or unseals
	// holds this for reading across its own store write, so none of them can
	// seal with the old key while the transaction below re-seals.
	s.sealMu.Lock()
	defer s.sealMu.Unlock()

	st := s.store()
	if st == nil {
		return nil, errors.New("fleet is not activated")
	}

	saltHex, err := st.Setting(ctx, store.SealSaltSetting)
	if err != nil {
		return nil, err
	}

	salt, err := hex.DecodeString(saltHex)
	if err != nil || len(salt) == 0 {
		return nil, errors.New("the stored sealing salt is missing or malformed")
	}

	old := s.sealKey()
	derived := seal.Derive(current, salt)

	if subtle.ConstantTimeCompare(derived[:], old[:]) != 1 {
		return nil, ErrWrongPassphrase
	}

	newSalt, err := seal.NewSalt()
	if err != nil {
		return nil, err
	}

	newKey := seal.Derive(next, newSalt)
	pending := s.paths.KeyFile + pendingKeySuffix

	if !dryRun {
		if err := seal.WritePendingKeyFile(pending, newKey, newSalt); err != nil {
			return nil, err
		}
	}

	counts, err := st.Reseal(ctx, hex.EncodeToString(newSalt), dryRun, func(sealed []byte) ([]byte, error) {
		plain, err := old.Open(sealed)
		if err != nil {
			return nil, err
		}
		return newKey.Seal(plain)
	})
	if err != nil {
		if !dryRun {
			_ = os.Remove(pending)
		}
		return nil, err
	}

	if dryRun {
		return counts, nil
	}

	if err := s.crashForTesting(); err != nil {
		return nil, err
	}

	// Committed: the store now holds the new salt and the new ciphertexts, so
	// the pending key is the only one that can read them.
	renameErr := os.Rename(pending, s.paths.KeyFile)

	// The in-memory key follows the store even when the rename failed:
	// otherwise this process would keep sealing new values with a key that
	// nothing can read back, on top of ciphertexts it can no longer open.
	s.mu.Lock()
	s.key = newKey
	s.mu.Unlock()

	// The gateway cached both the old sealing key and the device secrets it
	// unsealed with it; drop both so the next device request re-reads them.
	s.resetGateway()

	if renameErr != nil {
		// The next start recovers it from the salt, but say so now: until then
		// seal.key on disk cannot open this store.
		log.Printf("warphold fleet: the rotation committed but %s could not be renamed over %s: %v", pending, s.paths.KeyFile, renameErr)
		return nil, renameErr
	}

	return counts, nil
}

// recoverPendingKey finishes or discards a seal.key.new left behind by a
// rotation that crashed between its transaction and the rename. The pending
// file records the salt its key was derived from: if the store already holds
// that salt the transaction committed and the rename must finish, otherwise it
// did not and the pending key can never open anything.
func (s *Server) recoverPendingKey() {
	pending := s.paths.KeyFile + pendingKeySuffix

	_, salt, err := seal.ReadPendingKeyFile(pending)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		// The pending file is written atomically before the transaction, so a
		// malformed one belongs to a rotation that never got as far as
		// committing: it is safe to drop, and leaving it would block the next.
		log.Printf("warphold fleet: discarding malformed %s: %v", pending, err)
		_ = os.Remove(pending)

		return
	}

	if storedSalt(s.paths.DB) == hex.EncodeToString(salt) {
		if err := os.Rename(pending, s.paths.KeyFile); err != nil {
			// Loud: the store holds ciphertexts only the pending key opens.
			log.Printf("warphold fleet: cannot finish the passphrase rotation (%s -> %s): %v", pending, s.paths.KeyFile, err)
		}

		return
	}

	_ = os.Remove(pending)
}

// storedSalt reads the sealing salt straight from the database file, without
// creating one: a missing database means nothing was ever committed.
func storedSalt(dbPath string) string {
	if _, err := os.Stat(dbPath); err != nil {
		return ""
	}

	st, err := store.Open(dbPath)
	if err != nil {
		return ""
	}

	defer st.Close() //nolint:errcheck

	v, err := st.Setting(context.Background(), store.SealSaltSetting)
	if err != nil {
		return ""
	}

	return v
}

// SetRotateCrashForTesting installs a hook that runs after the rotation's
// transaction commits and before the key file is renamed - the one window the
// ordering exists to survive. Returning an error from it simulates the crash.
func (s *Server) SetRotateCrashForTesting(f func() error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rotateCrash = f
}

func (s *Server) crashForTesting() error {
	s.mu.RLock()
	f := s.rotateCrash
	s.mu.RUnlock()

	if f == nil {
		return nil
	}

	return f()
}

func (s *Server) handlePassphraseRotate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Current string `json:"current"`
		New     string `json:"new"`
		DryRun  bool   `json:"dry_run"`
	}

	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed body")
		return
	}

	// Same bucket and shape as the login limiter: this endpoint verifies a
	// passphrase, so without it an admin session doubles as an offline-speed
	// oracle for the sealing passphrase.
	if !s.login.allow(clientIP(r)) {
		writeErr(w, http.StatusTooManyRequests, "too many attempts, wait a minute")
		return
	}

	counts, err := s.RotatePassphrase(r.Context(), in.Current, in.New, in.DryRun)

	switch {
	case errors.Is(err, ErrWrongPassphrase):
		writeErr(w, http.StatusForbidden, err.Error())
	case errors.Is(err, ErrWeakPassphrase):
		writeErr(w, http.StatusBadRequest, err.Error())
	case err != nil:
		adminFailed(w, "rotate the sealing passphrase", err)
	default:
		writeJSON(w, http.StatusOK, map[string]any{"resealed": counts, "dry_run": in.DryRun})
	}
}
