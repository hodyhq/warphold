package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet"
	"github.com/kopia/kopia/fleet/api"
)

func TestResolveDataDir(t *testing.T) {
	configFile := filepath.Join(t.TempDir(), "repository.config")

	t.Run("default is next to the config file", func(t *testing.T) {
		if fi, err := os.Stat(fleetDataRoot); err == nil && fi.IsDir() {
			t.Skipf("%v exists on this host, so the default may be the data root", fleetDataRoot)
		}
		got, err := resolveDataDir("", configFile)
		require.NoError(t, err)
		require.Equal(t, filepath.Join(filepath.Dir(configFile), "data"), got)
	})

	t.Run("relative becomes absolute", func(t *testing.T) {
		got, err := resolveDataDir("some/where", configFile)
		require.NoError(t, err)
		require.True(t, filepath.IsAbs(got), "stored path must be absolute, got %v", got)
		require.Equal(t, "where", filepath.Base(got))
	})

	t.Run("a symlinked directory is refused", func(t *testing.T) {
		base := t.TempDir()
		real := filepath.Join(base, "real")
		link := filepath.Join(base, "link")
		require.NoError(t, os.Mkdir(real, 0o700))
		require.NoError(t, os.Symlink(real, link))

		_, err := resolveDataDir(link, configFile)
		require.ErrorContains(t, err, "symlink")
	})
}

// TestEnsureFleetRepoIsIdempotent: setup calls it once and every `server start`
// calls it again, so a second call must find the same repository rather than
// initialize a second one over the first.
func TestEnsureFleetRepoIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "repository.config")

	fs := api.New(fleet.StateDirFor(configFile))
	t.Cleanup(func() { fs.Close() })
	require.NoError(t, fs.Activate(t.Context(), "seal-me-please", "hody@hody.dev", "pw12345678", ""))

	repoDir, repoConfig, err := ensureFleetRepo(t.Context(), fs, configFile, "")
	require.NoError(t, err)

	formatBlob, err := os.ReadFile(filepath.Join(repoDir, "kopia.repository.f"))
	require.NoError(t, err)

	repoDir2, repoConfig2, err := ensureFleetRepo(t.Context(), fs, configFile, "")
	require.NoError(t, err)
	require.Equal(t, repoDir, repoDir2)
	require.Equal(t, repoConfig, repoConfig2)

	again, err := os.ReadFile(filepath.Join(repoDir, "kopia.repository.f"))
	require.NoError(t, err)
	require.Equal(t, formatBlob, again, "the repository was re-initialized")
}
