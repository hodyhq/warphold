package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAdminCRUDRequiresLoginAndRoundTrips(t *testing.T) {
	h := newHarness(t)
	resp, _ := h.do("GET", "/api/v1/fleet/targets", nil)
	require.Equal(t, 409, resp.StatusCode, "not activated")
	h.activateAndLogin()
	saved := h.jar
	h.jar = nil
	resp, _ = h.do("GET", "/api/v1/fleet/targets", nil)
	require.Equal(t, 401, resp.StatusCode)
	h.jar = saved

	resp, body := h.do("POST", "/api/v1/fleet/targets", map[string]any{"name": "local", "kind": "filesystem", "path": t.TempDir()})
	require.Equal(t, 201, resp.StatusCode)
	tid := body["id"].(float64)

	resp, body = h.do("POST", "/api/v1/fleet/templates", map[string]any{"name": "Home default", "sources": []string{"~"}, "policy": map[string]any{"retention": map[string]any{"keepHourly": 24}}})
	require.Equal(t, 201, resp.StatusCode)
	tpl := body["id"].(float64)

	resp, body = h.do("POST", "/api/v1/fleet/groups", map[string]any{"name": "Laptops", "target_id": tid, "template_id": tpl})
	require.Equal(t, 201, resp.StatusCode)

	resp, _ = h.do("POST", "/api/v1/fleet/groups", map[string]any{"name": "Bad", "target_id": 999, "template_id": tpl})
	require.Equal(t, 400, resp.StatusCode, "unknown target")

	res, err := http.DefaultClient.Do(h.newRequest("GET", "/api/v1/fleet/templates", nil))
	require.NoError(t, err)
	var list []map[string]any
	require.NoError(t, json.NewDecoder(res.Body).Decode(&list))
	require.Len(t, list, 1)
	require.Equal(t, "Home default", list[0]["name"])
	require.Equal(t, []any{"~"}, list[0]["sources"])
}

func TestB2TargetVerifiesObjectLock(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	h.s.SetB2ForTesting(fakeB2API{lock: true})
	resp, body := h.do("POST", "/api/v1/fleet/targets", map[string]any{"name": "b2", "kind": "b2", "bucket": "hody-backups", "key_id": "k", "key": "s"})
	require.Equal(t, 201, resp.StatusCode)
	require.Equal(t, true, body["object_lock_verified"])
	resp, list := h.doList("GET", "/api/v1/fleet/targets")
	require.Equal(t, 200, resp.StatusCode)
	_, hasKey := list[0]["key"]
	require.False(t, hasKey, "keys never leave the server")
}

func TestTokensDefaultsAndLimits(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	gid := h.mkGroup(t)

	// The gate: a token is useless without an enrollment URL to go with it.
	resp, body := h.do("POST", "/api/v1/fleet/tokens", map[string]any{"group_id": gid})
	require.Equal(t, 409, resp.StatusCode)
	require.Equal(t, "set the public URL before issuing enrollment tokens", body["error"])

	h.setPublicURL()
	resp, body = h.do("POST", "/api/v1/fleet/tokens", map[string]any{"group_id": gid})
	require.Equal(t, 201, resp.StatusCode)
	require.Equal(t, float64(1), body["max_uses"])
	require.True(t, strings.HasPrefix(body["token"].(string), "wh_"))
	resp, _ = h.do("POST", "/api/v1/fleet/tokens", map[string]any{"group_id": gid, "ttl_seconds": 31 * 86400})
	require.Equal(t, 400, resp.StatusCode)
	resp, _ = h.do("POST", "/api/v1/fleet/tokens", map[string]any{"group_id": gid, "ttl_seconds": 7 * 86400, "max_uses": 0})
	require.Equal(t, 201, resp.StatusCode)
}
