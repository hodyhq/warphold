// Package engine runs Kopia's server engine headless and drives it over its HTTP API.
package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	stderrors "errors"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/gorilla/mux"
	"github.com/minio/minio-go/v7"
	"github.com/pkg/errors"

	"github.com/kopia/kopia/agent/state"
	"github.com/kopia/kopia/internal/apiclient"
	"github.com/kopia/kopia/internal/auth"
	"github.com/kopia/kopia/internal/clock"
	"github.com/kopia/kopia/internal/passwordpersist"
	"github.com/kopia/kopia/internal/server"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/logging"
)

var log = logging.Module("warphold/engine")

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
//
// persist is where the server's own connect/create endpoints store the
// repository password. The agent passes passwordpersist.None() - its password
// comes from enrollment and nothing here may touch it; the app passes the
// CLI's configured strategy, because its repository is created by the UI's
// setup wizard and that password has nowhere else to go.
func StartHeadless(ctx context.Context, configFile, repoPassword, scope string, persist passwordpersist.Strategy) (_ *Headless, retErr error) {
	h := &Headless{User: headlessUser, Password: randomHex(32), LocalToken: randomHex(32), scope: scope}

	srv, err := server.New(ctx, &server.Options{
		ConfigFile:        configFile,
		ConnectOptions:    &repo.ConnectOptions{},
		RefreshInterval:   4 * time.Hour,
		Authenticator:     auth.AuthenticateSingleUser(h.User, h.Password),
		Authorizer:        auth.DefaultAuthorizer(),
		PasswordPersist:   persist,
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
		// The device runs no maintenance: the Fleet identity is the
		// repository's maintenance owner, and a hosted repository is served
		// over an append-only gateway that would refuse the deletes and the
		// kopia.maintenance overwrite a compaction issues. This is the
		// server-mode equivalent of --no-auto-maintenance; the option's effect
		// is pinned by internal/server's TestDisableMaintenanceStopsTheMaintenanceManager.
		DisableMaintenance: true,
	})
	if err != nil {
		return nil, errors.Wrap(err, "server.New")
	}

	h.srv = srv

	m := mux.NewRouter()
	srv.SetupControlAPIHandlers(m)
	srv.SetupHTMLUIAPIHandlers(m)
	// Serve the SPA itself so the tray's handoff URL lands on a real page.
	// The public bundle first (it must precede the "/" catch-all), then the
	// static files, which must come after the API handlers.
	srv.ServeSPAPublic(m, server.AssetFile())
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
		Handler:           newLocalAuth(m, h.LocalToken, h.User, h.Password, scope),
		ReadHeaderTimeout: 15 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	go func() { _ = h.http.Serve(ln) }()

	// From here on the engine is serving and may hold an open repository:
	// tear all of it down on any later failure so nothing leaks and no stale
	// engine.json points at a dead port.
	defer func() {
		if retErr != nil {
			_ = h.Stop(context.WithoutCancel(ctx))
		}
	}()

	open := func(ctx context.Context) (repo.Repository, error) {
		// The standalone app's first run has no repository at all: the UI's
		// setup wizard is what creates one, so a nil repository here means
		// "not configured" and the server comes up unconnected, exactly as
		// upstream's "server start" does (cli/config.go openRepository).
		//
		// The agent scope deliberately does not get this: it is enrolled, its
		// repository was connected at enrollment, and a missing config file
		// there means something went wrong and must be reported - not an
		// engine that quietly backs nothing up. An os.Stat error other than
		// IsNotExist is left to repo.Open to report.
		if _, err := os.Stat(configFile); os.IsNotExist(err) && scope == state.ScopeApp {
			return nil, nil
		}

		return openRepo(ctx, configFile, repoPassword, &repo.Options{})
	}

	if scope != state.ScopeApp {
		// An agent's storage is the Fleet: a Fleet restart must not take the
		// agent down with it, so a transient failure is retried until the
		// Fleet answers. The UI is already being served meanwhile.
		open = openWhenFleetAnswers(configFile, open)
	}

	// The open runs last, once the UI is served, so a device waiting for its
	// Fleet still answers its tray and 'agent status'.
	if _, err := srv.InitRepositoryAsync(ctx, "Open", open, true); err != nil {
		return nil, errors.Wrap(err, "open repository")
	}

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

// The agent's repository opener and its retry pacing; vars so tests can fake
// the storage and shrink the waits.
var (
	openRepo          = repo.Open
	openRetryFirst    = 5 * time.Second
	openRetryMax      = 5 * time.Minute
	openRetryLogEvery = time.Minute
)

// openWhenFleetAnswers retries open with a doubling backoff (5s up to 5m)
// until it succeeds, fails permanently, or ctx ends, logging at most once a
// minute. The config file is read once up front: one that is missing or does
// not parse is local and permanent, never the Fleet being away.
func openWhenFleetAnswers(configFile string, open server.InitRepositoryFunc) server.InitRepositoryFunc {
	return func(ctx context.Context) (repo.Repository, error) {
		if _, err := repo.LoadConfigFromFile(configFile); err != nil {
			return nil, errors.Wrap(err, "read repository config")
		}

		delay := openRetryFirst

		var lastLog time.Time

		for {
			r, err := open(ctx)
			if err == nil || isPermanentOpenError(err) {
				return r, err
			}

			if now := clock.Now(); now.Sub(lastLog) >= openRetryLogEvery {
				log(ctx).Warnf("waiting for the Fleet: cannot open the repository yet (%v); retrying", err)

				lastLog = now
			}

			if !clock.SleepInterruptibly(ctx, delay) {
				return nil, ctx.Err()
			}

			delay = min(delay*2, openRetryMax)
		}
	}
}

// isPermanentOpenError reports an open failure waiting will not fix: a wrong
// password or credentials, a missing or unreadable config, or a 4xx refusal
// from the storage. Anything else (5xx, network) is the Fleet being away.
func isPermanentOpenError(err error) bool {
	if errors.Is(err, repo.ErrInvalidPassword) || errors.Is(err, blob.ErrInvalidCredentials) ||
		errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
		return true
	}

	var me minio.ErrorResponse
	if errors.As(err, &me) && me.StatusCode >= 400 && me.StatusCode < 500 {
		return me.StatusCode != http.StatusRequestTimeout && me.StatusCode != http.StatusTooManyRequests
	}

	return false
}
