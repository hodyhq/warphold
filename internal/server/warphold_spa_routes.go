package server

import (
	"bytes"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/gorilla/mux"

	"github.com/kopia/kopia/internal/clock"
)

// warpholdPublicFiles are the files the built index.html references by an
// absolute path, plus the crawler/PWA files a browser asks for on its own.
// The bundle's hashed files live under /assets/ and are matched by prefix.
var warpholdPublicFiles = []string{
	"/favicon.ico",
	"/logo192.png",
	"/logo512.png",
	"/manifest.json",
	"/robots.txt",
	"/warphold.svg",
}

// ServeSPAPublic serves the WarpHold SPA bundle with no authentication.
//
// warphold: upstream serves the bundle from ServeStaticFiles, behind the UI
// basic-auth check (requireUIUser), so a server started with --server-username
// / --server-password answers 401 to GET / and the Fleet dashboard can never
// render its own login page. Upstream's isKnownUIRoute is also a hardcoded
// allowlist of upstream client routes, so a refresh on /fleet/devices falls
// through to the file server and 404s.
//
// Both are fixed here rather than in upstream files. The bundle is public
// static code: the index (patched exactly the way upstream patches it - title,
// version, CSRF token bound to the session cookie), the hashed assets, the few
// icons above, and the client-side deep links, which serve the same index.
// Everything else - /api/v1/*, /local/*, unknown paths - is left to upstream's
// handlers with their authentication intact.
//
// Must be called before Server.ServeStaticFiles, which registers the "/"
// catch-all: gorilla/mux matches in registration order.
func (s *Server) ServeSPAPublic(m *mux.Router, fsys http.FileSystem) {
	indexBytes := maybeReadIndexBytes(fsys)
	files := http.FileServer(fsys)

	// `server start --ui=false` never registers upstream's catch-all, and then
	// the bundle must not be served either. The hook that mounts these routes
	// runs before that flag is read, so the router is probed instead - once, on
	// the first request, by which time registration is complete - with a path
	// only a catch-all can match.
	uiEnabled := sync.OnceValue(func() bool {
		var rm mux.RouteMatch

		probe := &http.Request{Method: http.MethodGet, URL: &url.URL{Path: "/warphold-ui-enabled-probe"}}

		return m.Match(probe, &rm) && rm.MatchErr == nil
	})

	serveIndex := func(w http.ResponseWriter, r *http.Request) {
		if indexBytes == nil || !uiEnabled() {
			http.NotFound(w, r)
			return
		}

		var sessionID string

		if cookie, err := r.Cookie(kopiaSessionCookie); err == nil {
			// already in a session, likely a new tab was opened
			sessionID = cookie.Value
		} else {
			sessionID = uuid.NewString()

			http.SetCookie(w, &http.Cookie{
				Name:  kopiaSessionCookie,
				Value: sessionID,
				Path:  "/",
			})
		}

		// the index embeds a per-session CSRF token, so it must never be cached.
		w.Header().Set("Cache-Control", "no-cache")

		http.ServeContent(w, r, "/", clock.Now(), bytes.NewReader(s.patchIndexBytes(sessionID, indexBytes)))
	}

	m.Path("/").HandlerFunc(serveIndex)

	for _, p := range []string{"/fleet", "/agent"} {
		// exact + subtree only, so an unrelated "/fleeting" still 404s.
		m.Path(p).HandlerFunc(serveIndex)
		m.PathPrefix(p + "/").HandlerFunc(serveIndex)
	}

	serveFile := func(cacheControl string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			// no directory listings: http.FileServer generates one for any
			// directory without an index.html, and /assets/ is one.
			if !uiEnabled() || strings.HasSuffix(r.URL.Path, "/") {
				http.NotFound(w, r)
				return
			}

			// Only a file that exists gets the cache policy: an immutable header on
			// a 404 would let a proxy pin the miss for a year.
			f, err := fsys.Open(r.URL.Path)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			f.Close() //nolint:errcheck // read-only probe

			w.Header().Set("Cache-Control", cacheControl)
			files.ServeHTTP(w, r)
		}
	}

	// /assets/ holds content-hashed files, so a new build is a new URL.
	// mux cleans the request path before matching and http.FileServer cleans it
	// again, so "/assets/../api/v1/sources" can never reach the file system.
	m.PathPrefix("/assets/").HandlerFunc(serveFile("public, max-age=31536000, immutable"))

	for _, p := range warpholdPublicFiles {
		m.Path(p).HandlerFunc(serveFile("no-cache"))
	}
}
