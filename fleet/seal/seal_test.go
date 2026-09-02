package seal_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/seal"
)

func TestDeriveIsDeterministicAndSaltSensitive(t *testing.T) {
	salt, err := seal.NewSalt()
	require.NoError(t, err)
	require.Len(t, salt, 16)
	k1 := seal.Derive("correct horse", salt)
	k2 := seal.Derive("correct horse", salt)
	require.Equal(t, k1, k2)
	salt2, _ := seal.NewSalt()
	require.NotEqual(t, k1, seal.Derive("correct horse", salt2))
}

func TestSealOpenRoundTripAndTamper(t *testing.T) {
	salt, _ := seal.NewSalt()
	k := seal.Derive("pw", salt)
	sealed, err := k.Seal([]byte("repo-password-32-bytes"))
	require.NoError(t, err)
	plain, err := k.Open(sealed)
	require.NoError(t, err)
	require.Equal(t, "repo-password-32-bytes", string(plain))
	sealed[len(sealed)-1] ^= 0xff
	_, err = k.Open(sealed)
	require.ErrorIs(t, err, seal.ErrTampered)
	_, err = seal.Derive("other", salt).Open(sealed[:len(sealed)-1])
	require.Error(t, err)
}

func TestKeyFileIs0600(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "seal.key")
	salt, _ := seal.NewSalt()
	k := seal.Derive("pw", salt)
	require.NoError(t, seal.WriteKeyFile(p, k))
	st, err := os.Stat(p)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), st.Mode().Perm())
	got, err := seal.ReadKeyFile(p)
	require.NoError(t, err)
	require.Equal(t, k, got)
}
