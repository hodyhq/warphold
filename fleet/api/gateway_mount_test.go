package api_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/gateway"
)

// The device-facing S3 gateway goes through requireHost like every other Fleet
// route: once public_url is set, a device that resolved a stale or spoofed name
// is misrouted and never reaches the signature check.
func TestGatewayIsHostValidatedAndMounted(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()

	get := func(host string) *http.Response {
		t.Helper()

		req, err := http.NewRequest(http.MethodGet, h.srv.URL+"/"+gateway.BucketName+"/some-device/some-blob", nil) //nolint:noctx
		require.NoError(t, err)

		if host != "" {
			req.Host = host
		}

		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		t.Cleanup(func() { res.Body.Close() }) //nolint:errcheck // test cleanup

		return res
	}

	// Mounted, and unauthenticated is S3's 403 rather than the SPA's 404.
	require.Equal(t, http.StatusForbidden, get("").StatusCode)

	_, body := h.do("PUT", "/api/v1/fleet/settings", map[string]any{"public_url": "https://fleet.example.com"})
	require.Equal(t, "https://fleet.example.com", body["public_url"])

	require.Equal(t, http.StatusForbidden, get("fleet.example.com").StatusCode, "the configured host still reaches the gateway")
	require.Equal(t, http.StatusMisdirectedRequest, get("evil.example.com").StatusCode)

	// The bucket path with no key is the ListObjectsV2 route and is validated too.
	req, err := http.NewRequest(http.MethodGet, h.srv.URL+"/"+gateway.BucketName, nil) //nolint:noctx
	require.NoError(t, err)
	req.Host = "evil.example.com"
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	defer res.Body.Close() //nolint:errcheck // test cleanup

	require.Equal(t, http.StatusMisdirectedRequest, res.StatusCode)
}

// The gateway's limits and its trusted-proxy list are operator settings, not
// constants: they round-trip through the settings API and are range-checked.
func TestGatewayLimitSettings(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()

	_, body := h.do("GET", "/api/v1/fleet/settings", nil)
	require.EqualValues(t, gateway.DefaultIPRatePerSecond, body["gateway_ip_rate"])
	require.EqualValues(t, gateway.DefaultIPRateBurst, body["gateway_ip_burst"])
	require.EqualValues(t, gateway.DefaultRatePerSecond, body["gateway_device_rate"])
	require.EqualValues(t, gateway.DefaultRateBurst, body["gateway_device_burst"])
	require.Equal(t, "", body["trusted_proxies"])

	_, body = h.do("PUT", "/api/v1/fleet/settings", map[string]any{
		"trusted_proxies":     "10.0.0.0/8, 192.168.1.7",
		"gateway_ip_rate":     500,
		"gateway_device_rate": 25,
	})
	require.Equal(t, "10.0.0.0/8, 192.168.1.7", body["trusted_proxies"])
	require.EqualValues(t, 500, body["gateway_ip_rate"])
	require.EqualValues(t, 25, body["gateway_device_rate"])

	for _, bad := range []map[string]any{
		{"trusted_proxies": "10.0.0.0/8, nonsense"},
		{"trusted_proxies": 7},
		{"gateway_ip_rate": 0},
		{"gateway_device_burst": 99_999_999},
		{"gateway_device_rate": "fast"},
	} {
		resp, _ := h.do("PUT", "/api/v1/fleet/settings", bad)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode, "%v", bad)
	}

	// The rejected values did not land.
	_, body = h.do("GET", "/api/v1/fleet/settings", nil)
	require.Equal(t, "10.0.0.0/8, 192.168.1.7", body["trusted_proxies"])
	require.EqualValues(t, 500, body["gateway_ip_rate"])
}
