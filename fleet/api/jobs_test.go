package api_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet"
	"github.com/kopia/kopia/fleet/store"
)

// TestJobSchedulerRunsWithTheServer pins the scheduler's lifecycle: activation
// starts it (so an unactivated Fleet runs nothing), and Close stops it before
// the store is closed.
func TestJobSchedulerRunsWithTheServer(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()

	// A second connection to the same DB: the server owns its own.
	st, err := store.Open(fleet.PathsFor(h.stateDir).DB)
	require.NoError(t, err)

	defer st.Close() //nolint:errcheck // test cleanup

	require.Eventually(t, func() bool {
		js, err := st.RecentJobs(context.Background(), "mirror", 1)

		return err == nil && len(js) == 1 && js[0].Status == "ok"
	}, 10*time.Second, 10*time.Millisecond, "the fleet enqueues and runs its own mirror job")

	require.NoError(t, h.s.Close())
	require.NoError(t, h.s.Close()) // idempotent, and the scheduler is already stopped
}
