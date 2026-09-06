package api_test

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/jobs"
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
	require.Equal(t, "", body["public_url"], "no public URL set yet")
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
		"unknown key":           {"seal_salt": "deadbeef"},
		"poll below range":      {"poll_interval": 5},
		"poll above range":      {"poll_interval": 4000},
		"poll not a number":     {"poll_interval": "soon"},
		"name not a string":     {"fleet_name": 7},
		"name too long":         {"fleet_name": strings.Repeat("x", 65)},
		"public_url relative":   {"public_url": "fleet.example.com"},
		"public_url not http":   {"public_url": "ftp://fleet.example.com"},
		"public_url plain http": {"public_url": "http://fleet.example.com"},
		"public_url with path":  {"public_url": "https://fleet.example.com/fleet"},
		"public_url with query": {"public_url": "https://fleet.example.com?x=1"},
		"public_url with creds": {"public_url": "https://u:p@fleet.example.com"},
		"public_url not string": {"public_url": 7},
		"verify not a bool":     {"public_url": "https://fleet.example.com", "verify": "yes"},
	} {
		t.Run(name, func(t *testing.T) {
			resp, body := h.do("PUT", "/api/v1/fleet/settings", in)
			require.Equal(t, 400, resp.StatusCode)
			require.NotEmpty(t, body["error"])
		})
	}

	resp, body := h.do("GET", "/api/v1/fleet/settings", nil)
	require.Equal(t, 200, resp.StatusCode)
	require.ElementsMatch(t,
		[]string{"fleet_name", "poll_interval", "public_url", "revoked_retention_days",
			"trusted_proxies", "gateway_ip_rate", "gateway_ip_burst", "gateway_device_rate",
			"gateway_device_burst",
			"smtp_host", "smtp_port", "smtp_username", "smtp_from", "smtp_tls", "smtp_password_set",
			"job_intervals"},
		slices.Collect(maps.Keys(body)), "only the whitelisted keys are exposed")
	require.NotContains(t, body, "seal_salt")
}

// The Settings screen's Background-jobs card writes the scheduler's own
// cadence settings. They used to fall through to "unknown setting" and 400,
// so the card could show a cadence it could never change. Table-driven off
// jobs.IntervalSettings(), so a job kind added to the scheduler is covered
// here the day it is added rather than the day someone remembers.
func TestJobIntervalSettingsRoundTrip(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()

	_, body := h.do("GET", "/api/v1/fleet/settings", nil)
	shown, ok := body["job_intervals"].(map[string]any)
	require.True(t, ok, "job_intervals is an object")

	specs := jobs.IntervalSettings()
	require.NotEmpty(t, specs)
	require.Len(t, shown, len(specs), "every scheduled kind's cadence is exposed")

	for key, spec := range specs {
		t.Run(key, func(t *testing.T) {
			require.Equal(t, float64(spec.DefaultSeconds), shown[key], "unset reads as the default")

			// The floor is accepted, and read back as written.
			resp, body := h.do("PUT", "/api/v1/fleet/settings", map[string]any{key: spec.MinSeconds})
			require.Equal(t, 200, resp.StatusCode, body["error"])
			require.Equal(t, float64(spec.MinSeconds),
				body["job_intervals"].(map[string]any)[key], "the PUT response reflects it")

			_, body = h.do("GET", "/api/v1/fleet/settings", nil)
			require.Equal(t, float64(spec.MinSeconds),
				body["job_intervals"].(map[string]any)[key], "and so does a fresh GET")

			// Below the scheduler's floor is refused rather than silently
			// clamped: the number the screen shows must be the number that runs.
			resp, body = h.do("PUT", "/api/v1/fleet/settings", map[string]any{key: spec.MinSeconds - 1})
			require.Equal(t, 400, resp.StatusCode)
			require.Contains(t, body["error"], key)

			resp, _ = h.do("PUT", "/api/v1/fleet/settings", map[string]any{key: spec.MaxSeconds + 1})
			require.Equal(t, 400, resp.StatusCode, "an absurd cadence would overflow the scheduler's Duration")

			resp, _ = h.do("PUT", "/api/v1/fleet/settings", map[string]any{key: "hourly"})
			require.Equal(t, 400, resp.StatusCode, "not a number")

			// The refusals changed nothing.
			_, body = h.do("GET", "/api/v1/fleet/settings", nil)
			require.Equal(t, float64(spec.MinSeconds), body["job_intervals"].(map[string]any)[key])
		})
	}
}
