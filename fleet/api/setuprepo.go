package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"

	"github.com/kopia/kopia/fleet/store"
)

const (
	// fleetRepoPasswordSetting holds the password of the Fleet host's own
	// repository, sealed with the activation passphrase and hex-encoded
	// because settings are TEXT. Nothing but this Fleet needs it: it exists so
	// the server can reconnect its own repository without an operator typing
	// anything, and so passphrase rotation can re-seal it with everything else.
	//
	// The store.SealedSettingPrefix is load-bearing, not decoration: Reseal
	// sweeps `sealed\_%` and nothing else, so this key spelled without it was
	// skipped by every rotation and the Fleet host's own repository became
	// unopenable the first time anyone rotated. Databases written under the
	// old name are moved by renamedSettings in fleet/store/migrate.go.
	fleetRepoPasswordSetting = store.SealedSettingPrefix + "fleet_repo_password"
	// fleetRepoPathSetting records where that repository lives, so a server
	// started without the --data-dir the operator activated with still finds it.
	fleetRepoPathSetting = "fleet_repo_path"

	fleetRepoPasswordBytes = 32
)

// FleetRepoPassword returns the password of the Fleet host's own repository,
// generating and sealing one on first call and returning the same one after
// that. Setup calls it once; `server start` calls it on every start, which is
// why it must be idempotent - a second password would orphan the repository.
func (s *Server) FleetRepoPassword(ctx context.Context) (string, error) {
	st := s.store()
	if st == nil {
		return "", errors.New("fleet is not activated")
	}

	s.setupMu.Lock()
	defer s.setupMu.Unlock()

	// The rotation read lock, for the same reason every sealing route takes it
	// through sealHeld: below is a seal-then-write span, and a rotation
	// committing between the Seal and the SetSetting would store the password
	// under the retired key after Reseal had already swept the settings table.
	// This is not an HTTP handler -- `server start` and OnActivated call it --
	// so nothing above it holds the lock already. Order is setupMu -> sealMu,
	// and it is the only place the two meet.
	s.sealMu.RLock()
	defer s.sealMu.RUnlock()

	raw, err := st.Setting(ctx, fleetRepoPasswordSetting)
	if err != nil {
		return "", err
	}

	if raw != "" {
		sealed, err := hex.DecodeString(raw)
		if err != nil {
			return "", errors.New("stored fleet repository password is corrupt")
		}

		pw, err := s.sealKey().Open(sealed)
		if err != nil {
			return "", err
		}

		return string(pw), nil
	}

	b := make([]byte, fleetRepoPasswordBytes)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}

	pw := hex.EncodeToString(b)

	sealed, err := s.sealKey().Seal([]byte(pw))
	if err != nil {
		return "", err
	}

	if err := st.SetSetting(ctx, fleetRepoPasswordSetting, hex.EncodeToString(sealed)); err != nil {
		return "", err
	}

	return pw, nil
}

// FleetRepoPath returns where the Fleet host's own repository lives, recording
// def the first time and ignoring def afterwards: the repository cannot move
// just because a later `server start` derived a different default.
func (s *Server) FleetRepoPath(ctx context.Context, def string) (string, error) {
	st := s.store()
	if st == nil {
		return "", errors.New("fleet is not activated")
	}

	s.setupMu.Lock()
	defer s.setupMu.Unlock()

	path, err := st.Setting(ctx, fleetRepoPathSetting)
	if err != nil {
		return "", err
	}

	if path != "" {
		return path, nil
	}

	if err := st.SetSetting(ctx, fleetRepoPathSetting, def); err != nil {
		return "", err
	}

	return def, nil
}
