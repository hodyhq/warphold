package tray

import (
	"testing"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
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
