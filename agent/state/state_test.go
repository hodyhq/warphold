package state_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/agent/state"
)

func TestSaveLoad0600(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WARPHOLD_STATE_DIR", dir)
	c := &state.Config{Server: "https://fleet.example", AgentID: "ag_1", Bearer: "wa_x", Name: "fw13", PollInterval: 300, Scope: "user"}
	require.NoError(t, state.Save("user", c))
	st, err := os.Stat(filepath.Join(dir, "agent.json"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), st.Mode().Perm())
	got, err := state.Load("user")
	require.NoError(t, err)
	require.Equal(t, c, got)
	require.Equal(t, filepath.Join(dir, "repository.config"), state.RepoConfigPath("user"))
}

func TestDirsWithoutOverride(t *testing.T) {
	t.Setenv("WARPHOLD_STATE_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg")
	require.Equal(t, "/tmp/xdg/warphold", state.Dir("user"))
	require.Equal(t, "/etc/warphold", state.Dir("system"))
	require.Equal(t, "/var/cache/warphold", state.CacheDir("system"))
}
