package engine

import (
	"crypto/subtle"
	"encoding/base64"
	"net/http"
)

// localSessionPath is the tray's one-URL handoff into the engine's web UI:
// the tray reads the token from engine.json and opens
// http://127.0.0.1:<port>/local/session?t=<token> in the user's browser.
const localSessionPath = "/local/session"

// localCookieName names the session cookie the handoff sets. The cookie is
// deliberately not the token: the token travels in a URL, which browsers keep
// in history and hand to page scripts as a referrer, while the cookie stays
// in the cookie jar.
const localCookieName = "wh_local"

// localAuth converts a valid wh_local cookie into the HTTP basic auth Kopia's
// own handlers require (internal/server checks r.BasicAuth()). A request that
// already carries an Authorization header is passed through untouched, so the
// agent's own API client keeps authenticating as itself.
type localAuth struct {
	next   http.Handler
	token  string
	cookie string
	basic  string
}

func newLocalAuth(next http.Handler, token, user, password string) *localAuth {
	return &localAuth{
		next:   next,
		token:  token,
		cookie: randomHex(32),
		basic:  "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password)),
	}
}

func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func (a *localAuth) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == localSessionPath {
		a.handoff(w, r)
		return
	}

	if r.Header.Get("Authorization") == "" {
		if c, err := r.Cookie(localCookieName); err == nil && constantTimeEqual(c.Value, a.cookie) {
			r.Header.Set("Authorization", a.basic)
		}
	}

	a.next.ServeHTTP(w, r)
}

func (a *localAuth) handoff(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed\n", http.StatusMethodNotAllowed)
		return
	}

	// The token is never empty in practice (StartHeadless generates 32 random
	// bytes), but an empty one must never be accepted by an empty "?t=".
	t := r.URL.Query().Get("t")
	if t == "" || !constantTimeEqual(t, a.token) {
		http.Error(w, "invalid local session token\n", http.StatusForbidden)
		return
	}

	// No Secure flag: the engine listens on loopback over plain HTTP, and a
	// Secure cookie would never be sent back.
	http.SetCookie(w, &http.Cookie{
		Name:     localCookieName,
		Value:    a.cookie,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})

	http.Redirect(w, r, "/", http.StatusFound)
}
