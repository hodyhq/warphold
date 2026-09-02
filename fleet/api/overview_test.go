package api_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/agent/poll"
)

// enrollInto enrolls one agent into an existing group, so a test can build a
// fleet of several devices that share one target (enrollAgent creates a fresh
// group, and therefore a fresh target, per call).
func enrollInto(t *testing.T, h *harness, groupID float64, hostname string) (id, bearer string) {
	t.Helper()
	_, tok := h.do("POST", "/api/v1/fleet/tokens", map[string]any{"group_id": groupID})
	admin := h.jar
	h.jar = nil
	resp, body := h.do("POST", "/api/v1/fleet/enroll", map[string]any{"token": tok["token"], "hostname": hostname, "os": "linux", "arch": "amd64", "version": "0.1.0", "scope": "user"})
	require.Equal(t, 201, resp.StatusCode)
	h.jar = admin
	return body["agent_id"].(string), body["bearer"].(string)
}

func report(t *testing.T, h *harness, bearer, task, kind string, finished time.Time, status, stderr string) {
	t.Helper()
	c := &poll.Client{Server: h.srv.URL, Bearer: bearer}
	require.NoError(t, c.Report(t.Context(), poll.Report{
		TaskID: task, Kind: kind, Source: "~",
		StartedAt: finished.Add(-time.Minute), FinishedAt: finished,
		Status: status, Stderr: stderr,
	}))
}

func TestOverviewRequiresAdminAndStartsEmpty(t *testing.T) {
	h := newHarness(t)
	resp, _ := h.do("GET", "/api/v1/fleet/overview", nil)
	require.Equal(t, 409, resp.StatusCode, "not activated")

	h.activateAndLogin()
	saved := h.jar
	h.jar = nil
	resp, _ = h.do("GET", "/api/v1/fleet/overview", nil)
	require.Equal(t, 401, resp.StatusCode, "overview is admin-only")
	h.jar = saved

	resp, body := h.do("GET", "/api/v1/fleet/overview", nil)
	require.Equal(t, 200, resp.StatusCode)
	require.Equal(t, "", body["fleet_name"], "no fleet name set yet")
	require.Equal(t, float64(0), body["counts"].(map[string]any)["agents"])
	require.Equal(t, float64(0), body["counts"].(map[string]any)["targets"])
	require.Empty(t, body["devices"])
	require.Nil(t, body["latest_failure"])
	require.Nil(t, body["dedup_ratio"], "repo stats land with Plan 3")
	require.Equal(t, float64(0), body["stored_bytes"])
	require.Len(t, body["last24h"].(map[string]any)["buckets"], 24)
}

func TestOverviewCountsBucketsAndDays(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	gid := h.mkGroup(t)
	_, greenBearer := enrollInto(t, h, gid, "laptop-1")
	redID, redBearer := enrollInto(t, h, gid, "media-nuc")
	_, quietBearer := enrollInto(t, h, gid, "never-ran")

	now := time.Now().UTC()
	report(t, h, greenBearer, "g1", "snapshot", now.Add(-30*time.Minute), "ok", "")
	report(t, h, greenBearer, "g2", "snapshot", now.Add(-3*time.Hour), "ok", "")
	report(t, h, greenBearer, "g3", "snapshot", now.Add(-3*24*time.Hour), "ok", "")
	report(t, h, redBearer, "r1", "snapshot", now.Add(-2*time.Hour), "error", "kopia: error: unable to write blob")
	// A command ack backs nothing up: it must not reach the timeline, the day
	// strips or the health of the agent that sent it.
	report(t, h, quietBearer, "c1", "command", now.Add(-time.Hour), "ok", "")

	_, body := h.do("GET", "/api/v1/fleet/overview", nil)

	counts := body["counts"].(map[string]any)
	require.Equal(t, float64(3), counts["agents"])
	require.Equal(t, float64(1), counts["green"])
	require.Equal(t, float64(0), counts["yellow"])
	require.Equal(t, float64(1), counts["red"])
	require.Equal(t, float64(1), counts["unknown"])
	require.Equal(t, float64(1), counts["targets"])

	last24h := body["last24h"].(map[string]any)
	require.Equal(t, float64(2), last24h["completed"])
	require.Equal(t, float64(1), last24h["failed"])
	buckets := last24h["buckets"].([]any)
	require.Len(t, buckets, 24)
	var ok, failed float64
	var prev time.Time
	for i, b := range buckets {
		m := b.(map[string]any)
		ok += m["ok"].(float64)
		failed += m["failed"].(float64)
		hour, err := time.Parse(time.RFC3339, m["hour"].(string))
		require.NoError(t, err)
		require.Equal(t, hour.Truncate(time.Hour), hour, "buckets start on the hour")
		if i > 0 {
			require.Equal(t, time.Hour, hour.Sub(prev), "buckets are consecutive hours")
		}
		prev = hour
	}
	require.Equal(t, float64(2), ok)
	require.Equal(t, float64(1), failed)
	require.Equal(t, now.Truncate(time.Hour), prev.UTC(), "last bucket is the current hour")

	fail := body["latest_failure"].(map[string]any)
	require.Equal(t, redID, fail["agent_id"])
	require.Equal(t, "media-nuc", fail["name"])
	require.Contains(t, fail["stderr"], "unable to write blob")

	devices := body["devices"].([]any)
	require.Len(t, devices, 3)
	first := devices[0].(map[string]any)
	require.Equal(t, "laptop-1", first["name"])
	require.Equal(t, "Laptops", first["group"])
	require.Equal(t, "green", first["health"])
	require.True(t, strings.HasSuffix(first["last"].(string), "ago"), "server formats the relative time")
	days := first["days"].([]any)
	require.Len(t, days, 30)
	require.Equal(t, "good", days[29], "a snapshot finished today")
	require.Equal(t, "good", days[26], "and one three days ago")
	require.Equal(t, "none", days[0])

	second := devices[1].(map[string]any)
	require.Equal(t, "red", second["health"])
	require.Equal(t, "bad", second["days"].([]any)[29], "errors only that day")
	require.Equal(t, "never", second["last"], "never had a good snapshot")

	third := devices[2].(map[string]any)
	require.Equal(t, "unknown", third["health"], "a command ack is not a backup")
	require.Equal(t, "none", third["days"].([]any)[29])

	// A revoked device is no longer part of "protected right now".
	resp, _ := h.do("POST", "/api/v1/fleet/agents/"+redID+"/revoke", nil)
	require.Equal(t, 204, resp.StatusCode)
	_, body = h.do("GET", "/api/v1/fleet/overview", nil)
	require.Equal(t, float64(2), body["counts"].(map[string]any)["agents"])
	require.Len(t, body["devices"], 2)
	require.Nil(t, body["latest_failure"], "its failure goes with it")
}
