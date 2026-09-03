package api_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// statusStub serves whatever body the caller wants at the status endpoint, so
// a test can play "a proxy that is not this Fleet".
func statusStub(t *testing.T, code int, body string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/fleet/status" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	return ts
}

// The whole point of verify=true is that it is not a regex: the server fetches
// its own status through the URL, so a typo, a proxy that swallows the path,
// and a URL that answers for somebody else's Fleet all fail here rather than
// three weeks later during a restore.
func TestPublicURLVerifiesEndToEnd(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()

	// The harness's own server is the real thing, instance id and all.
	resp, body := h.do("PUT", "/api/v1/fleet/settings", map[string]any{"public_url": h.srv.URL, "verify": true})
	require.Equal(t, 200, resp.StatusCode, body)
	require.Equal(t, h.srv.URL, body["public_url"])

	_, body = h.do("GET", "/api/v1/fleet/settings", nil)
	require.Equal(t, h.srv.URL, body["public_url"], "the verified URL is stored")

	stub := statusStub(t, http.StatusOK, `{"activated":true,"instance_id":"someone-elses-fleet"}`)
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://elsewhere.example.com"+r.URL.Path, http.StatusFound)
	}))
	t.Cleanup(redirect.Close)

	for name, tc := range map[string]string{
		"nothing there":     statusStub(t, http.StatusNotFound, `not found`).URL,
		"not activated":     statusStub(t, http.StatusOK, `{"activated":false}`).URL,
		"a different fleet": stub.URL,
		// A proxy that redirects the status endpoint redirects the agent's
		// uploads too, so the device would be told an origin that does not
		// actually answer.
		"a redirect": redirect.URL,
	} {
		t.Run(name, func(t *testing.T) {
			resp, body := h.do("PUT", "/api/v1/fleet/settings", map[string]any{"public_url": tc, "verify": true})
			require.Equal(t, 400, resp.StatusCode)
			require.NotEmpty(t, body["error"])
			reqs := body["proxy_requirements"].([]any)
			require.Len(t, reqs, 5, "the operator gets the proxy checklist, not just a failure")
			require.Contains(t, reqs, "forward the Host header unchanged")
			require.Contains(t, reqs, "forward the full path, including /s3/")
		})
	}

	// The stored value survives every rejected attempt.
	_, body = h.do("GET", "/api/v1/fleet/settings", nil)
	require.Equal(t, h.srv.URL, body["public_url"])

	// Without verify the syntax check alone decides, which is what the CLI
	// and any non-interactive path need.
	resp, body = h.do("PUT", "/api/v1/fleet/settings", map[string]any{"public_url": "https://FLEET.Example.COM"})
	require.Equal(t, 200, resp.StatusCode)
	require.Equal(t, "https://fleet.example.com", body["public_url"], "the host is lowercased")

	// A trailing slash is not a path; it normalizes away.
	_, body = h.do("PUT", "/api/v1/fleet/settings", map[string]any{"public_url": "https://fleet.example.com/"})
	require.Equal(t, "https://fleet.example.com", body["public_url"])
}

// The status response carries an opaque instance id, which is what lets the
// probe above tell "this URL answers" from "this URL is us".
func TestStatusCarriesAStableInstanceID(t *testing.T) {
	h := newHarness(t)
	_, body := h.do("GET", "/api/v1/fleet/status", nil)
	require.NotContains(t, body, "instance_id", "nothing to identify before activation")

	h.activateAndLogin()
	_, body = h.do("GET", "/api/v1/fleet/status", nil)
	id, _ := body["instance_id"].(string)
	require.Len(t, id, 32)

	_, body = h.do("GET", "/api/v1/fleet/status", nil)
	require.Equal(t, id, body["instance_id"], "it is generated once and kept")
}

// Enrollment cannot start until the operator has said where devices should
// reach this Fleet - a token issued now would enrol against the wrong origin.
func TestTokensAndEnrollShAreGatedOnPublicURL(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	gid := h.mkGroup(t)

	resp, body := h.do("POST", "/api/v1/fleet/tokens", map[string]any{"group_id": gid})
	require.Equal(t, 409, resp.StatusCode)
	require.Equal(t, "set the public URL before issuing enrollment tokens", body["error"])

	res, err := http.Get(h.srv.URL + "/enroll.sh")
	require.NoError(t, err)
	res.Body.Close()
	require.Equal(t, 409, res.StatusCode)

	h.setPublicURL()

	resp, _ = h.do("POST", "/api/v1/fleet/tokens", map[string]any{"group_id": gid})
	require.Equal(t, 201, resp.StatusCode)
	res, err = http.Get(h.srv.URL + "/enroll.sh")
	require.NoError(t, err)
	res.Body.Close()
	require.Equal(t, 200, res.StatusCode)
}

// The installer must name the URL the operator published, not whatever the
// Host header says: behind a proxy those differ, and the device keeps the
// value forever.
func TestEnrollShUsesPublicURLNotTheHostHeader(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	_, body := h.do("PUT", "/api/v1/fleet/settings", map[string]any{"public_url": "https://fleet.example.com"})
	require.Equal(t, "https://fleet.example.com", body["public_url"])

	req, err := http.NewRequest(http.MethodGet, h.srv.URL+"/enroll.sh", nil)
	require.NoError(t, err)
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, 200, res.StatusCode)

	raw, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	script := string(raw)
	require.Contains(t, script, `SERVER="https://fleet.example.com"`)
	require.Contains(t, script, `https://fleet.example.com/dl/warphold-linux-$ARCH`, "the binary download follows the same origin")
	require.NotContains(t, script, "127.0.0.1")
}

// Secure follows public_url's scheme, because that is the scheme the browser
// actually used; r.TLS describes only the hop into this process, which behind
// a TLS-terminating proxy is always plain HTTP.
func TestSessionCookieSecureFollowsPublicURL(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()

	secure := func() (login, logout bool) {
		t.Helper()
		cs := h.login("hody@hody.dev", "pw12345678")
		require.Len(t, cs, 2)
		require.Equal(t, cs[0].Secure, cs[1].Secure, "both cookies agree")
		h.jar = cs
		resp, _ := h.do("DELETE", "/api/v1/fleet/session", nil)
		require.Equal(t, 204, resp.StatusCode)
		cleared := resp.Cookies()
		require.Len(t, cleared, 2)
		require.Equal(t, cleared[0].Secure, cleared[1].Secure)
		return cs[0].Secure, cleared[0].Secure
	}

	h.jar = h.login("hody@hody.dev", "pw12345678")
	_, body := h.do("PUT", "/api/v1/fleet/settings", map[string]any{"public_url": "https://fleet.example.com"})
	require.Equal(t, "https://fleet.example.com", body["public_url"])
	set, cleared := secure()
	require.True(t, set, "an https public URL means Secure")
	// A clearing cookie whose attributes differ from the live one is a
	// different cookie to the browser, which then keeps the live one.
	require.True(t, cleared, "the logout cookie must match the one it clears")

	h.jar = h.login("hody@hody.dev", "pw12345678")
	h.setPublicURL() // http, on loopback
	set, cleared = secure()
	require.False(t, set, "plain http must not set Secure")
	require.False(t, cleared)
}

// Once public_url is set, a request arriving under some other name is
// misrouted: answering it would mint links for a host we do not control.
func TestHostValidation(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()

	get := func(host string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, h.srv.URL+"/api/v1/fleet/status", nil)
		require.NoError(t, err)
		if host != "" {
			req.Host = host
		}
		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		res.Body.Close()
		return res.StatusCode
	}

	require.Equal(t, 200, get("anything.example.com"), "no public_url, no check")

	_, body := h.do("PUT", "/api/v1/fleet/settings", map[string]any{"public_url": "https://fleet.example.com"})
	require.Equal(t, "https://fleet.example.com", body["public_url"])

	require.Equal(t, 200, get("fleet.example.com"))
	require.Equal(t, 200, get("fleet.example.com:443"), "a port on the Host header is not a mismatch")
	require.Equal(t, 200, get("FLEET.example.com"), "hosts are case-insensitive")
	require.Equal(t, 200, get(""), "loopback stays reachable for a local curl")
	require.Equal(t, 200, get("localhost:1234"))
	require.Equal(t, http.StatusMisdirectedRequest, get("evil.example.com"))
}

// The setup wizard sets the public URL in the same call that activates, so
// the very first admin session already has one. A malformed URL fails before
// anything is written: activation happens once, and there is no second go.
func TestActivateAcceptsPublicURL(t *testing.T) {
	activate := func(h *harness, pub string) *http.Response {
		t.Helper()
		req, err := http.NewRequest("POST", h.srv.URL+"/api/v1/fleet/activate",
			jsonBody(map[string]string{"passphrase": "seal-me!", "email": "hody@hody.dev", "password": "pw12345678", "public_url": pub}))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-WarpHold-Setup-Token", h.setupToken())
		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		t.Cleanup(func() { res.Body.Close() })
		return res
	}

	h := newHarness(t)
	require.Equal(t, 400, activate(h, "fleet.example.com").StatusCode, "a bad URL must not activate the fleet")
	_, body := h.do("GET", "/api/v1/fleet/status", nil)
	require.Equal(t, false, body["activated"])

	require.Equal(t, 201, activate(h, "https://fleet.example.com").StatusCode)
	h.jar = h.login("hody@hody.dev", "pw12345678")
	_, body = h.do("GET", "/api/v1/fleet/settings", nil)
	require.Equal(t, "https://fleet.example.com", body["public_url"])
}
