package engine_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/agent/engine"
)

func TestInfoWriteReadRemove(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WARPHOLD_STATE_DIR", dir)

	path := filepath.Join(dir, "engine.json")

	_, err := engine.ReadInfo("user")
	require.Error(t, err, "no engine.json means no running engine")

	want := engine.Info{
		BaseURL:    "http://127.0.0.1:34567",
		User:       "warphold-agent",
		Password:   "hunter2",
		LocalToken: "0123456789abcdef",
		PID:        os.Getpid(),
		StartedAt:  time.Now().UTC().Truncate(time.Millisecond),
	}
	require.NoError(t, engine.WriteInfo("user", want))

	// engine.json holds the engine's password and the local handoff token.
	st, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), st.Mode().Perm())

	got, err := engine.ReadInfo("user")
	require.NoError(t, err)
	require.Equal(t, want.BaseURL, got.BaseURL)
	require.Equal(t, want.User, got.User)
	require.Equal(t, want.Password, got.Password)
	require.Equal(t, want.LocalToken, got.LocalToken)
	require.Equal(t, want.PID, got.PID)
	require.True(t, want.StartedAt.Equal(got.StartedAt))

	require.NoError(t, engine.RemoveInfo("user"))
	require.NoFileExists(t, path)
	require.NoError(t, engine.RemoveInfo("user"), "removing a missing info file is not an error")
}

// TestInfoRewriteTightensMode pins that a pre-existing world-readable
// engine.json does not survive a rewrite with its old mode.
func TestInfoRewriteTightensMode(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WARPHOLD_STATE_DIR", dir)
	path := filepath.Join(dir, "engine.json")

	require.NoError(t, os.WriteFile(path, []byte("{}"), 0o644))
	require.NoError(t, engine.WriteInfo("user", engine.Info{BaseURL: "http://127.0.0.1:1"}))

	st, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), st.Mode().Perm())
}
