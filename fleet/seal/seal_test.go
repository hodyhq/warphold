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
	// Test too-short input lengths
	_, err = k.Open([]byte{})
	require.ErrorIs(t, err, seal.ErrTampered)
	_, err = k.Open([]byte("short"))
	require.ErrorIs(t, err, seal.ErrTampered)
}

func TestKeyFileIs0600(t *testing.T) {
	tmpDir := t.TempDir()
	subDir := filepath.Join(tmpDir, "sub")
	// Pre-create dir with looser permissions to test enforcement
	require.NoError(t, os.MkdirAll(subDir, 0o755))
	p := filepath.Join(subDir, "seal.key")
	salt, _ := seal.NewSalt()
	k := seal.Derive("pw", salt)
	require.NoError(t, seal.WriteKeyFile(p, k))
	// Verify file mode is 0600
	st, err := os.Stat(p)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), st.Mode().Perm())
	// Verify directory mode was enforced to 0700
	dirSt, err := os.Stat(subDir)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), dirSt.Mode().Perm())
	// Verify roundtrip
	got, err := seal.ReadKeyFile(p)
	require.NoError(t, err)
	require.Equal(t, k, got)
}

func TestReadKeyFileRejectsMalformed(t *testing.T) {
	// Test rejection of invalid hex
	p := filepath.Join(t.TempDir(), "bad-hex.key")
	require.NoError(t, os.WriteFile(p, []byte("zz\n"), 0o600))
	_, err := seal.ReadKeyFile(p)
	require.Error(t, err)

	// Test rejection of valid hex but wrong length (16 bytes instead of 32)
	p2 := filepath.Join(t.TempDir(), "short.key")
	require.NoError(t, os.WriteFile(p2, []byte("00112233445566778899aabbccddeeff\n"), 0o600))
	_, err = seal.ReadKeyFile(p2)
	require.Error(t, err)
}
