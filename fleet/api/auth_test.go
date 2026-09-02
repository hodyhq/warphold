package api

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSessionTokenIsOpaqueAndHashed(t *testing.T) {
	tok, hash, err := newSessionToken()
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(tok, sessionPrefix))
	require.Len(t, hash, 32)
	require.Equal(t, hash, sessionTokenHash(tok))
	require.NotContains(t, string(hash), tok, "the store never holds the token itself")

	other, _, err := newSessionToken()
	require.NoError(t, err)
	require.NotEqual(t, tok, other)
}

func TestCSRFDoubleSubmit(t *testing.T) {
	req := func(cookie, header string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: csrfCookie, Value: cookie})
		}
		if header != "" {
			r.Header.Set(csrfHeader, header)
		}
		return r
	}

	require.True(t, csrfOK(req("tok", "tok")))
	require.False(t, csrfOK(req("tok", "other")))
	require.False(t, csrfOK(req("tok", "")), "cookie alone is what a cross-site form has")
	require.False(t, csrfOK(req("", "tok")))
	require.False(t, csrfOK(req("", "")), "two empty values must not compare equal")

	// Safe methods pass through untouched; unsafe ones do not.
	called := false
	h := requireCSRF(func(http.ResponseWriter, *http.Request) { called = true })

	rr := httptest.NewRecorder()
	h(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	require.True(t, called)
	require.Equal(t, 200, rr.Code)

	called = false
	rr = httptest.NewRecorder()
	h(rr, req("tok", ""))
	require.False(t, called)
	require.Equal(t, http.StatusForbidden, rr.Code)
}

func TestPasswordHashVerify(t *testing.T) {
	h, err := HashPassword("s3cret")
	require.NoError(t, err)
	require.Contains(t, h, "$argon2id$")
	require.True(t, VerifyPassword("s3cret", h))
	require.False(t, VerifyPassword("wrong", h))
	require.False(t, VerifyPassword("s3cret", "garbage"))
}

func TestDummyPWHashNeverCachesEmpty(t *testing.T) {
	h := dummyPWHash()
	require.NotEmpty(t, h)
	require.False(t, VerifyPassword("x", h))
}

func TestLimiter(t *testing.T) {
	l := newLimiter(3, time.Minute)
	now := time.Unix(0, 0)
	l.now = func() time.Time { return now }
	for range 3 {
		require.True(t, l.allow("1.2.3.4"))
	}
	require.False(t, l.allow("1.2.3.4"))
	require.True(t, l.allow("5.6.7.8"))
	now = now.Add(61 * time.Second)
	require.True(t, l.allow("1.2.3.4"))
}

// Cleanup used to drop only expired keys, so a flood of still-active keys grew
// the map without bound.
func TestLimiterEvictsWhenEveryKeyIsActive(t *testing.T) {
	l := newLimiter(3, time.Hour)
	now := time.Unix(0, 0)
	l.now = func() time.Time { return now }
	for i := range maxLimiterKeys + 50 {
		require.True(t, l.allow(strconv.Itoa(i)))
		now = now.Add(time.Millisecond)
	}
	require.LessOrEqual(t, len(l.hits), maxLimiterKeys+1)
	_, oldestKept := l.hits["0"]
	require.False(t, oldestKept, "least recently used key was evicted first")
}
