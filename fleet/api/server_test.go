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
	"github.com/kopia/kopia/fleet/gateway"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/s3"
)

// jsonNum formats a float64 (as decoded from JSON) back into an integer path segment.
func jsonNum(f float64) string { return strconv.FormatInt(int64(f), 10) }

// jsonBody marshals v into a JSON request body.
func jsonBody(v any) io.Reader {
	b, _ := json.Marshal(v)
	return bytes.NewReader(b)
}

type harness struct {
	t        *testing.T
	srv      *httptest.Server
	s        *api.Server
	jar      []*http.Cookie
	stateDir string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	s := api.New(dir)
	t.Cleanup(func() { s.Close() })
	m := mux.NewRouter()
	s.Mount(m)
	ts := httptest.NewServer(m)
	t.Cleanup(ts.Close)
	return &harness{t: t, srv: ts, s: s, stateDir: dir}
}

type fakeB2API struct {
	lock bool
	// lockUnreadable is B2 hiding fileLockConfiguration from an under-scoped
	// key; the zero value is a key that can read it.
	lockUnreadable bool
	created        []b2api.KeyRequest
	deleted        []string
}

func (f fakeB2API) BucketInfo(_ context.Context, _, _, _ string) (b2api.BucketInfo, error) {
	return b2api.BucketInfo{ID: "bkt1", ObjectLockEnabled: f.lock, LockReadable: !f.lockUnreadable}, nil
}

func (f fakeB2API) CreateKey(_ context.Context, _, _ string, r b2api.KeyRequest) (b2api.CreatedKey, error) {
	return b2api.CreatedKey{KeyID: "kid-" + r.Name, Key: "sec-" + r.Name}, nil
}

func (f fakeB2API) DeleteKey(_ context.Context, _, _, _ string) error { return nil }

// fakeCloud stands in for an S3-compatible provider. The wire shapes it would
// otherwise exercise are covered against a real fake S3 server in
// fleet/gateway's TestProbeObjectLock and TestProbeConditionalPut; what is
// tested through here is which bucket and prefix the API asks about, and what
// it does with each answer.
type fakeCloud struct {
	lock bool // the bucket has Object Lock enabled
	cond bool // the provider enforces If-None-Match: *

	// condUnsupported is Backblaze B2: the provider refuses the conditional
	// header itself (501), which is a different answer from taking it and
	// overwriting anyway. It wins over cond.
	condUnsupported bool

	mu       sync.Mutex
	asked    []string // "<endpoint>/<bucket>" per Object Lock probe
	prefixes []string // the prefix of each conditional-put probe
	keyIDs   []string // the access key of each conditional-put probe
}

func (f *fakeCloud) ObjectLock(_ context.Context, ci blob.ConnectionInfo) error {
	o := ci.Config.(*s3.Options)

	f.mu.Lock()
	f.asked = append(f.asked, o.Endpoint+"/"+o.BucketName)
	f.mu.Unlock()

	if !f.lock {
		return gateway.ErrNoObjectLock
	}

	return nil
}

func (f *fakeCloud) ConditionalPut(_ context.Context, ci blob.ConnectionInfo, prefix string) error {
	o := ci.Config.(*s3.Options)

	f.mu.Lock()
	f.prefixes = append(f.prefixes, prefix)
	f.keyIDs = append(f.keyIDs, o.AccessKeyID)
	f.mu.Unlock()

	switch {
	case f.condUnsupported:
		return gateway.ErrCondPutNotImplemented
	case !f.cond:
		return gateway.ErrNoConditionalPut
	}

	return nil
}

// lastKeyID is the access key the most recent conditional-put probe used, which
// is how a test sees which credentials a re-verification actually ran with.
func (f *fakeCloud) lastKeyID() string {
	f.mu.Lock()
	defer f.mu.Unlock()

	if len(f.keyIDs) == 0 {
		return ""
	}

	return f.keyIDs[len(f.keyIDs)-1]
}

func (f *fakeCloud) probed() ([]string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.asked...), append([]string(nil), f.prefixes...)
}

// csrfCookieName and csrfHeaderName mirror the server's double-submit pair;
// the harness echoes the cookie back in the header the way the UI must.
const (
	csrfCookieName = "wh_csrf"
	csrfHeaderName = "X-WarpHold-CSRF"
)

// cookie returns the value of one cookie in the jar, or "".
func (h *harness) cookie(name string) string {
	for _, c := range h.jar {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

// mergeCookies applies Set-Cookie headers to the jar by name, the way a
// browser would, so a response that refreshes one cookie does not drop the
// other. It never writes through the old backing array: tests keep a copy of
// the jar to prove a stale cookie is dead.
func (h *harness) mergeCookies(cs []*http.Cookie) {
	if len(cs) == 0 {
		return
	}
	jar := append([]*http.Cookie(nil), h.jar...)
	for _, c := range cs {
		replaced := false
		for i, old := range jar {
			if old.Name == c.Name {
				jar[i], replaced = c, true
				break
			}
		}
		if !replaced {
			jar = append(jar, c)
		}
	}
	h.jar = jar
}

// newRequest builds a request carrying the jar plus the CSRF header read back
// out of the wh_csrf cookie - the double submit every state-changing admin
// call has to make.
func (h *harness) newRequest(method, path string, body io.Reader) *http.Request {
	h.t.Helper()
	req, err := http.NewRequest(method, h.srv.URL+path, body)
	require.NoError(h.t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, c := range h.jar {
		req.AddCookie(c)
	}
	if tok := h.cookie(csrfCookieName); tok != "" {
		req.Header.Set(csrfHeaderName, tok)
	}
	return req
}

func (h *harness) do(method, path string, body any) (*http.Response, map[string]any) {
	h.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(h.t, json.NewEncoder(&buf).Encode(body))
	}
	req := h.newRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(h.t, err)
	defer resp.Body.Close()
	h.mergeCookies(resp.Cookies())
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func (h *harness) doList(method, path string) (*http.Response, []map[string]any) {
	h.t.Helper()
	resp, err := http.DefaultClient.Do(h.newRequest(method, path, nil))
	require.NoError(h.t, err)
	defer resp.Body.Close()
	var out []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

// login signs in with a jar of its own and returns the cookies it got, so a
// test can hold two independent sessions at once.
func (h *harness) login(email, pw string) []*http.Cookie {
	h.t.Helper()
	req, err := http.NewRequest("POST", h.srv.URL+"/api/v1/fleet/session", jsonBody(map[string]string{"email": email, "password": pw}))
	require.NoError(h.t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(h.t, err)
	defer resp.Body.Close()
	require.Equal(h.t, 204, resp.StatusCode)
	return resp.Cookies()
}

func (h *harness) mkGroup(t *testing.T) float64 {
	t.Helper()
	_, tg := h.do("POST", "/api/v1/fleet/targets", map[string]any{"name": "local", "kind": "filesystem", "path": t.TempDir()})
	_, tp := h.do("POST", "/api/v1/fleet/templates", map[string]any{"name": "Home default", "sources": []string{"~"}, "policy": map[string]any{}})
	_, g := h.do("POST", "/api/v1/fleet/groups", map[string]any{"name": "Laptops", "target_id": tg["id"], "template_id": tp["id"]})
	return g["id"].(float64)
}

// setupToken reads the one-time setup token the server wrote to its state
// directory. HTTP activation always requires it (there is no loopback
// exception), so every test that activates over HTTP goes through here.
func (h *harness) setupToken() string {
	h.t.Helper()
	path := h.s.SetupTokenPathForTesting()
	require.NotEmpty(h.t, path, "server has no pending setup token")
	b, err := os.ReadFile(path)
	require.NoError(h.t, err)
	return strings.TrimSpace(string(b))
}

func (h *harness) activateAndLogin() {
	h.t.Helper()
	req, _ := http.NewRequest("POST", h.srv.URL+"/api/v1/fleet/activate", jsonBody(map[string]string{"passphrase": "seal-me!", "email": "hody@hody.dev", "password": "pw12345678"}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-WarpHold-Setup-Token", h.setupToken())
	resp, err := http.DefaultClient.Do(req)
	require.NoError(h.t, err)
	resp.Body.Close()
	require.Equal(h.t, 201, resp.StatusCode)
	h.jar = h.login("hody@hody.dev", "pw12345678")
}

// setPublicURL points the fleet's public_url at the harness's own httptest
// server. It is http://127.0.0.1:<port>, which parsePublicURL allows because
// it is loopback. Enrollment tokens and /enroll.sh are gated on this setting,
// so any test that reaches them calls this first; the ones that deliberately
// exercise the unset state (the gate, and the requestIsHTTPS fallback) do not.
func (h *harness) setPublicURL() string {
	h.t.Helper()
	resp, body := h.do("PUT", "/api/v1/fleet/settings", map[string]any{"public_url": h.srv.URL})
	require.Equal(h.t, 200, resp.StatusCode, body)
	return h.srv.URL
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

	// The token file is gone after activation, so a second HTTP activation is
	// refused by the gate before it ever reaches the already-activated check.
	resp, _ = h.do("POST", "/api/v1/fleet/activate", map[string]string{"passphrase": "again", "email": "a@b", "password": "pw12345678"})
	require.Equal(t, 403, resp.StatusCode)
	require.Empty(t, h.s.SetupTokenPathForTesting(), "setup token cleared on activation")
	require.ErrorIs(t, h.s.Activate(t.Context(), "again", "a@b", "pw12345678"), api.ErrAlreadyActivated)

	resp, _ = h.do("DELETE", "/api/v1/fleet/session", nil)
	require.Equal(t, 204, resp.StatusCode)
}

// The session cookie must keep its Secure attribute when Fleet runs behind a
// TLS-terminating reverse proxy, where r.TLS is always nil and the only
// evidence of the client's scheme is X-Forwarded-Proto.
func TestSessionCookieSecureFollowsForwardedProto(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()

	login := func(proto string) []*http.Cookie {
		t.Helper()
		req, _ := http.NewRequest("POST", h.srv.URL+"/api/v1/fleet/session", jsonBody(map[string]string{"email": "hody@hody.dev", "password": "pw12345678"}))
		req.Header.Set("Content-Type", "application/json")
		if proto != "" {
			req.Header.Set("X-Forwarded-Proto", proto)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, 204, resp.StatusCode)
		cs := resp.Cookies()
		require.Len(t, cs, 2, "login sets the session and CSRF cookies")
		return cs
	}

	// Both cookies follow the same scheme: a CSRF token leaked over plain HTTP
	// is as good as no CSRF token.
	secure := func(proto string) bool {
		t.Helper()
		cs := login(proto)
		require.Equal(t, cs[0].Secure, cs[1].Secure, "both cookies agree")
		return cs[0].Secure
	}

	require.False(t, secure(""), "plain http must not set Secure")
	require.False(t, secure("http"))
	require.True(t, secure("https"), "https through a proxy must set Secure")
	require.True(t, secure("HTTPS"), "the header value is case-insensitive")
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
	h := &harness{t: t, srv: ts, s: s}
	h.activateAndLogin()
	ts.Close()
	require.NoError(t, s.Close())

	s2 := api.New(dir)
	defer s2.Close()
	require.True(t, s2.Activated(), "key file + db must reopen without the passphrase")
}

// TestActivateRejectsLoopbackWithoutToken pins the dropped loopback bypass:
// RemoteAddr is the reverse proxy's address behind a proxy, so "came from
// 127.0.0.1" proves nothing about the caller. httptest.Server serves from
// loopback, and the request is still refused without the header.
func TestActivateRejectsLoopbackWithoutToken(t *testing.T) {
	h := newHarness(t)
	resp, _ := h.do("POST", "/api/v1/fleet/activate", map[string]string{"passphrase": "seal-me!", "email": "hody@hody.dev", "password": "pw12345678"})
	require.Equal(t, 403, resp.StatusCode)
	require.False(t, h.s.Activated())
}

// TestActivateRequiresToken drives the mux directly (rather than through a
// real TCP httptest.Server) so RemoteAddr can be forged to a non-loopback
// address, and checks the setup-token file bridges the gate.
func TestActivateRequiresToken(t *testing.T) {
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

// TestActivateRefusesToOverwriteUnloadableState pins C2: if New cannot load
// existing state (here: an unreadable DB) the server reports "not activated",
// but Activate must NOT write a fresh seal.key over the old one - deriving a
// new key from a new salt would permanently destroy every escrowed repo
// password and B2 key.
func TestActivateRefusesToOverwriteUnloadableState(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}

	dir := t.TempDir()
	s := api.New(dir)
	require.NoError(t, s.Activate(t.Context(), "seal-me!", "hody@hody.dev", "pw12345678"))
	require.NoError(t, s.Close())

	keyFile := filepath.Join(dir, "seal.key")
	before, err := os.ReadFile(keyFile)
	require.NoError(t, err)

	dbFile := filepath.Join(dir, "fleet.db")
	require.NoError(t, os.Chmod(dbFile, 0))
	t.Cleanup(func() { os.Chmod(dbFile, 0o600) }) //nolint:errcheck

	s2 := api.New(dir)
	t.Cleanup(func() { s2.Close() })
	require.False(t, s2.Activated(), "unreadable DB means state could not be loaded")

	err = s2.Activate(t.Context(), "different-passphrase", "attacker@example.com", "pw12345678")
	require.Error(t, err)
	require.Contains(t, err.Error(), "refusing to overwrite seal.key")

	after, err := os.ReadFile(keyFile)
	require.NoError(t, err)
	require.Equal(t, before, after, "seal.key must be byte-identical")
}

// TestFailedActivationLeavesNoStateBehind pins the ordering of Activate: the
// seal.key is written last and a failure rolls the state directory back, so a
// retry is not rejected by the "state exists but could not be loaded" guard.
func TestFailedActivationLeavesNoStateBehind(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}

	dir := t.TempDir()
	s := api.New(dir)
	t.Cleanup(func() { s.Close() }) //nolint:errcheck

	// A read-only state directory fails store.Open, which stands in for any
	// step after the key derivation.
	require.NoError(t, os.Chmod(dir, 0o500))
	require.Error(t, s.Activate(t.Context(), "seal-me!", "hody@hody.dev", "pw12345678"))
	require.NoError(t, os.Chmod(dir, 0o700))

	_, err := os.Stat(filepath.Join(dir, "seal.key"))
	require.ErrorIs(t, err, os.ErrNotExist, "a failed activation must not leave seal.key behind")

	require.NoError(t, s.Activate(t.Context(), "seal-me!", "hody@hody.dev", "pw12345678"), "retry after a failed activation")
	require.True(t, s.Activated())
}
