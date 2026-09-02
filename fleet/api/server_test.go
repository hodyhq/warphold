package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/api"
)

type harness struct {
	t   *testing.T
	srv *httptest.Server
	jar []*http.Cookie
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	s := api.New(t.TempDir())
	t.Cleanup(func() { s.Close() })
	m := mux.NewRouter()
	s.Mount(m)
	ts := httptest.NewServer(m)
	t.Cleanup(ts.Close)
	return &harness{t: t, srv: ts}
}

func (h *harness) do(method, path string, body any) (*http.Response, map[string]any) {
	h.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(h.t, json.NewEncoder(&buf).Encode(body))
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, &buf)
	req.Header.Set("Content-Type", "application/json")
	for _, c := range h.jar {
		req.AddCookie(c)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(h.t, err)
	defer resp.Body.Close()
	if cs := resp.Cookies(); len(cs) > 0 {
		h.jar = cs
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func (h *harness) activateAndLogin() {
	h.t.Helper()
	resp, _ := h.do("POST", "/api/v1/fleet/activate", map[string]string{"passphrase": "seal-me!", "email": "hody@hody.dev", "password": "pw12345678"})
	require.Equal(h.t, 201, resp.StatusCode)
	resp, _ = h.do("POST", "/api/v1/fleet/session", map[string]string{"email": "hody@hody.dev", "password": "pw12345678"})
	require.Equal(h.t, 204, resp.StatusCode)
}

func TestStatusActivateLogin(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do("GET", "/api/v1/fleet/status", nil)
	require.Equal(t, 200, resp.StatusCode)
	require.Equal(t, false, body["activated"])

	resp, _ = h.do("POST", "/api/v1/fleet/session", map[string]string{"email": "x", "password": "y"})
	require.Equal(t, 409, resp.StatusCode, "cannot log in before activation")

	h.activateAndLogin()
	resp, body = h.do("GET", "/api/v1/fleet/status", nil)
	require.Equal(t, true, body["activated"])

	resp, _ = h.do("POST", "/api/v1/fleet/activate", map[string]string{"passphrase": "again", "email": "a@b", "password": "pw12345678"})
	require.Equal(t, 409, resp.StatusCode)

	resp, _ = h.do("DELETE", "/api/v1/fleet/session", nil)
	require.Equal(t, 204, resp.StatusCode)
}

func TestLoginRateLimitAndBadPassword(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	h.jar = nil
	for i := 0; i < 5; i++ {
		resp, _ := h.do("POST", "/api/v1/fleet/session", map[string]string{"email": "hody@hody.dev", "password": "wrong"})
		require.Equal(t, 401, resp.StatusCode)
	}
	resp, _ := h.do("POST", "/api/v1/fleet/session", map[string]string{"email": "hody@hody.dev", "password": "pw12345678"})
	require.Equal(t, 429, resp.StatusCode)
}

func TestActivationSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	s := api.New(dir)
	m := mux.NewRouter()
	s.Mount(m)
	ts := httptest.NewServer(m)
	h := &harness{t: t, srv: ts}
	h.activateAndLogin()
	ts.Close()
	require.NoError(t, s.Close())

	s2 := api.New(dir)
	defer s2.Close()
	require.True(t, s2.Activated(), "key file + db must reopen without the passphrase")
}
