package cli_test

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/agent/state"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/snapshot/policy"
	"github.com/kopia/kopia/tests/testenv"
)

// passwordFor reads the password persisted next to the repository config file by
// Kopia's default file-based password-persistence strategy (see
// internal/passwordpersist/passwordpersist_file.go): base64 in "<cfg>.kopia-password".
func passwordFor(t *testing.T, cfg string) string {
	t.Helper()
	b, err := os.ReadFile(cfg + ".kopia-password")
	require.NoError(t, err)
	dec, err := base64.StdEncoding.DecodeString(string(b))
	require.NoError(t, err)
	return string(dec)
}

func TestAgentRunOnceAppliesPolicyAndReports(t *testing.T) {
	url, tok := fleetForTest(t)
	stateDir := t.TempDir()
	t.Setenv("WARPHOLD_STATE_DIR", stateDir)
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "note.txt"), []byte("x"), 0o600))

	runner := testenv.NewInProcRunner(t)
	e := testenv.NewCLITest(t, nil, runner)
	e.RunAndExpectSuccess(t, "agent", "enroll", "--server", url, "--token", tok)
	e.RunAndExpectSuccess(t, "agent", "run", "--once")

	st, err := state.Load("user")
	require.NoError(t, err)
	require.NotEmpty(t, st.ETag)

	pw := passwordFor(t, state.RepoConfigPath("user"))
	r, err := repo.Open(context.Background(), state.RepoConfigPath("user"), pw, nil)
	require.NoError(t, err)
	defer r.Close(context.Background())
	pols, err := policy.ListPolicies(context.Background(), r)
	require.NoError(t, err)
	var found bool
	for _, p := range pols {
		if p.Target().Path == home {
			found = true
			require.EqualValues(t, 3, *p.RetentionPolicy.KeepLatest)
		}
	}
	require.True(t, found, "template source '~' expanded to HOME and applied")
}
