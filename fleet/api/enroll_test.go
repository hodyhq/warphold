package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/repo"
)

func TestEnrollHappyPathAndRevoke(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	gid := h.mkGroup(t)
	_, tok := h.do("POST", "/api/v1/fleet/tokens", map[string]any{"group_id": gid})

	admin := h.jar
	h.jar = nil // enrollment needs no session
	resp, body := h.do("POST", "/api/v1/fleet/enroll", map[string]any{"token": tok["token"], "hostname": "fw13", "os": "linux", "arch": "amd64", "version": "0.1.0", "scope": "user"})
	require.Equal(t, 201, resp.StatusCode, body)
	require.True(t, strings.HasPrefix(body["agent_id"].(string), "ag_"))
	require.True(t, strings.HasPrefix(body["bearer"].(string), "wa_"))
	require.Equal(t, "fw13", body["name"])
	_, pw, err := repo.DecodeToken(body["connect_token"].(string))
	require.NoError(t, err)
	require.Len(t, pw, 43)
	require.Equal(t, float64(300), body["poll_interval_seconds"])

	resp, _ = h.do("POST", "/api/v1/fleet/enroll", map[string]any{"token": tok["token"], "hostname": "again", "os": "linux", "arch": "amd64", "scope": "user"})
	require.Equal(t, 403, resp.StatusCode, "single-use token")

	h.jar = admin
	resp, list := h.doList("GET", "/api/v1/fleet/agents")
	require.Equal(t, 200, resp.StatusCode)
	require.Len(t, list, 1)
	_, leaks := list[0]["connect_token"]
	require.False(t, leaks)
	id := list[0]["id"].(string)

	resp, _ = h.do("POST", "/api/v1/fleet/agents/"+id+"/commands", map[string]any{"kind": "snapshot-now", "source": "~"})
	require.Equal(t, 201, resp.StatusCode)
	resp, _ = h.do("POST", "/api/v1/fleet/agents/"+id+"/commands", map[string]any{"kind": "rm-rf"})
	require.Equal(t, 400, resp.StatusCode)

	resp, _ = h.do("POST", "/api/v1/fleet/agents/"+id+"/revoke", nil)
	require.Equal(t, 204, resp.StatusCode)
	_, detail := h.do("GET", "/api/v1/fleet/agents/"+id, nil)
	require.NotNil(t, detail["revoked_at"])
}

const wellFormedEnrollToken = "wh_deadbeefdeadbeefdead1234"

func TestEnrollShIsServed(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	res, err := http.Get(h.srv.URL + "/enroll.sh?token=" + wellFormedEnrollToken)
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, 200, res.StatusCode)
	require.Contains(t, res.Header.Get("Content-Type"), "text/x-shellscript")
	buf := make([]byte, 4096)
	n, _ := res.Body.Read(buf)
	require.Contains(t, string(buf[:n]), "warphold agent enroll")
	require.Contains(t, string(buf[:n]), "--token "+wellFormedEnrollToken)
}

func TestEnrollShRejectsUnsafeToken(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	res, err := http.Get(h.srv.URL + "/enroll.sh?token=wh_abc%22%3B%20echo%20pwned")
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, 400, res.StatusCode)
}

func TestEnrollShRejectsUnsafeHost(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	req, err := http.NewRequest(http.MethodGet, h.srv.URL+"/enroll.sh?token="+wellFormedEnrollToken, nil)
	require.NoError(t, err)
	req.Host = "evil.example; echo pwned"
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, 400, res.StatusCode)
}
