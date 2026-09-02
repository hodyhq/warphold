package engine

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net/http"

	"github.com/kopia/kopia/agent/state"
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

// localInfoPath serves the agent page's vault label. It is the one thing the
// page needs that Kopia's own API does not know: which enrollment this engine
// belongs to.
const localInfoPath = "/local/info"

// localAuth converts a valid wh_local cookie into the HTTP basic auth Kopia's
// own handlers require (internal/server checks r.BasicAuth()). A request that
// already carries an Authorization header is passed through untouched, so the
// agent's own API client keeps authenticating as itself.
type localAuth struct {
	next   http.Handler
	token  string
	cookie string
	basic  string
	// scope selects the state directory /local/info reads agent.json from.
	scope string
}

func newLocalAuth(next http.Handler, token, user, password, scope string) *localAuth {
	return &localAuth{
		next:   next,
		token:  token,
		cookie: randomHex(32),
		basic:  "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password)),
		scope:  scope,
	}
}

func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// sameOrigin reports whether r may be authenticated from the wh_local cookie
// alone. SameSite=Strict does not isolate ports, so a page served from any
// other 127.0.0.1 port is same-site and its fetches carry the cookie; the
// fetch-metadata and Origin headers are what actually separate the engine's
// own UI from such a page.
//
// A request with neither header is allowed on purpose. Browsers send at least
// one of them on a cross-origin request, so "neither" means the caller is not
// a browser (curl, the tray), and a local process that can read the cookie can
// read engine.json's password just as easily - it is not the threat here.
func sameOrigin(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
	default:
		return false
	}

	if o := r.Header.Get("Origin"); o != "" && o != "http://"+r.Host {
		return false
	}

	return true
}

func (a *localAuth) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case localSessionPath:
		a.handoff(w, r)
		return
	case localInfoPath:
		a.info(w, r)
		return
	}

	if r.Header.Get("Authorization") == "" && sameOrigin(r) {
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

// localInfo is the agent page's vault label: a display name and the Fleet
// group it was enrolled into. Deliberately nothing else - the target, its
// bucket and its keys live in the same state directory and are never served.
type localInfo struct {
	Name string `json:"name"`
	// Group is empty for now: the enroll response carries the agent's name
	// only (see fleet/api handleEnroll), so agent.json has no group to read.
	// It is served anyway so the label has one shape whether or not the
	// enrollment ever grows one.
	Group string `json:"group"`
}

// info answers with the label. It is not sensitive, but it is guarded exactly
// like the handoff - cookie plus same origin - so the engine's local surface
// has one rule rather than a per-endpoint judgement call.
func (a *localAuth) info(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed\n", http.StatusMethodNotAllowed)
		return
	}

	if !sameOrigin(r) {
		http.Error(w, "forbidden\n", http.StatusForbidden)
		return
	}

	c, err := r.Cookie(localCookieName)
	if err != nil || !constantTimeEqual(c.Value, a.cookie) {
		http.Error(w, "unauthorized\n", http.StatusUnauthorized)
		return
	}

	// A missing agent.json is not an error here: an engine can run before (or
	// without) enrollment, and the page falls back to its own default label
	// rather than showing a failure where a name belongs.
	var out localInfo

	if cfg, err := state.Load(a.scope); err == nil {
		out.Name = cfg.Name
	}

	w.Header().Set("Content-Type", "application/json")

	// The response is already committed by the time an encode can fail, so
	// there is nothing left to tell the client.
	_ = json.NewEncoder(w).Encode(out)
}
