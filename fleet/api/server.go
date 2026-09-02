// Package api serves the Fleet control-plane HTTP API.
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"

	"github.com/kopia/kopia/fleet"
	"github.com/kopia/kopia/fleet/b2api"
	"github.com/kopia/kopia/fleet/seal"
	"github.com/kopia/kopia/fleet/store"
)

// ErrAlreadyActivated is returned by Activate on an activated Fleet.
var ErrAlreadyActivated = errors.New("fleet is already activated")

const (
	sessionTTL = 12 * time.Hour
	// loginMaxAttempts is 6, not 5: the login flow always issues one successful
	// call before any failed ones (see activateAndLogin in server_test.go), and
	// that call shares the same per-IP bucket, so 5 would rate-limit the 5th
	// deliberate failure instead of the 6th (otherwise-correct) request.
	loginMaxAttempts = 6
	loginWindow      = time.Minute
	maxBody          = 1 << 20
)

// Server holds Fleet state for the HTTP handlers.
type Server struct {
	mu    sync.RWMutex
	paths fleet.Paths
	st    *store.Store
	key   seal.Key
	sess  *sessions
	login *limiter
	now   func() time.Time
	b2    b2api.API
}

// New creates a Server for stateDir; if Fleet was activated before, its state is loaded.
func New(stateDir string) *Server {
	s := &Server{paths: fleet.PathsFor(stateDir), login: newLimiter(loginMaxAttempts, loginWindow), now: time.Now, b2: b2api.New(nil)}
	_ = s.load()
	return s
}

func (s *Server) load() error {
	key, err := seal.ReadKeyFile(s.paths.KeyFile)
	if err != nil {
		return err
	}
	st, err := store.Open(s.paths.DB)
	if err != nil {
		return err
	}
	secret, err := st.Setting(context.Background(), "session_secret")
	if err != nil || secret == "" {
		st.Close()
		return errors.New("session secret missing")
	}
	s.mu.Lock()
	s.key, s.st, s.sess = key, st, newSessions([]byte(secret), sessionTTL)
	s.mu.Unlock()
	return nil
}

// Activated reports whether the store and key are loaded.
func (s *Server) Activated() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.st != nil
}

// Close closes the store.
func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st == nil {
		return nil
	}
	err := s.st.Close()
	s.st = nil
	return err
}

// Activate creates the DB, seals with a key derived from passphrase, and creates the first admin.
func (s *Server) Activate(ctx context.Context, passphrase, email, password string) error {
	if s.Activated() {
		return ErrAlreadyActivated
	}
	if len(passphrase) < 8 || len(password) < 8 || !strings.Contains(email, "@") {
		return errors.New("passphrase and password need 8+ characters and email must be valid")
	}
	salt, err := seal.NewSalt()
	if err != nil {
		return err
	}
	key := seal.Derive(passphrase, salt)
	if err := seal.WriteKeyFile(s.paths.KeyFile, key); err != nil {
		return err
	}
	st, err := store.Open(s.paths.DB)
	if err != nil {
		return err
	}
	pwHash, err := HashPassword(password)
	if err != nil {
		return err
	}
	secret := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, secret); err != nil {
		return err
	}
	if err := errors.Join(
		st.SetSetting(ctx, "seal_salt", hex.EncodeToString(salt)),
		st.SetSetting(ctx, "session_secret", hex.EncodeToString(secret)),
	); err != nil {
		return err
	}
	if _, err := st.CreateAdmin(ctx, email, pwHash); err != nil {
		return err
	}
	st.Close()
	return s.load()
}

// Mount registers all Fleet routes.
func (s *Server) Mount(m *mux.Router) {
	m.HandleFunc("/api/v1/fleet/status", s.handleStatus).Methods(http.MethodGet)
	m.HandleFunc("/api/v1/fleet/activate", s.handleActivate).Methods(http.MethodPost)
	m.HandleFunc("/api/v1/fleet/session", s.handleLogin).Methods(http.MethodPost)
	m.HandleFunc("/api/v1/fleet/session", s.handleLogout).Methods(http.MethodDelete)
	s.mountAdmin(m) // Task 7
	s.mountAgent(m) // Tasks 11, 14
}

func decode(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(v)
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"activated": s.Activated()})
}

func (s *Server) handleActivate(w http.ResponseWriter, r *http.Request) {
	var in struct{ Passphrase, Email, Password string }
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed body")
		return
	}
	if err := s.Activate(r.Context(), in.Passphrase, in.Email, in.Password); err != nil {
		if errors.Is(err, ErrAlreadyActivated) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	a, _ := s.st.AdminByEmail(r.Context(), in.Email)
	writeJSON(w, http.StatusCreated, map[string]any{"admin_id": a.ID})
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.Activated() {
		writeErr(w, http.StatusConflict, "fleet is not activated")
		return
	}
	if !s.login.allow(clientIP(r)) {
		writeErr(w, http.StatusTooManyRequests, "too many attempts, wait a minute")
		return
	}
	var in struct{ Email, Password string }
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed body")
		return
	}
	a, err := s.st.AdminByEmail(r.Context(), in.Email)
	if err != nil || !VerifyPassword(in.Password, a.PWHash) {
		writeErr(w, http.StatusUnauthorized, "wrong email or password")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: s.sess.issue(a.ID), Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil, MaxAge: int(sessionTTL.Seconds())})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleLogout(w http.ResponseWriter, _ *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, MaxAge: -1})
	w.WriteHeader(http.StatusNoContent)
}

// requireActivated wraps admin handlers so they 409 before activation.
func (s *Server) requireActivated(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.Activated() {
			writeErr(w, http.StatusConflict, "fleet is not activated")
			return
		}
		next(w, r)
	}
}
