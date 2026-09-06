package mail

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/seal"
	"github.com/kopia/kopia/fleet/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "fleet.db"))
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })

	return st
}

func TestLoadFallsBackToDefaultsAndRoundTripsThroughTheSeal(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	salt, err := seal.NewSalt()
	require.NoError(t, err)

	k := seal.Derive("pw", salt)

	c, err := Load(ctx, st, k.Open)
	require.NoError(t, err)
	require.Equal(t, Defaults(), c, "nothing stored yet")

	sealed, err := SealPassword(k, "s3cret")
	require.NoError(t, err)
	require.NotContains(t, sealed, "s3cret", "stored at rest sealed")
	require.NoError(t, st.SetSettings(ctx, map[string]string{
		HostKey: "mail.example.com", PortKey: "587", UsernameKey: "u",
		PasswordKey: sealed, FromKey: "fleet@example.com", TLSKey: "false",
		"public_url": "https://fleet.example.com",
	}))

	c, err = Load(ctx, st, k.Open)
	require.NoError(t, err)
	require.Equal(t, Config{
		Host: "mail.example.com", Port: 587, Username: "u", Password: "s3cret",
		From: "fleet@example.com", TLS: false, MessageIDHost: "fleet.example.com",
	}, c)

	// The wrong key must fail loudly rather than sending with a blank password.
	_, err = Load(ctx, st, seal.Derive("other", salt).Open)
	require.Error(t, err)
}

func TestSenderForUsesTheStoredSettings(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	salt, _ := seal.NewSalt()
	k := seal.Derive("pw", salt)
	s := startStub(t, true)
	sealed, err := SealPassword(k, "pw")
	require.NoError(t, err)
	require.NoError(t, st.SetSettings(ctx, map[string]string{
		HostKey: "127.0.0.1", PortKey: strconv.Itoa(s.port), UsernameKey: "user",
		PasswordKey: sealed, FromKey: "fleet@example.com", TLSKey: "true",
	}))

	send := SenderFor(st, k.Open)
	require.NoError(t, send(ctx, []string{"ops@example.com"}, "hi", "t", "<p>h</p>"))

	auth, from, rcpt, _ := s.snapshot()
	require.Equal(t, "\x00user\x00pw", auth)
	require.Equal(t, "fleet@example.com", from)
	require.Equal(t, []string{"ops@example.com"}, rcpt)
}
