package engine_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/agent/engine"
	"github.com/kopia/kopia/agent/state"
	"github.com/kopia/kopia/internal/passwordpersist"
)

// startUnconfiguredApp starts the engine the standalone app runs on its first
// ever launch: an app-scope state directory with no repository in it at all.
func startUnconfiguredApp(t *testing.T) (*engine.Headless, string) {
	t.Helper()

	cfgHome := t.TempDir()
	t.Setenv("WARPHOLD_STATE_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", cfgHome)

	ctx := context.Background()
	stateDir := filepath.Join(cfgHome, "warphold", "app")
	require.Equal(t, stateDir, state.Dir(state.ScopeApp))

	h, err := engine.StartHeadless(ctx, state.RepoConfigPath(state.ScopeApp), "", state.ScopeApp, passwordpersist.File())
	require.NoError(t, err)

	t.Cleanup(func() { h.Stop(ctx) }) //nolint:errcheck

	return h, stateDir
}

// TestAppEngineStartsWithoutARepository pins the first-run path: an app whose
// repository does not exist yet must still come up and serve the UI, because
// the UI's setup wizard is what creates that repository. An engine that
// refused to start here would leave the installed service crash-looping with
// no way for the user to configure anything.
func TestAppEngineStartsWithoutARepository(t *testing.T) {
	h, stateDir := startUnconfiguredApp(t)

	require.NoFileExists(t, state.RepoConfigPath(state.ScopeApp))

	// engine.json is written where the app scope says, holds the loopback
	// port and the local token, and is readable by nobody else.
	info, err := engine.ReadInfo(state.ScopeApp)
	require.NoError(t, err)
	require.Equal(t, h.BaseURL, info.BaseURL)
	require.True(t, strings.HasPrefix(info.BaseURL, "http://127.0.0.1:"), info.BaseURL)
	require.NotEmpty(t, info.LocalToken)
	require.Equal(t, os.Getpid(), info.PID)

	st, err := os.Stat(filepath.Join(stateDir, "engine.json"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), st.Mode().Perm())

	// The API answers - as "not connected", not as a failure.
	require.Equal(t, http.StatusOK, get(t, h, "/api/v1/repo/status"))
}

// TestAppStateDirOverrideKeepsTheAppSeparate pins that an explicit
// WARPHOLD_STATE_DIR - which the installed unit bakes in - still puts the app
// one level down, so an agent and an app pointed at one directory do not end
// up sharing a repository.
func TestAppStateDirOverrideKeepsTheAppSeparate(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WARPHOLD_STATE_DIR", dir)

	require.Equal(t, dir, state.Dir(state.ScopeUser))
	require.Equal(t, filepath.Join(dir, "app"), state.Dir(state.ScopeApp))
	require.Equal(t, filepath.Join(dir, "app", "cache"), state.CacheDir(state.ScopeApp))
}

// TestAgentEngineRefusesAMissingRepository pins that the first-run tolerance
// above is the app scope's alone: an enrolled agent connected its repository
// at enrollment, so a missing repository.config there is a real failure and
// must be reported rather than becoming an engine that quietly backs nothing
// up.
func TestAgentEngineRefusesAMissingRepository(t *testing.T) {
	t.Setenv("WARPHOLD_STATE_DIR", t.TempDir())

	h, err := engine.StartHeadless(context.Background(), state.RepoConfigPath(state.ScopeUser), "", state.ScopeUser, passwordpersist.None())
	if h != nil {
		defer h.Stop(context.Background()) //nolint:errcheck
	}

	require.Error(t, err)
}

// TestAppEngineServesSoloUI pins what the SPA sees in solo mode: the bundle
// is served, and there are no Fleet routes, which is how the UI decides it is
// a single machine rather than a Fleet server.
func TestAppEngineServesSoloUI(t *testing.T) {
	h, _ := startUnconfiguredApp(t)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, h.BaseURL+"/", nil)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	defer resp.Body.Close() //nolint:errcheck

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, string(body), "<title>WarpHold")

	require.Equal(t, http.StatusNotFound, get(t, h, "/api/v1/fleet/status"))
}

// TestAppEngineSessionHandoff pins that the tray's and 'app url”s one-URL
// handoff works against an app engine with no repository: the token buys a
// session cookie, and that cookie is what the UI then uses to set the
// repository up.
func TestAppEngineSessionHandoff(t *testing.T) {
	h, _ := startUnconfiguredApp(t)

	u, err := engine.SessionURL(state.ScopeApp)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(u, h.BaseURL+"/local/session?t="), u)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, u, nil)
	require.NoError(t, err)

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	resp, err := client.Do(req)
	require.NoError(t, err)

	defer resp.Body.Close() //nolint:errcheck

	require.Equal(t, http.StatusFound, resp.StatusCode)

	var cookie *http.Cookie

	for _, c := range resp.Cookies() {
		if c.Name == "wh_local" {
			cookie = c
		}
	}

	require.NotNil(t, cookie, "the handoff sets the session cookie")

	// The cookie authenticates an API call the way the browser will.
	apiReq, err := http.NewRequestWithContext(context.Background(), http.MethodGet, h.BaseURL+"/api/v1/repo/status", nil)
	require.NoError(t, err)
	apiReq.AddCookie(cookie)

	api, err := client.Do(apiReq)
	require.NoError(t, err)

	defer api.Body.Close() //nolint:errcheck

	require.Equal(t, http.StatusOK, api.StatusCode)

	// A wrong token buys nothing.
	badReq, err := http.NewRequestWithContext(context.Background(), http.MethodGet, h.BaseURL+"/local/session?t="+url.QueryEscape("nope"), nil)
	require.NoError(t, err)

	bad, err := client.Do(badReq)
	require.NoError(t, err)

	defer bad.Body.Close() //nolint:errcheck

	require.Equal(t, http.StatusForbidden, bad.StatusCode)
}

// get makes an authenticated request and returns its status code.
func get(t *testing.T, h *engine.Headless, path string) int {
	t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, h.BaseURL+path, nil)
	require.NoError(t, err)
	req.SetBasicAuth(h.User, h.Password)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	defer resp.Body.Close() //nolint:errcheck

	return resp.StatusCode
}

// TestAppLocalInfoIsTheHostname pins the label the app's own page shows:
// there is no enrollment to name it, so /local/info answers with this
// machine's hostname - the same label the tray uses, so the two never
// disagree.
func TestAppLocalInfoIsTheHostname(t *testing.T) {
	h, _ := startUnconfiguredApp(t)

	info, err := engine.ReadInfo(state.ScopeApp)
	require.NoError(t, err)

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, h.BaseURL+"/local/session?t="+url.QueryEscape(info.LocalToken), nil)
	require.NoError(t, err)

	session, err := client.Do(req)
	require.NoError(t, err)

	defer session.Body.Close() //nolint:errcheck

	var cookie *http.Cookie

	for _, c := range session.Cookies() {
		if c.Name == "wh_local" {
			cookie = c
		}
	}

	require.NotNil(t, cookie)

	infoReq, err := http.NewRequestWithContext(context.Background(), http.MethodGet, h.BaseURL+"/local/info", nil)
	require.NoError(t, err)
	infoReq.AddCookie(cookie)

	resp, err := client.Do(infoReq)
	require.NoError(t, err)

	defer resp.Body.Close() //nolint:errcheck

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	host, err := os.Hostname()
	require.NoError(t, err)
	require.JSONEq(t, `{"name":"`+host+`","group":""}`, string(body))
}
