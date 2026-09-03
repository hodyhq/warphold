package tray

import (
	"os"
	"testing"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/agent/state"
)

// TestSnapshotAllAttemptsEveryPath pins that one source the engine refuses
// does not stop the others: every path is attempted and every failure is
// reported.
func TestSnapshotAllAttemptsEveryPath(t *testing.T) {
	var tried []string

	err := snapshotAll([]string{"/home/user", "/gone", "/etc"}, func(p string) error {
		tried = append(tried, p)

		switch p {
		case "/gone":
			return errors.New("no such directory")
		case "/etc":
			return errors.New("permission denied")
		}

		return nil
	})

	require.Equal(t, []string{"/home/user", "/gone", "/etc"}, tried)
	require.ErrorContains(t, err, "/gone")
	require.ErrorContains(t, err, "no such directory")
	require.ErrorContains(t, err, "/etc")
	require.ErrorContains(t, err, "permission denied")

	require.NoError(t, snapshotAll(nil, func(string) error { return errors.New("never called") }))
}

// TestVaultLabelInAppMode pins the standalone app's menu label: there is no
// enrollment to read a name from, so the machine's own hostname is what the
// user sees - and no group, because there is no Fleet.
func TestVaultLabelInAppMode(t *testing.T) {
	t.Setenv("WARPHOLD_STATE_DIR", t.TempDir())

	host, err := os.Hostname()
	require.NoError(t, err)

	c := &client{scope: state.ScopeApp}
	require.Equal(t, host, c.vault())

	// An agent with no agent.json still falls back to the product name, not
	// to the hostname: an unenrolled agent is not the app.
	a := &client{scope: state.ScopeUser}
	require.Equal(t, "WarpHold", a.vault())
}
