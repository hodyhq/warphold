package api

import (
	"crypto/subtle"
	"net/http"
)

// csrfOK reports whether r carries a double-submit CSRF token: the
// X-WarpHold-CSRF header must equal the wh_csrf cookie. A cross-site form can
// make the browser send the cookie, but it cannot read it back to set the
// header, and it cannot set a custom header at all. An empty cookie or header
// never matches - otherwise a request with neither would compare equal.
func csrfOK(r *http.Request) bool {
	c, err := r.Cookie(csrfCookie)
	if err != nil || c.Value == "" {
		return false
	}
	sent := r.Header.Get(csrfHeader)
	if sent == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(sent)) == 1
}

// requireCSRF enforces csrfOK on state-changing methods. Safe methods are
// exempt, so the UI can load without a token in hand.
func requireCSRF(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if !csrfOK(r) {
				writeErr(w, http.StatusForbidden, "missing or invalid "+csrfHeader+" header")
				return
			}
		}
		next(w, r)
	}
}
