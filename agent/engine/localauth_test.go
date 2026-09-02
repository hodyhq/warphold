package engine_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/agent/engine"
	"github.com/kopia/kopia/internal/apiclient"
	"github.com/kopia/kopia/internal/serverapi"
)

// noRedirect keeps the 302 from /local/session visible to the test.
func noRedirect() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func doGet(t *testing.T, cl *http.Client, url string, cookies ...*http.Cookie) *http.Response {
	t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	require.NoError(t, err)

	for _, c := range cookies {
		req.AddCookie(c)
	}

	resp, err := cl.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { resp.Body.Close() }) //nolint:errcheck

	return resp
}

// doGetWithHeaders is doGet plus request headers, for the browser-shaped
// requests the same-origin guard keys on.
func doGetWithHeaders(t *testing.T, cl *http.Client, url string, hdr map[string]string, cookies ...*http.Cookie) *http.Response {
	t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	require.NoError(t, err)

	for _, c := range cookies {
		req.AddCookie(c)
	}

	for k, v := range hdr {
		req.Header.Set(k, v)
	}

	resp, err := cl.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { resp.Body.Close() }) //nolint:errcheck

	return resp
}

// TestLocalSessionHandoff pins the tray's path into the engine's UI: a
// one-URL handoff on loopback that swaps a process-lifetime token for a
// cookie, which the middleware turns into the basic auth Kopia's own
// handlers require.
func TestLocalSessionHandoff(t *testing.T) {
	ctx := context.Background()
	stateDir := t.TempDir()
	t.Setenv("WARPHOLD_STATE_DIR", stateDir)

	cfg, pw := provisionedRepo(t)

	h, err := engine.StartHeadless(ctx, cfg, pw, "user")
	require.NoError(t, err)

	stopped := false

	defer func() {
		if !stopped {
			h.Stop(ctx) //nolint:errcheck
		}
	}()

	info, err := engine.ReadInfo("user")
	require.NoError(t, err)
	require.Equal(t, h.BaseURL, info.BaseURL)
	require.Equal(t, os.Getpid(), info.PID)
	require.NotEmpty(t, info.LocalToken)
	require.False(t, info.StartedAt.IsZero())

	cl := noRedirect()

	// no credentials at all: Kopia's own handler rejects it.
	require.Equal(t, http.StatusUnauthorized, doGet(t, cl, h.BaseURL+"/api/v1/sources").StatusCode)

	// wrong token: no cookie handed out.
	bad := doGet(t, cl, h.BaseURL+"/local/session?t=wrong")
	require.Equal(t, http.StatusForbidden, bad.StatusCode)
	require.Empty(t, bad.Cookies())

	// empty token: an engine with an empty token would otherwise hand out a
	// session to anyone who can reach the port.
	require.Equal(t, http.StatusForbidden, doGet(t, cl, h.BaseURL+"/local/session?t=").StatusCode)

	// right token: 302 to the UI with the session cookie set.
	ok := doGet(t, cl, h.BaseURL+"/local/session?t="+info.LocalToken)
	require.Equal(t, http.StatusFound, ok.StatusCode)
	require.Equal(t, "/", ok.Header.Get("Location"))

	var c *http.Cookie

	for _, ck := range ok.Cookies() {
		if ck.Name == "wh_local" {
			c = ck
		}
	}

	require.NotNil(t, c, "wh_local cookie")
	require.True(t, c.HttpOnly)
	require.Equal(t, http.SameSiteStrictMode, c.SameSite)
	require.Equal(t, "/", c.Path)
	require.NotEqual(t, info.LocalToken, c.Value, "the cookie is not the token itself")

	// the cookie now authenticates API calls.
	require.Equal(t, http.StatusOK, doGet(t, cl, h.BaseURL+"/api/v1/sources", c).StatusCode)

	// a present Authorization header always wins, so a valid cookie cannot
	// elevate a request that authenticated as somebody else.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.BaseURL+"/api/v1/sources", nil)
	require.NoError(t, err)
	req.AddCookie(c)
	req.SetBasicAuth("someone", "else")

	elevated, err := cl.Do(req)
	require.NoError(t, err)

	defer elevated.Body.Close() //nolint:errcheck

	require.Equal(t, http.StatusUnauthorized, elevated.StatusCode)

	// SameSite=Strict ignores ports, so a page on another 127.0.0.1 port is
	// same-site and its fetches carry wh_local. Only the fetch-metadata and
	// Origin headers separate it from the engine's own UI.
	require.Equal(t, http.StatusUnauthorized,
		doGetWithHeaders(t, cl, h.BaseURL+"/api/v1/sources", map[string]string{"Sec-Fetch-Site": "cross-site"}, c).StatusCode,
		"a cross-site fetch cannot ride the cookie")
	require.Equal(t, http.StatusUnauthorized,
		doGetWithHeaders(t, cl, h.BaseURL+"/api/v1/sources", map[string]string{"Origin": "http://127.0.0.1:9999"}, c).StatusCode,
		"another loopback port is a different origin")
	require.Equal(t, http.StatusOK,
		doGetWithHeaders(t, cl, h.BaseURL+"/api/v1/sources", map[string]string{"Sec-Fetch-Site": "same-origin"}, c).StatusCode)
	require.Equal(t, http.StatusOK,
		doGetWithHeaders(t, cl, h.BaseURL+"/api/v1/sources", map[string]string{"Origin": h.BaseURL}, c).StatusCode,
		"the engine's own origin passes")

	// Neither header: a non-browser caller (curl, the tray). Deliberately
	// allowed - it is not the threat the guard exists for, and such a caller
	// can read engine.json's password anyway. This is the doGet call above.

	// a forged cookie does not.
	require.Equal(t, http.StatusUnauthorized,
		doGet(t, cl, h.BaseURL+"/api/v1/sources", &http.Cookie{Name: "wh_local", Value: "nope"}).StatusCode)

	// engine.json's own credentials are what 'warphold agent status' uses.
	api, err := apiclient.NewKopiaAPIClient(apiclient.Options{BaseURL: info.BaseURL, Username: info.User, Password: info.Password})
	require.NoError(t, err)

	var sr serverapi.SourcesResponse

	require.NoError(t, api.Get(ctx, "sources", nil, &sr))

	require.NoError(t, h.Stop(ctx))

	stopped = true

	require.NoFileExists(t, filepath.Join(stateDir, "engine.json"), "Stop removes the info file")
}
