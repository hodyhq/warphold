package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net/http"

	"github.com/kopia/kopia/fleet/store"
)

const (
	sessionCookie = "wh_session"
	sessionPrefix = "ws_"
	csrfCookie    = "wh_csrf"
	csrfHeader    = "X-WarpHold-CSRF"
)

type ctxKey int

// sessionCtxKey carries the *store.Session that requireAdmin resolved.
const sessionCtxKey ctxKey = iota

// newSessionToken returns an opaque session token and the SHA-256 the store
// keeps. The token itself is never persisted, so a copy of fleet.db cannot be
// replayed as a login.
func newSessionToken() (string, []byte, error) {
	b := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", nil, err
	}
	tok := sessionPrefix + base64.RawURLEncoding.EncodeToString(b)
	return tok, sessionTokenHash(tok), nil
}

func sessionTokenHash(tok string) []byte {
	h := sha256.Sum256([]byte(tok))
	return h[:]
}

// startSession records a session for adminID and sets the session and CSRF
// cookies on w. Both cookies share the request's scheme and the session TTL:
// a CSRF token that outlives its session, or travels in the clear while the
// session does not, protects nothing.
func (s *Server) startSession(ctx context.Context, w http.ResponseWriter, r *http.Request, adminID int64) error {
	st := s.store()
	if st == nil {
		return errors.New("fleet is not activated")
	}
	tok, hash, err := newSessionToken()
	if err != nil {
		return err
	}
	csrf := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, csrf); err != nil {
		return err
	}
	now := s.now()
	if _, err := st.CreateSession(ctx, hash, adminID, now, now.Add(sessionTTL)); err != nil {
		return err
	}
	secure, maxAge := s.cookieSecure(r), int(sessionTTL.Seconds())
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: tok, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: secure, MaxAge: maxAge})
	// Not HttpOnly on purpose: the admin UI has to read this one to echo it
	// back in the X-WarpHold-CSRF header. It authorizes nothing on its own.
	http.SetCookie(w, &http.Cookie{Name: csrfCookie, Value: hex.EncodeToString(csrf), Path: "/", SameSite: http.SameSiteStrictMode, Secure: secure, MaxAge: maxAge})
	return nil
}

// currentSession resolves r's session cookie against the store, or returns
// nil for a missing, unknown, revoked or expired session. Every admin request
// pays this lookup, which is what makes revocation immediate.
func (s *Server) currentSession(r *http.Request) *store.Session {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	st := s.store()
	if st == nil {
		return nil
	}
	sess, err := st.SessionByHash(r.Context(), sessionTokenHash(c.Value))
	if err != nil || sess.RevokedAt != nil || !s.now().Before(sess.ExpiresAt) {
		return nil
	}
	return sess
}

// sessionFrom returns the session requireAdmin put in the request context.
func sessionFrom(r *http.Request) *store.Session {
	sess, _ := r.Context().Value(sessionCtxKey).(*store.Session)
	return sess
}

// adminFrom returns the signed-in admin's id, or 0 outside requireAdmin.
func adminFrom(r *http.Request) int64 {
	if sess := sessionFrom(r); sess != nil {
		return sess.AdminID
	}
	return 0
}

// cookieSecure decides the Secure attribute. Once public_url is set it is the
// only input: that is the scheme the browser actually reached us on, whereas
// r.TLS and X-Forwarded-Proto describe the last hop into this process, which
// behind a TLS-terminating proxy is plain HTTP. requestIsHTTPS remains the
// fallback for a Fleet whose public_url has not been set yet.
func (s *Server) cookieSecure(r *http.Request) bool {
	if u, ok := s.PublicURL(r.Context()); ok {
		return u.Scheme == "https"
	}
	return requestIsHTTPS(r)
}

// clearAuthCookies expires both cookies on the client. Secure must match what
// startSession set, or the browser treats these as different cookies and
// keeps the live ones.
func (s *Server) clearAuthCookies(w http.ResponseWriter, r *http.Request) {
	secure := s.cookieSecure(r)
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: secure, MaxAge: -1})
	http.SetCookie(w, &http.Cookie{Name: csrfCookie, Value: "", Path: "/", SameSite: http.SameSiteStrictMode, Secure: secure, MaxAge: -1})
}
