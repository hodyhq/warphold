package server

import (
	"net/http"

	"github.com/gorilla/mux"
)

// warphold: upstream's isKnownUIRoute is a hardcoded allowlist of upstream
// client routes, so a direct navigation or a refresh on one of WarpHold's own
// routes (/fleet/devices, /agent, ...) falls through to the file server and
// 404s. Registering these routes before ServeStaticFiles keeps upstream
// untouched: each one rewrites the path to "/" and re-dispatches through the
// same router, so the index is served by upstream's own handler, with its
// authentication check, session cookie and index patching (CSRF token, version)
// intact.
//
// Must be called before Server.ServeStaticFiles, which registers the "/"
// catch-all: gorilla/mux matches in registration order.
func ServeSPADeepLinks(m *mux.Router) {
	serveIndex := func(w http.ResponseWriter, r *http.Request) {
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/"

		m.ServeHTTP(w, r2)
	}

	for _, p := range []string{"/fleet", "/agent"} {
		// exact + subtree only, so an unrelated "/fleeting" still 404s.
		m.Path(p).HandlerFunc(serveIndex)
		m.PathPrefix(p + "/").HandlerFunc(serveIndex)
	}
}
