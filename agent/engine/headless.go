// Package engine runs Kopia's server engine headless and drives it over its HTTP API.
package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	stderrors "errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/gorilla/mux"
	"github.com/pkg/errors"

	"github.com/kopia/kopia/agent/state"
	"github.com/kopia/kopia/internal/apiclient"
	"github.com/kopia/kopia/internal/auth"
	"github.com/kopia/kopia/internal/clock"
	"github.com/kopia/kopia/internal/passwordpersist"
	"github.com/kopia/kopia/internal/server"
	"github.com/kopia/kopia/repo"
)

const headlessUser = "warphold-agent"

// Headless is Kopia's engine on loopback: scheduler, uploads, tasks, and the
// WarpHold UI.
type Headless struct {
	BaseURL string
	User    string
	// Password authenticates API clients; LocalToken is the value the tray puts
	// in a /local/session URL to obtain a browser session. Both are generated
	// per process, expire with it, and also live in engine.json.
	Password   string
	LocalToken string

	scope string
	srv   *server.Server
	http  *http.Server
	ln    net.Listener
}

// randomHex returns n random bytes as hex. A failing entropy source must never
// degrade into a short or guessable token: every caller here mints a secret
// (the API password, the local-session token, the session cookie), so the only
// safe answer is to take the process down.
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("warphold: crypto/rand failed, refusing to mint a guessable token: " + err.Error())
	}

	return hex.EncodeToString(b)
}

// StartHeadless opens the repository at configFile and serves the control +
// UI API, plus the WarpHold UI itself, on 127.0.0.1:0. scope selects the state directory (see state.Dir),
// which holds the UI preferences and the engine.json written once the engine
// is listening; Stop removes that file.
func StartHeadless(ctx context.Context, configFile, repoPassword, scope string) (_ *Headless, retErr error) {
	h := &Headless{User: headlessUser, Password: randomHex(32), LocalToken: randomHex(32), scope: scope}
	srv, err := server.New(ctx, &server.Options{
		ConfigFile:        configFile,
		ConnectOptions:    &repo.ConnectOptions{},
		RefreshInterval:   4 * time.Hour,
		Authenticator:     auth.AuthenticateSingleUser(h.User, h.Password),
		Authorizer:        auth.DefaultAuthorizer(),
		PasswordPersist:   passwordpersist.None(),
		UIUser:            h.User,
		ServerControlUser: h.User,
		UIPreferencesFile: filepath.Join(state.Dir(scope), "ui-preferences.json"),
		// The engine serves the WarpHold UI, but its own API client (and the
		// tray) authenticate with basic auth and carry no session cookie, so
		// Kopia's cookie-bound CSRF token can't be required here. Requests
		// reach the engine either with basic auth or through localAuth's
		// cookie branch, and that branch injects credentials only for
		// same-origin requests, so a page on another loopback port cannot ride
		// the session cookie.
		DisableCSRFTokenChecks: true,
		MinMaintenanceInterval: 24 * time.Hour,
	})
	if err != nil {
		return nil, errors.Wrap(err, "server.New")
	}
	h.srv = srv
	open := func(ctx context.Context) (repo.Repository, error) {
		return repo.Open(ctx, configFile, repoPassword, &repo.Options{})
	}
	if _, err := srv.InitRepositoryAsync(ctx, "Open", open, true); err != nil {
		return nil, errors.Wrap(err, "open repository")
	}

	// From here on the repository is open and background goroutines are running:
	// release it on any subsequent failure so we don't leak them.
	defer func() {
		if retErr != nil {
			_ = srv.SetRepository(ctx, nil)
		}
	}()

	m := mux.NewRouter()
	srv.SetupControlAPIHandlers(m)
	srv.SetupHTMLUIAPIHandlers(m)
	// Serve the SPA itself so the tray's handoff URL lands on a real page.
	// Deep links first (they must precede the "/" catch-all), then the static
	// files, which must come after the API handlers.
	server.ServeSPADeepLinks(m)
	srv.ServeStaticFiles(m, server.AssetFile())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	h.ln = ln
	h.BaseURL = "http://" + ln.Addr().String()

	if err := WriteInfo(scope, Info{
		BaseURL:    h.BaseURL,
		User:       h.User,
		Password:   h.Password,
		LocalToken: h.LocalToken,
		PID:        os.Getpid(),
		StartedAt:  clock.Now(),
	}); err != nil {
		ln.Close() //nolint:errcheck,gosec

		return nil, errors.Wrap(err, "write engine info")
	}

	h.http = &http.Server{
		Handler:           newLocalAuth(m, h.LocalToken, h.User, h.Password),
		ReadHeaderTimeout: 15 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	go func() { _ = h.http.Serve(ln) }()

	return h, nil
}

// Client returns an API client authenticated as the headless user.
func (h *Headless) Client() (*apiclient.KopiaAPIClient, error) {
	return apiclient.NewKopiaAPIClient(apiclient.Options{BaseURL: h.BaseURL, Username: h.User, Password: h.Password})
}

// Stop shuts the HTTP server and disconnects the repository.
func (h *Headless) Stop(ctx context.Context) error {
	ctx2, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err := h.http.Shutdown(ctx2)

	return stderrors.Join(err, h.srv.SetRepository(ctx, nil), RemoveInfo(h.scope))
}
