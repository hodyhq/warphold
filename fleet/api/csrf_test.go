package api

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := parsePublicURL(raw)
	require.NoError(t, err)
	return u
}

// originAllowed is the origin half of the CSRF defence. The case that matters
// most is the one that passes: a request with neither header is curl, the
// agent or CI, and rejecting it would break every non-browser client while
// stopping nothing a browser can do.
func TestOriginAllowed(t *testing.T) {
	pub := mustURL(t, "https://fleet.example.com")

	req := func(headers map[string]string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		return r
	}

	require.True(t, originAllowed(req(map[string]string{"Origin": "https://fleet.example.com"}), pub), "matching Origin")
	require.False(t, originAllowed(req(map[string]string{"Origin": "https://evil.example.com"}), pub), "foreign Origin")
	require.True(t, originAllowed(req(map[string]string{"Referer": "https://fleet.example.com/devices"}), pub), "no Origin, matching Referer")
	require.False(t, originAllowed(req(map[string]string{"Referer": "https://evil.example.com/devices"}), pub), "no Origin, foreign Referer")
	require.True(t, originAllowed(req(nil), pub), "neither header: curl and the agent must keep working")
	require.True(t, originAllowed(req(nil), nil), "no public_url: the check is skipped")

	// A browser sends a scheme too; http://fleet.example.com is a different
	// origin from the https one and must not pass.
	require.False(t, originAllowed(req(map[string]string{"Origin": "http://fleet.example.com"}), pub), "scheme is part of the origin")
	require.True(t, originAllowed(req(map[string]string{"Origin": "https://FLEET.example.com"}), pub), "origins are case-insensitive")
	// "null" is what a sandboxed iframe or a data: document sends. It is a
	// present header, so it must be rejected rather than read as absent.
	require.False(t, originAllowed(req(map[string]string{"Origin": "null"}), pub), "an opaque origin is not a missing one")
	require.False(t, originAllowed(req(map[string]string{"Referer": "garbage"}), pub))
}

// With no public_url there is nothing to compare an origin against, so the
// check is skipped - but exactly once per server, the operator is told.
func TestRequireCSRFWarnsOnceWithoutPublicURL(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })

	s := &Server{}
	called := 0
	h := s.requireCSRF(func(http.ResponseWriter, *http.Request) { called++ })

	post := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.AddCookie(&http.Cookie{Name: csrfCookie, Value: "tok"})
		r.Header.Set(csrfHeader, "tok")
		// A foreign origin, which would be rejected if public_url were set.
		r.Header.Set("Origin", "https://evil.example.com")
		rr := httptest.NewRecorder()
		h(rr, r)
		return rr
	}

	require.Equal(t, 200, post().Code)
	require.Equal(t, 200, post().Code)
	require.Equal(t, 2, called)
	require.Contains(t, buf.String(), "public_url is not set")
	require.Equal(t, 1, bytes.Count(buf.Bytes(), []byte("public_url is not set")), "warned once, not once per request")
}

// A safe method is exempt from the whole check, origin included: the UI has to
// be able to load a page it was linked to.
func TestRequireCSRFExemptsSafeMethods(t *testing.T) {
	s := &Server{}
	called := false
	h := s.requireCSRF(func(http.ResponseWriter, *http.Request) { called = true })

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Origin", "https://evil.example.com")
	rr := httptest.NewRecorder()
	h(rr, r)
	require.True(t, called)
	require.Equal(t, 200, rr.Code)
}

func TestParsePublicURL(t *testing.T) {
	for raw, want := range map[string]string{
		"https://fleet.example.com":         "https://fleet.example.com",
		"https://fleet.example.com/":        "https://fleet.example.com",
		"  https://FLEET.example.com:8443 ": "https://fleet.example.com:8443",
		"http://localhost:51515":            "http://localhost:51515",
		"http://127.0.0.1:8080":             "http://127.0.0.1:8080",
		"http://[::1]:8080":                 "http://[::1]:8080",
		// A default port is not part of the origin a browser sends.
		"https://fleet.example.com:443": "https://fleet.example.com",
		"http://localhost:80":           "http://localhost",
		"https://[::1]:443":             "https://[::1]",
	} {
		u, err := parsePublicURL(raw)
		require.NoError(t, err, raw)
		require.Equal(t, want, u.String())
	}

	for _, raw := range []string{
		"",
		"fleet.example.com",
		"/fleet",
		"ftp://fleet.example.com",
		"http://fleet.example.com",
		"https://",
		"https://fleet.example.com/path",
		"https://fleet.example.com?a=b",
		"https://fleet.example.com#frag",
		"https://user:pass@fleet.example.com",
		"https://fleet.example.com; echo pwned",
		// Internationalized host names must be supplied punycoded, or they
		// would never match the Origin a browser sends.
		"https://fleet.exämple.com",
	} {
		_, err := parsePublicURL(raw)
		require.Error(t, err, raw)
	}
}

// The default port is stripped so that an Origin header, which never carries
// one, still matches a public_url written with it.
func TestDefaultPortMatchesOrigin(t *testing.T) {
	pub := mustURL(t, "https://fleet.example.com:443")
	require.Equal(t, "https://fleet.example.com", pub.String())

	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("Origin", "https://fleet.example.com")
	require.True(t, originAllowed(r, pub))
}
