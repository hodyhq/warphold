package jobs

import (
	"context"
	"encoding/hex"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/mail"
	"github.com/kopia/kopia/fleet/seal"
	"github.com/kopia/kopia/fleet/store"
)

// rotatingKey stands in for fleet/api's (*Server).unseal: the sealing key a
// passphrase rotation swaps under a scheduler that is already running. Runners
// have to reach the key through this on every unseal; a runner that copied a
// seal.Key when it was built keeps opening with the retired one.
type rotatingKey struct {
	mu  sync.RWMutex
	key seal.Key
}

func (r *rotatingKey) open(sealed []byte) ([]byte, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.key.Open(sealed)
}

// rotate re-seals every ciphertext reseal would touch, then swaps the key --
// the same order store.Reseal commits in.
func (r *rotatingKey) rotate(t *testing.T, reseal func(old, next seal.Key)) seal.Key {
	t.Helper()

	salt, err := seal.NewSalt()
	require.NoError(t, err)

	next := seal.Derive("the rotated passphrase", salt)

	r.mu.Lock()
	old := r.key
	r.mu.Unlock()

	reseal(old, next)

	r.mu.Lock()
	r.key = next
	r.mu.Unlock()

	return next
}

// resealed opens a value with old and re-seals it under next.
func resealed(t *testing.T, old, next seal.Key, sealed []byte) []byte {
	t.Helper()

	plain, err := old.Open(sealed)
	require.NoError(t, err)

	out, err := next.Seal(plain)
	require.NoError(t, err)

	return out
}

// A passphrase rotation used to leave the running scheduler holding the
// retired key: jobs.Runners took a seal.Key, which is [32]byte, so every
// runner closure got a frozen copy while RotatePassphrase re-sealed the store
// under a new one. Every job then failed silently and permanently until the
// process restarted -- verify/test-restore/maintenance/stats on the escrowed
// bundle, mirror on the offsite credentials, and digest on the SMTP password,
// so not even the digest could report it.
//
// Each subtest runs its job, rotates underneath it exactly as
// RotatePassphrase does, and runs it again.
func TestRunnersFollowAPassphraseRotation(t *testing.T) {
	t.Run("verify", func(t *testing.T) {
		fx := newRepoFixture(t, "ag_1")
		ctx := context.Background()
		rk := &rotatingKey{key: fx.key}

		// Through Runners, not the constructor: the wiring is half of what
		// regressed.
		run := Runners(fx.st, rk.open, nil)["verify"]

		detail, err := run(ctx, store.Job{Kind: "verify"})
		require.NoError(t, err, detail)
		require.Contains(t, detail, "verified 1/1 ok")

		rk.rotate(t, func(old, next seal.Key) {
			a, err := fx.st.Agent(ctx, "ag_1")
			require.NoError(t, err)
			require.NoError(t, fx.st.SetAgentBundle(ctx, a.ID, resealed(t, old, next, a.SealedBundle)))
		})

		detail, err = run(ctx, store.Job{Kind: "verify"})
		require.NoError(t, err, detail)
		require.Contains(t, detail, "verified 1/1 ok", "the runner must unseal with the rotated key")
	})

	t.Run("mirror", func(t *testing.T) {
		f := newMirrorFixture(t, nil)
		ctx := context.Background()
		rk := &rotatingKey{key: f.key}

		f.write(t, f.dir, "ag_1/p0001", "one")

		run := Runners(f.st, rk.open, nil)["mirror"]

		detail, err := run(ctx, store.Job{Kind: "mirror"})
		require.NoError(t, err, detail)

		rk.rotate(t, func(old, next seal.Key) {
			tgt, err := f.st.Target(ctx, f.target.ID)
			require.NoError(t, err)

			tgt.SealedMirrorKey = resealed(t, old, next, tgt.SealedMirrorKey)
			require.NoError(t, f.st.SetTargetMirror(ctx, tgt))
		})

		f.write(t, f.dir, "ag_1/p0002", "two")

		detail, err = run(ctx, store.Job{Kind: "mirror"})
		require.NoError(t, err, detail)
		require.NotContains(t, detail, "unsealing the mirror credentials failed")
	})

	t.Run("digest", func(t *testing.T) {
		f := newDigestFixture(t)
		ctx := context.Background()
		rk := &rotatingKey{key: f.key}

		f.configureSMTP(t)
		f.addAdmin(t, "owner@example.com")

		sealPassword := func(k seal.Key) {
			v, err := mail.SealPassword(k, "hunter2")
			require.NoError(t, err)
			require.NoError(t, f.st.SetSetting(ctx, mail.PasswordKey, v))
		}
		sealPassword(f.key)

		var sent int

		send := func(context.Context, []string, string, string, string) error { sent++; return nil }
		run := Digest(f.st, rk.open, send)

		detail, err := run(ctx, store.Job{Kind: "digest"})
		require.NoError(t, err, detail)
		require.Equal(t, 1, sent)

		rk.rotate(t, func(old, next seal.Key) {
			v, err := f.st.Setting(ctx, mail.PasswordKey)
			require.NoError(t, err)

			raw, err := hex.DecodeString(v)
			require.NoError(t, err)

			out := resealed(t, old, next, raw)
			require.NoError(t, f.st.SetSetting(ctx, mail.PasswordKey, hex.EncodeToString(out)))
		})

		detail, err = run(ctx, store.Job{Kind: "digest"})
		require.NoError(t, err, detail)
		require.Equal(t, 2, sent, "the digest must unseal the SMTP password with the rotated key")
	})
}
