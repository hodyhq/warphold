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

// TestJobsAPIQueuesAndLists covers the two endpoints the dashboard drives:
// queue a job for one device, and read that device's job history back.
func TestJobsAPIQueuesAndLists(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()

	id, _ := enrollAgent(t, h)

	resp, body := h.do("POST", "/api/v1/fleet/jobs", map[string]any{"kind": "verify", "agent_id": id})
	require.Equal(t, 202, resp.StatusCode, body)
	require.NotZero(t, body["id"])

	// Every kind the server can actually run is accepted; nothing else is.
	for _, kind := range []string{"verify", "test-restore", "maintenance", "mirror", "reap"} {
		resp, body := h.do("POST", "/api/v1/fleet/jobs", map[string]any{"kind": kind})
		require.Equal(t, 202, resp.StatusCode, body)
	}

	resp, body = h.do("POST", "/api/v1/fleet/jobs", map[string]any{"kind": "nonesuch"})
	require.Equal(t, 400, resp.StatusCode)
	require.Contains(t, body["error"], "test-restore", "the error names what is accepted")

	resp, _ = h.do("POST", "/api/v1/fleet/jobs", map[string]any{"kind": "verify", "agent_id": "ag_nope"})
	require.Equal(t, 404, resp.StatusCode)

	resp, list := h.doList("GET", "/api/v1/fleet/agents/"+id+"/jobs")
	require.Equal(t, 200, resp.StatusCode)
	require.Len(t, list, 1, "only the job that names this agent")
	require.Equal(t, "verify", list[0]["kind"])
	require.Equal(t, id, list[0]["agent_id"])
	require.Contains(t, list[0], "detail")
	require.Contains(t, list[0], "scheduled_for")

	resp, _ = h.doList("GET", "/api/v1/fleet/agents/ag_nope/jobs")
	require.Equal(t, 404, resp.StatusCode)

	// And it is admin-only, like the rest of the control plane.
	saved := h.jar
	h.jar = nil
	resp, _ = h.do("POST", "/api/v1/fleet/jobs", map[string]any{"kind": "verify"})
	require.Equal(t, 401, resp.StatusCode)
	resp, _ = h.doList("GET", "/api/v1/fleet/agents/"+id+"/jobs")
	require.Equal(t, 401, resp.StatusCode)
	h.jar = saved
}
