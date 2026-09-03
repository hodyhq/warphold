package api_test

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/api"
	"github.com/kopia/kopia/fleet/seal"
	"github.com/kopia/kopia/fleet/store"
	"github.com/kopia/kopia/repo/blob"
)

// The passphrase every harness activates with; rotation verifies against it.
const oldPassphrase = "seal-me!"

// seeded is the plaintext behind every value seedSealed puts in the store, so
// a test can assert the rotation moved the ciphertexts and not the contents.
var seeded = []string{
	"bundle-1", "bundle-2",
	"admin-key-1", "admin-key-2", "mirror-key-2",
	"device-secret-1", "device-secret-2", "device-secret-3",
	"smtp-password",
}

// currentKey derives the sealing key the store is currently sealed under.
func currentKey(t *testing.T, st *store.Store, passphrase string) seal.Key {
	t.Helper()

	saltHex, err := st.Setting(context.Background(), store.SealSaltSetting)
	require.NoError(t, err)
	require.NotEmpty(t, saltHex)

	salt, err := hex.DecodeString(saltHex)
	require.NoError(t, err)

	return seal.Derive(passphrase, salt)
}

// seedSealed writes one of every sealed value a rotation has to move: two
// agents, two targets (one of them mirrored), three gateway keys and a sealed
// setting. The group's own filesystem target has no sealed key at all, so the
// counts also prove rotation skips rows with nothing to re-seal.
func (h *harness) seedSealed(t *testing.T) {
	t.Helper()

	ctx := context.Background()
	st := h.s.StoreForTesting()
	k := currentKey(t, st, oldPassphrase)
	now := time.Now().UTC()

	seal1 := func(plain string) []byte {
		b, err := k.Seal([]byte(plain))
		require.NoError(t, err)

		return b
	}

	gid := int64(h.mkGroup(t))

	_, err := st.CreateTarget(ctx, &store.Target{
		Name: "b2-one", Kind: "b2", Bucket: "b1", SealedAdminKey: seal1("admin-key-1"), CreatedAt: now,
	})
	require.NoError(t, err)

	_, err = st.CreateTarget(ctx, &store.Target{
		Name: "b2-two", Kind: "b2", Bucket: "b2", SealedAdminKey: seal1("admin-key-2"),
		SealedMirrorKey: seal1("mirror-key-2"), CreatedAt: now,
	})
	require.NoError(t, err)

	for i, id := range []string{"agent-1", "agent-2"} {
		require.NoError(t, st.CreateAgent(ctx, &store.Agent{
			ID: id, Name: id, Hostname: id, OS: "linux", Arch: "amd64", Scope: "user", GroupID: gid,
			BearerHash: []byte(id), SealedBundle: seal1("bundle-" + string(rune('1'+i))), EnrolledAt: now,
		}))
	}

	for i, ak := range []struct{ id, agent, secret string }{
		{"AKIA1", "agent-1", "device-secret-1"},
		{"AKIA2", "agent-1", "device-secret-2"},
		{"AKIA3", "agent-2", "device-secret-3"},
	} {
		require.NoError(t, st.CreateDeviceKey(ctx, &store.DeviceKey{
			AccessKeyID: ak.id, AgentID: ak.agent, SealedSecret: seal1(ak.secret),
			Prefix: ak.agent + "/", CreatedAt: now.Add(time.Duration(i) * time.Second),
		}))
	}

	require.NoError(t, st.SetSetting(ctx, "sealed_smtp_password", hex.EncodeToString(seal1("smtp-password"))))
}

// unsealAll opens every sealed value in the store with k. It returns an error
// as soon as one does not open, which is what "nothing opens under the old
// key" is asserted with.
func unsealAll(st *store.Store, k seal.Key) ([]string, error) {
	ctx := context.Background()

	var out []string

	open := func(b []byte) error {
		plain, err := k.Open(b)
		if err != nil {
			return err
		}

		out = append(out, string(plain))

		return nil
	}

	agents, err := st.Agents(ctx)
	if err != nil {
		return nil, err
	}

	for _, a := range agents {
		if err := open(a.SealedBundle); err != nil {
			return nil, err
		}

		keys, err := st.DeviceKeysForAgent(ctx, a.ID)
		if err != nil {
			return nil, err
		}

		for _, dk := range keys {
			if err := open(dk.SealedSecret); err != nil {
				return nil, err
			}
		}
	}

	targets, err := st.Targets(ctx)
	if err != nil {
		return nil, err
	}

	for _, tg := range targets {
		for _, b := range [][]byte{tg.SealedAdminKey, tg.SealedMirrorKey} {
			if len(b) == 0 {
				continue
			}

			if err := open(b); err != nil {
				return nil, err
			}
		}
	}

	v, err := st.Setting(ctx, "sealed_smtp_password")
	if err != nil {
		return nil, err
	}

	raw, err := hex.DecodeString(v)
	if err != nil {
		return nil, err
	}

	if err := open(raw); err != nil {
		return nil, err
	}

	return out, nil
}

func keyFileBytes(t *testing.T, dir string) []byte {
	t.Helper()

	b, err := os.ReadFile(filepath.Join(dir, "seal.key"))
	require.NoError(t, err)

	return b
}

func TestRotatePassphraseResealsEverything(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	h.seedSealed(t)

	st := h.s.StoreForTesting()
	oldKey := currentKey(t, st, oldPassphrase)
	oldFile := keyFileBytes(t, h.stateDir)

	const newPassphrase = "a-much-longer-passphrase"

	wantCounts := map[string]any{"agents": 2.0, "targets": 2.0, "device_keys": 3.0, "settings": 1.0}

	// A new passphrase under 12 characters is refused before anything else.
	resp, body := h.do("POST", "/api/v1/fleet/settings/passphrase", map[string]any{"current": oldPassphrase, "new": "short"})
	require.Equal(t, 400, resp.StatusCode, body)

	// Dry run: real counts, no writes at all.
	resp, body = h.do("POST", "/api/v1/fleet/settings/passphrase",
		map[string]any{"current": oldPassphrase, "new": newPassphrase, "dry_run": true})
	require.Equal(t, 200, resp.StatusCode, body)
	require.Equal(t, wantCounts, body["resealed"])
	require.Equal(t, oldFile, keyFileBytes(t, h.stateDir), "a dry run must not touch seal.key")
	require.NoFileExists(t, filepath.Join(h.stateDir, "seal.key.new"))

	got, err := unsealAll(st, oldKey)
	require.NoError(t, err, "a dry run must leave every value sealed under the old key")
	require.ElementsMatch(t, seeded, got)

	// A wrong current passphrase is a 403 and writes nothing.
	resp, body = h.do("POST", "/api/v1/fleet/settings/passphrase",
		map[string]any{"current": "not-the-passphrase", "new": newPassphrase})
	require.Equal(t, 403, resp.StatusCode, body)
	require.Equal(t, oldFile, keyFileBytes(t, h.stateDir))

	got, err = unsealAll(st, oldKey)
	require.NoError(t, err)
	require.ElementsMatch(t, seeded, got)

	// The real thing.
	resp, body = h.do("POST", "/api/v1/fleet/settings/passphrase",
		map[string]any{"current": oldPassphrase, "new": newPassphrase})
	require.Equal(t, 200, resp.StatusCode, body)
	require.Equal(t, wantCounts, body["resealed"])

	require.NotEqual(t, oldFile, keyFileBytes(t, h.stateDir), "seal.key must hold the new key")
	require.NoFileExists(t, filepath.Join(h.stateDir, "seal.key.new"), "the pending key file is renamed, not left behind")

	newKey := currentKey(t, st, newPassphrase)
	require.NotEqual(t, oldKey, newKey, "the new key comes from a fresh salt")

	onDisk, err := seal.ReadKeyFile(filepath.Join(h.stateDir, "seal.key"))
	require.NoError(t, err)
	require.Equal(t, newKey, onDisk)

	got, err = unsealAll(st, newKey)
	require.NoError(t, err, "every value must open under the new key")
	require.ElementsMatch(t, seeded, got)

	_, err = unsealAll(st, oldKey)
	require.ErrorIs(t, err, seal.ErrTampered, "nothing may still open under the old key")

	// The live server sealed with the new key too: a settings write goes
	// through the same lock the rotation held.
	resp, _ = h.do("PUT", "/api/v1/fleet/settings", map[string]any{"fleet_name": "after-rotation"})
	require.Equal(t, 200, resp.StatusCode)
}

// A wrong current passphrase must cost the same as a wrong login: the endpoint
// is otherwise an online oracle for the sealing passphrase.
func TestRotatePassphraseWrongCurrentIsRateLimited(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin() // one hit on the shared per-IP bucket

	for i := range 5 {
		resp, body := h.do("POST", "/api/v1/fleet/settings/passphrase",
			map[string]any{"current": "wrong", "new": "a-much-longer-passphrase"})
		require.Equal(t, 403, resp.StatusCode, "attempt %d: %v", i, body)
	}

	resp, _ := h.do("POST", "/api/v1/fleet/settings/passphrase",
		map[string]any{"current": "wrong", "new": "a-much-longer-passphrase"})
	require.Equal(t, 429, resp.StatusCode)
}

// Crash point one: the pending key file was written but the transaction never
// committed. Its salt does not match the stored one, so the next Open drops it
// and the fleet stays on the old key.
func TestRotatePassphraseRecoversFromCrashBeforeCommit(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	h.seedSealed(t)

	oldKey := currentKey(t, h.s.StoreForTesting(), oldPassphrase)
	oldFile := keyFileBytes(t, h.stateDir)
	pending := filepath.Join(h.stateDir, "seal.key.new")

	strandedSalt, err := seal.NewSalt()
	require.NoError(t, err)
	require.NoError(t, seal.WritePendingKeyFile(pending, seal.Derive("never-committed", strandedSalt), strandedSalt))

	require.NoError(t, h.s.Close())

	s2 := api.New(h.stateDir)
	defer s2.Close() //nolint:errcheck

	require.NoFileExists(t, pending, "a pending key whose salt is not the stored one is dead")
	require.Equal(t, oldFile, keyFileBytes(t, h.stateDir))

	got, err := unsealAll(s2.StoreForTesting(), oldKey)
	require.NoError(t, err)
	require.ElementsMatch(t, seeded, got)
}

// Crash point two: the transaction committed and the process died before the
// rename. The store's salt now matches the pending key, so the next Open
// finishes the rotation instead of leaving a fleet no key can open.
func TestRotatePassphraseRecoversFromCrashBeforeKeyRename(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	h.seedSealed(t)

	const newPassphrase = "a-much-longer-passphrase"

	boom := errors.New("power cut")
	h.s.SetRotateCrashForTesting(func() error { return boom })

	_, err := h.s.RotatePassphrase(context.Background(), oldPassphrase, newPassphrase, false)
	require.ErrorIs(t, err, boom)

	pending := filepath.Join(h.stateDir, "seal.key.new")
	require.FileExists(t, pending, "the committed rotation's key must still be on disk")

	require.NoError(t, h.s.Close())

	s2 := api.New(h.stateDir)
	defer s2.Close() //nolint:errcheck

	require.NoFileExists(t, pending, "the rename finishes on the next open")

	st := s2.StoreForTesting()
	newKey := currentKey(t, st, newPassphrase)

	onDisk, err := seal.ReadKeyFile(filepath.Join(h.stateDir, "seal.key"))
	require.NoError(t, err)
	require.Equal(t, newKey, onDisk, "seal.key is the key the committed transaction sealed with")

	got, err := unsealAll(st, newKey)
	require.NoError(t, err)
	require.ElementsMatch(t, seeded, got)
}

// While a rotation holds the sealing key, a request that seals must wait for
// it rather than write a value sealed with the key being replaced.
func TestRotationBlocksSealingWriters(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	h.seedSealed(t)

	started, release := make(chan struct{}), make(chan struct{})
	h.s.SetRotateCrashForTesting(func() error {
		close(started)
		<-release

		return nil
	})

	rotated := make(chan error, 1)

	go func() {
		_, err := h.s.RotatePassphrase(context.Background(), oldPassphrase, "a-much-longer-passphrase", false)
		rotated <- err
	}()

	<-started

	// Built on this goroutine (the harness jar is not safe to share) and sent
	// from another, so the assertion below is about the server, not the client.
	req := h.newRequest("PUT", "/api/v1/fleet/settings", jsonBody(map[string]any{"fleet_name": "during-rotation"}))
	done := make(chan int, 1)

	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- 0
			return
		}

		defer resp.Body.Close()

		done <- resp.StatusCode
	}()

	select {
	case code := <-done:
		t.Fatalf("a sealing write completed (%d) while the rotation held the key", code)
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	require.NoError(t, <-rotated)
	require.Equal(t, 200, <-done, "the write runs once the rotation lets go")
}

// The gateway caches the sealing key and the device secrets it unsealed with
// it, so a rotation that did not drop both would leave every enrolled device
// unable to reach its repository until the process restarted.
func TestRotatePassphraseKeepsTheGatewayWorking(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.activateAndLogin()
	h.setPublicURL()
	gid := h.mkHostedGroup(t, t.TempDir())
	_, tok := h.do("POST", "/api/v1/fleet/tokens", map[string]any{"group_id": gid})

	admin := h.jar
	h.jar = nil
	resp, body := h.do("POST", "/api/v1/fleet/enroll",
		map[string]any{"token": tok["token"], "hostname": "fw16", "os": "linux", "arch": "amd64", "scope": "user"})
	require.Equal(t, 201, resp.StatusCode, body)

	st := deviceStore(t, body["connect_token"].(string))

	// Warms the gateway's key cache with a secret unsealed under the old key.
	found, err := blob.ListAllBlobs(ctx, st, "kopia.repository")
	require.NoError(t, err)
	require.Len(t, found, 1)

	h.jar = admin
	resp, body = h.do("POST", "/api/v1/fleet/settings/passphrase",
		map[string]any{"current": oldPassphrase, "new": "a-much-longer-passphrase"})
	require.Equal(t, 200, resp.StatusCode, body)

	found, err = blob.ListAllBlobs(ctx, st, "kopia.repository")
	require.NoError(t, err, "the device's credential must survive a passphrase rotation")
	require.Len(t, found, 1)
}
