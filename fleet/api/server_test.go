package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/api"
	"github.com/kopia/kopia/fleet/b2api"
)

// jsonNum formats a float64 (as decoded from JSON) back into an integer path segment.
func jsonNum(f float64) string { return strconv.FormatInt(int64(f), 10) }

// jsonBody marshals v into a JSON request body.
func jsonBody(v any) io.Reader {
	b, _ := json.Marshal(v)
	return bytes.NewReader(b)
}

type harness struct {
	t   *testing.T
	srv *httptest.Server
	s   *api.Server
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
	return &harness{t: t, srv: ts, s: s}
}

type fakeB2API struct {
	lock    bool
	created []b2api.KeyRequest
	deleted []string
}

func (f fakeB2API) BucketInfo(_ context.Context, _, _, _ string) (b2api.BucketInfo, error) {
	return b2api.BucketInfo{ID: "bkt1", ObjectLockEnabled: f.lock}, nil
}

func (f fakeB2API) CreateKey(_ context.Context, _, _ string, r b2api.KeyRequest) (b2api.CreatedKey, error) {
	return b2api.CreatedKey{KeyID: "kid-" + r.Name, Key: "sec-" + r.Name}, nil
}

func (f fakeB2API) DeleteKey(_ context.Context, _, _, _ string) error { return nil }

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

func (h *harness) doList(method, path string) (*http.Response, []map[string]any) {
	h.t.Helper()
	req, _ := http.NewRequest(method, h.srv.URL+path, nil)
	for _, c := range h.jar {
		req.AddCookie(c)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(h.t, err)
	defer resp.Body.Close()
	var out []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func (h *harness) mkGroup(t *testing.T) float64 {
	t.Helper()
	_, tg := h.do("POST", "/api/v1/fleet/targets", map[string]any{"name": "local", "kind": "filesystem", "path": t.TempDir()})
	_, tp := h.do("POST", "/api/v1/fleet/templates", map[string]any{"name": "Home default", "sources": []string{"~"}, "policy": map[string]any{}})
	_, g := h.do("POST", "/api/v1/fleet/groups", map[string]any{"name": "Laptops", "target_id": tg["id"], "template_id": tp["id"]})
	return g["id"].(float64)
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

// TestActivateAllowsLoopback confirms the setup-token gate does not break the
// common case: a request from loopback (as httptest.Server serves) may
// activate without any header.
func TestActivateAllowsLoopback(t *testing.T) {
	h := newHarness(t)
	resp, _ := h.do("POST", "/api/v1/fleet/activate", map[string]string{"passphrase": "seal-me!", "email": "hody@hody.dev", "password": "pw12345678"})
	require.Equal(t, 201, resp.StatusCode)
}

// TestActivateRequiresLoopbackOrToken drives the mux directly (rather than
// through a real TCP httptest.Server) so RemoteAddr can be forged to a
// non-loopback address, and checks the setup-token file bridges the gate.
func TestActivateRequiresLoopbackOrToken(t *testing.T) {
	dir := t.TempDir()
	s := api.New(dir)
	t.Cleanup(func() { s.Close() })
	m := mux.NewRouter()
	s.Mount(m)

	body := func() *bytes.Buffer {
		b, _ := json.Marshal(map[string]string{"passphrase": "seal-me!", "email": "hody@hody.dev", "password": "pw12345678"})
		return bytes.NewBuffer(b)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/fleet/activate", body())
	req.RemoteAddr = "203.0.113.5:1234"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	m.ServeHTTP(rr, req)
	require.Equal(t, 403, rr.Code)

	tokBytes, err := os.ReadFile(filepath.Join(dir, "setup-token"))
	require.NoError(t, err)
	tok := strings.TrimSpace(string(tokBytes))

	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/fleet/activate", body())
	req2.RemoteAddr = "203.0.113.5:1234"
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-WarpHold-Setup-Token", tok)
	rr2 := httptest.NewRecorder()
	m.ServeHTTP(rr2, req2)
	require.Equal(t, 201, rr2.Code)
}

// TestActivateIsExclusive fires Activate concurrently and checks the
// activateMu-guarded check-then-write leaves exactly one winner.
func TestActivateIsExclusive(t *testing.T) {
	dir := t.TempDir()
	s := api.New(dir)
	t.Cleanup(func() { s.Close() })

	const n = 5
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = s.Activate(context.Background(), "seal-me!", fmt.Sprintf("admin%d@hody.dev", i), "pw12345678")
		}(i)
	}
	wg.Wait()

	var successes, conflicts int
	for _, err := range errs {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, api.ErrAlreadyActivated):
			conflicts++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	require.Equal(t, 1, successes)
	require.Equal(t, n-1, conflicts)

	admins, err := s.AdminsForTesting(context.Background())
	require.NoError(t, err)
	require.Len(t, admins, 1)
}
