package api_test

import (
	"io"
	"net/http"
	"net/url"
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
	res, err := http.Get(h.srv.URL + "/enroll.sh")
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, 200, res.StatusCode)
	require.Contains(t, res.Header.Get("Content-Type"), "text/x-shellscript")
	raw, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	script := string(raw)
	require.Contains(t, script, "warphold agent enroll")
	// The token reaches the binary through the environment, never its argv,
	// so it is not visible in "ps" while the enroll runs.
	require.Contains(t, script, `WARPHOLD_ENROLL_TOKEN="$TOKEN" "$BIN/warphold" agent enroll --server "$SERVER" --scope "$SCOPE"`)
	require.NotContains(t, script, `--token "$TOKEN"`, "the token must not be passed in the child's argv")
	// The script itself accepts the token from either the environment or the
	// --token argument, the latter kept for compatibility.
	require.Contains(t, script, `TOKEN="${WARPHOLD_ENROLL_TOKEN:-}"`)
	require.Contains(t, script, "--token) TOKEN=")
	// The script is static: the operator supplies the token at run time, so
	// no token material is ever in the body.
	require.NotContains(t, script, "wh_", "the served script must not carry an enrollment token")
	require.Contains(t, script, "usage: WARPHOLD_ENROLL_TOKEN=", "no token means the usage message, not a silent enroll")
}

// TestEnrollShIgnoresQueryToken pins the fix for a token served back in the
// response: `?token=` is not read at all any more, so a token in the URL
// neither lands in the body (where `sh -s` echoes it into terminals and CI
// logs) nor reaches the shell template as an injection vector.
func TestEnrollShIgnoresQueryToken(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()

	for _, tok := range []string{wellFormedEnrollToken, `wh_abc"; echo pwned`} {
		res, err := http.Get(h.srv.URL + "/enroll.sh?token=" + url.QueryEscape(tok))
		require.NoError(t, err)
		raw, err := io.ReadAll(res.Body)
		res.Body.Close()
		require.NoError(t, err)
		require.Equal(t, 200, res.StatusCode)
		require.NotContains(t, string(raw), tok, "query token must not appear in the served script")
		require.NotContains(t, string(raw), "pwned")
	}
}

func TestEnrollShRejectsUnsafeHost(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	req, err := http.NewRequest(http.MethodGet, h.srv.URL+"/enroll.sh", nil)
	require.NoError(t, err)
	req.Host = "evil.example; echo pwned"
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, 400, res.StatusCode)
}
