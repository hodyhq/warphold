// Package engine runs Kopia's server engine headless and drives it over its HTTP API.
package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	stderrors "errors"
	"net"
	"net/http"
	"path/filepath"
	"time"

	"github.com/gorilla/mux"
	"github.com/pkg/errors"

	"github.com/kopia/kopia/internal/apiclient"
	"github.com/kopia/kopia/internal/auth"
	"github.com/kopia/kopia/internal/passwordpersist"
	"github.com/kopia/kopia/internal/server"
	"github.com/kopia/kopia/repo"
)

const headlessUser = "warphold-agent"

// Headless is Kopia's engine on loopback: scheduler, uploads, tasks; no static UI.
type Headless struct {
	BaseURL  string
	User     string
	Password string

	srv  *server.Server
	http *http.Server
	ln   net.Listener
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// StartHeadless opens the repository at configFile and serves the control + UI API on 127.0.0.1:0.
func StartHeadless(ctx context.Context, configFile, repoPassword, prefsDir string) (*Headless, error) {
	h := &Headless{User: headlessUser, Password: randomHex(32)}
	srv, err := server.New(ctx, &server.Options{
		ConfigFile:             configFile,
		ConnectOptions:         &repo.ConnectOptions{},
		RefreshInterval:        4 * time.Hour,
		Authenticator:          auth.AuthenticateSingleUser(h.User, h.Password),
		Authorizer:             auth.DefaultAuthorizer(),
		PasswordPersist:        passwordpersist.None(),
		UIUser:                 h.User,
		ServerControlUser:      h.User,
		UIPreferencesFile:      filepath.Join(prefsDir, "ui-preferences.json"),
		DisableCSRFTokenChecks: true, // loopback + random per-process password; CSRF is for browsers
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
	m := mux.NewRouter()
	srv.SetupControlAPIHandlers(m)
	srv.SetupHTMLUIAPIHandlers(m)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	h.ln = ln
	h.BaseURL = "http://" + ln.Addr().String()
	h.http = &http.Server{Handler: m, ReadHeaderTimeout: 15 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
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
	return stderrors.Join(err, h.srv.SetRepository(ctx, nil))
}
