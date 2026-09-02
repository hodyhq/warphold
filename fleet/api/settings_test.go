package api_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSettingsRequiresAdminAndRoundTrips(t *testing.T) {
	h := newHarness(t)
	resp, _ := h.do("GET", "/api/v1/fleet/settings", nil)
	require.Equal(t, 409, resp.StatusCode, "not activated")

	h.activateAndLogin()
	saved := h.jar
	h.jar = nil
	resp, _ = h.do("GET", "/api/v1/fleet/settings", nil)
	require.Equal(t, 401, resp.StatusCode, "settings are admin-only")
	h.jar = saved

	resp, body := h.do("GET", "/api/v1/fleet/settings", nil)
	require.Equal(t, 200, resp.StatusCode)
	require.Equal(t, "", body["fleet_name"], "no fleet name set yet")
	require.Equal(t, float64(300), body["poll_interval"], "the agent default")

	// A partial write leaves the key it does not mention alone.
	resp, body = h.do("PUT", "/api/v1/fleet/settings", map[string]any{"fleet_name": "  Moinzadeh  "})
	require.Equal(t, 200, resp.StatusCode)
	require.Equal(t, "Moinzadeh", body["fleet_name"], "trimmed")
	require.Equal(t, float64(300), body["poll_interval"])

	resp, body = h.do("PUT", "/api/v1/fleet/settings", map[string]any{"poll_interval": 900})
	require.Equal(t, 200, resp.StatusCode)
	require.Equal(t, "Moinzadeh", body["fleet_name"])
	require.Equal(t, float64(900), body["poll_interval"])

	_, body = h.do("GET", "/api/v1/fleet/settings", nil)
	require.Equal(t, "Moinzadeh", body["fleet_name"])
	require.Equal(t, float64(900), body["poll_interval"])

	// The overview header reads the same setting.
	_, body = h.do("GET", "/api/v1/fleet/overview", nil)
	require.Equal(t, "Moinzadeh", body["fleet_name"])
}

// The settings table also holds seal_salt, which must never be readable or
// writable over HTTP: the whole escrow rests on it.
func TestSettingsRejectsUnknownKeysAndBadValues(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()

	for name, in := range map[string]map[string]any{
		"unknown key":       {"seal_salt": "deadbeef"},
		"poll below range":  {"poll_interval": 5},
		"poll above range":  {"poll_interval": 4000},
		"poll not a number": {"poll_interval": "soon"},
		"name not a string": {"fleet_name": 7},
		"name too long":     {"fleet_name": strings.Repeat("x", 65)},
	} {
		t.Run(name, func(t *testing.T) {
			resp, body := h.do("PUT", "/api/v1/fleet/settings", in)
			require.Equal(t, 400, resp.StatusCode)
			require.NotEmpty(t, body["error"])
		})
	}

	resp, body := h.do("GET", "/api/v1/fleet/settings", nil)
	require.Equal(t, 200, resp.StatusCode)
	require.Len(t, body, 2, "only the whitelisted keys are exposed")
	require.NotContains(t, body, "seal_salt")
}
