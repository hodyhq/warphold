package api

import (
	"crypto/subtle"
	"log"
	"net/http"
	"net/url"
	"strings"
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

// originAllowed is the origin half of the CSRF defence, and it is deliberately
// not fail-closed:
//
//	Origin present  -> must equal publicURL's scheme+host, else reject
//	else Referer    -> its origin must match, else reject
//	neither present -> PASS. Non-browser clients (curl, the agent, CI) send
//	                   no Origin at all; they are already covered by the
//	                   double-submit token, which a cross-site page cannot
//	                   forge. Failing closed here would break every CLI while
//	                   stopping nothing a browser can do.
//	publicURL nil   -> skipped by the caller, with a warning.
func originAllowed(r *http.Request, publicURL *url.URL) bool {
	if publicURL == nil {
		return true
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		if ref := r.Header.Get("Referer"); ref != "" {
			u, err := url.Parse(ref)
			if err != nil || u.Host == "" {
				return false
			}
			origin = u.Scheme + "://" + u.Host
		}
	}
	if origin == "" {
		return true
	}
	// "null" is what a sandboxed iframe or a data: document sends; it matches
	// no public URL and must not be treated as a missing header.
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	// Scheme and host are case-insensitive, and a browser always sends them
	// ASCII (an internationalized host is punycoded before it reaches Origin).
	return strings.EqualFold(u.Scheme, publicURL.Scheme) && strings.EqualFold(u.Host, publicURL.Host)
}

// requireCSRF enforces csrfOK and the origin check on state-changing methods.
// Safe methods are exempt, so the UI can load without a token in hand.
func (s *Server) requireCSRF(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if !csrfOK(r) {
				writeErr(w, http.StatusForbidden, "missing or invalid "+csrfHeader+" header")
				return
			}
			pu, ok := s.PublicURL(r.Context())
			if !ok {
				s.csrfWarnOnce.Do(func() {
					log.Print("warphold fleet: public_url is not set, so the CSRF origin check is disabled; set it in Settings")
				})
			}
			if !originAllowed(r, pu) {
				writeErr(w, http.StatusForbidden, "request origin does not match the configured public URL")
				return
			}
		}
		next(w, r)
	}
}
