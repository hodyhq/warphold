package api

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
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

	// A pending key nobody could resolve means the store may already be on a
	// key this process does not hold; rotating on top of that would bury it.
	if err := s.StateError(); err != nil {
		return nil, err
	}

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

	newSalt, err := seal.NewSalt()
	if err != nil {
		return nil, err
	}

	// Both argon2id runs (64 MiB, ~100 ms each) happen before the exclusive
	// lock: holding it across them would let a wrong guess - the one thing an
	// attacker can send in bulk - stall every enroll and poll in the fleet.
	derived, newKey := seal.Derive(current, salt), seal.Derive(next, newSalt)

	// Exclusive for the whole rotation: every handler that seals or unseals
	// holds this for reading across its own store write, so none of them can
	// seal with the old key while the transaction below re-seals.
	s.sealMu.Lock()
	defer s.sealMu.Unlock()

	// Read under the lock, and compared against a key derived from the salt
	// read before it: a rotation that committed in between leaves a different
	// key here, and this one reports a wrong passphrase rather than re-sealing
	// from a stale starting point.
	old := s.sealKey()

	if subtle.ConstantTimeCompare(derived[:], old[:]) != 1 {
		return nil, ErrWrongPassphrase
	}

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
		// The pending key stays on disk even though this run failed: the last
		// thing Reseal does is Commit, and a Commit that reports an error is
		// not proof that nothing was written. If it did commit, this file is
		// the only key that opens the store, and recoverPendingKey settles it
		// at the next start by comparing salts. A stale one costs nothing: its
		// salt will not match, so it is dropped then.
		if !dryRun {
			log.Printf("warphold fleet: rotation failed, leaving %s for the next start to settle: %v", pending, err)
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
// that salt the transaction committed and the rename must finish.
//
// It deletes the file in exactly one case - the store handed back a salt, and
// it is not this file's - because that is the only reading that proves the
// transaction did not commit. Every other outcome (an unreadable store, an
// unreadable or malformed pending file, a failed rename) is ambiguous, and the
// ambiguous case is the one where this file is the only key that can open the
// store. Those keep the file and return an error, which stops the server from
// coming up: serving on the old key would seal new secrets into a store the
// rest of which needs the pending one.
func (s *Server) recoverPendingKey() error {
	pending := s.paths.KeyFile + pendingKeySuffix

	_, salt, err := seal.ReadPendingKeyFile(pending)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}

		return keepPending(pending, fmt.Errorf("cannot read the pending sealing key: %w", err))
	}

	stored, err := storedSalt(s.paths.DB)
	if err != nil {
		return keepPending(pending, fmt.Errorf("cannot read %s from %s: %w", store.SealSaltSetting, s.paths.DB, err))
	}

	if stored == hex.EncodeToString(salt) {
		// The transaction committed: this key, and only this key, opens the store.
		if err := os.Rename(pending, s.paths.KeyFile); err != nil {
			return keepPending(pending, fmt.Errorf("cannot finish the passphrase rotation (%s -> %s): %w", pending, s.paths.KeyFile, err))
		}

		return nil
	}

	// The store holds a different salt, so the rotation this file belongs to
	// never committed and its key can never open anything.
	if err := os.Remove(pending); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("cannot remove the dead pending sealing key %s: %w", pending, err)
	}

	return nil
}

// keepPending is the fail-closed exit: it says what is on disk, what it is
// worth and what not to do with it, and returns the error that stops the start.
func keepPending(pending string, err error) error {
	log.Printf("warphold fleet: %v", err)
	log.Printf("warphold fleet: refusing to start. %s is kept: it may be the only key that opens this fleet's store. "+
		"Fix the underlying problem (disk space, permissions, a restored or moved fleet.db) and start again; "+
		"do not delete it until a start has succeeded without it.", pending)

	return err
}

// storedSalt reads the sealing salt straight from the database file. Every
// failure is an error rather than an empty string: "no salt" and "could not
// read the salt" have opposite consequences for the pending key.
func storedSalt(dbPath string) (string, error) {
	if _, err := os.Stat(dbPath); err != nil {
		return "", err
	}

	st, err := store.Open(dbPath)
	if err != nil {
		return "", err
	}

	defer st.Close() //nolint:errcheck

	v, err := st.Setting(context.Background(), store.SealSaltSetting)
	if err != nil {
		return "", err
	}

	if v == "" {
		return "", errors.New("the store holds no sealing salt")
	}

	return v, nil
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
	case errors.Is(err, store.ErrSealedNotHex):
		// Admin-only, and the setting name is the whole diagnosis: without it
		// this is an "internal error" on a fleet that will never rotate again.
		writeErr(w, http.StatusInternalServerError, err.Error())
	case err != nil:
		adminFailed(w, "rotate the sealing passphrase", err)
	default:
		writeJSON(w, http.StatusOK, map[string]any{"resealed": counts, "dry_run": in.DryRun})
	}
}
