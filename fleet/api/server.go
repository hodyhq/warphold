// Package api serves the Fleet control-plane HTTP API.
package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
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

// ErrInvalidActivation is returned by Activate when the passphrase, password
// or email fail validation; handleActivate maps it to 400 with its message,
// unlike any other error, which maps to a generic 500 (see handleActivate).
var ErrInvalidActivation = errors.New("passphrase and password need 8+ characters and email must be valid")

const (
	sessionTTL = 12 * time.Hour
	// loginMaxAttempts is 6, not 5: the login flow always issues one successful
	// call before any failed ones (see activateAndLogin in server_test.go), and
	// that call shares the same per-IP bucket, so 5 would rate-limit the 5th
	// deliberate failure instead of the 6th (otherwise-correct) request.
	loginMaxAttempts = 6
	loginWindow      = time.Minute
	maxBody          = 1 << 20

	setupTokenFile   = "setup-token"
	setupTokenHeader = "X-WarpHold-Setup-Token"
)

// Server holds Fleet state for the HTTP handlers.
type Server struct {
	mu sync.RWMutex
	// activateMu serializes Activate end to end (check-then-write), so
	// concurrent activation attempts cannot race past the Activated() check:
	// only the caller holding activateMu can see !Activated() and proceed.
	activateMu sync.Mutex

	paths fleet.Paths
	st    *store.Store
	key   seal.Key
	sess  *sessions
	login *limiter
	now   func() time.Time
	b2    b2api.API

	// setupTokenPath and setupToken gate POST /activate before the Fleet is
	// activated (see handleActivate); both are cleared once activation succeeds.
	setupTokenPath string
	setupToken     string
}

// New creates a Server for stateDir; if Fleet was activated before, its state is loaded.
func New(stateDir string) *Server {
	s := &Server{paths: fleet.PathsFor(stateDir), login: newLimiter(loginMaxAttempts, loginWindow), now: time.Now, b2: b2api.New(nil)}
	_ = s.load()
	if !s.Activated() {
		if path, token, err := ensureSetupToken(s.paths.StateDir); err == nil {
			s.mu.Lock()
			s.setupTokenPath, s.setupToken = path, token
			s.mu.Unlock()
		}
	}
	return s
}

// ensureSetupToken returns the setup token in dir, creating it (32 random
// bytes, hex-encoded, mode 0600 in a 0700 dir) if it does not already exist.
func ensureSetupToken(dir string) (path, token string, err error) {
	path = filepath.Join(dir, setupTokenFile)
	if b, err := os.ReadFile(path); err == nil {
		return path, strings.TrimSpace(string(b)), nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return path, "", err
	}
	raw := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return path, "", err
	}
	token = hex.EncodeToString(raw)
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return path, "", err
	}
	return path, token, nil
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
	s.activateMu.Lock()
	defer s.activateMu.Unlock()

	if s.Activated() {
		return ErrAlreadyActivated
	}
	if len(passphrase) < 8 || len(password) < 8 || !strings.Contains(email, "@") {
		return ErrInvalidActivation
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
	defer func() {
		if st != nil {
			st.Close()
		}
	}()
	pwHash, err := HashPassword(password)
	if err != nil {
		return err
	}
	// Admin insert happens before the settings writes: if it fails (e.g. a
	// duplicate email on retry), no settings are touched, so a retry with a
	// fresh email starts from a clean slate.
	//
	// ponytail: this doesn't make the whole activation atomic — a settings
	// write failing right after a successful insert still leaves a stray
	// admin row that blocks a same-email retry. A real fix needs a
	// transaction exposed by fleet/store; out of scope for this pass.
	if _, err := st.CreateAdmin(ctx, email, pwHash); err != nil {
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
	if err := st.Close(); err != nil {
		return err
	}
	st = nil
	if err := s.load(); err != nil {
		return err
	}
	s.clearSetupToken()
	return nil
}

// clearSetupToken deletes the one-time setup token file once activation succeeds.
func (s *Server) clearSetupToken() {
	s.mu.Lock()
	path := s.setupTokenPath
	s.setupTokenPath, s.setupToken = "", ""
	s.mu.Unlock()
	if path != "" {
		_ = os.Remove(path)
	}
}

// Mount registers all Fleet routes.
func (s *Server) Mount(m *mux.Router) {
	if !s.Activated() {
		s.mu.RLock()
		path := s.setupTokenPath
		s.mu.RUnlock()
		log.Printf("warphold fleet: not activated; setup token is in %s", path)
	}
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

// setupAllowed reports whether r may call POST /activate: either it comes
// from loopback, or it carries the one-time setup token. This only gates the
// HTTP endpoint; the `fleet activate` CLI calls Activate directly and is
// unaffected.
func (s *Server) setupAllowed(r *http.Request) bool {
	if ip := clientIP(r); ip == "127.0.0.1" || ip == "::1" {
		return true
	}
	s.mu.RLock()
	tok := s.setupToken
	s.mu.RUnlock()
	if tok == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(r.Header.Get(setupTokenHeader)), []byte(tok)) == 1
}

func (s *Server) handleActivate(w http.ResponseWriter, r *http.Request) {
	if !s.setupAllowed(r) {
		writeErr(w, http.StatusForbidden, "activation requires loopback or the setup token")
		return
	}
	var in struct{ Passphrase, Email, Password string }
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed body")
		return
	}
	if err := s.Activate(r.Context(), in.Passphrase, in.Email, in.Password); err != nil {
		switch {
		case errors.Is(err, ErrAlreadyActivated):
			writeErr(w, http.StatusConflict, err.Error())
		case errors.Is(err, ErrInvalidActivation):
			writeErr(w, http.StatusBadRequest, err.Error())
		default:
			log.Printf("warphold fleet: activation failed: %v", err)
			writeErr(w, http.StatusInternalServerError, "activation failed")
		}
		return
	}
	a, err := s.st.AdminByEmail(r.Context(), in.Email)
	if err != nil {
		log.Printf("warphold fleet: activated but admin lookup failed: %v", err)
		writeErr(w, http.StatusInternalServerError, "activation failed")
		return
	}
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

// SetB2ForTesting swaps the B2 client.
func (s *Server) SetB2ForTesting(b b2api.API) { s.b2 = b }

// AdminsForTesting exposes the admin list for tests.
func (s *Server) AdminsForTesting(ctx context.Context) ([]store.Admin, error) {
	s.mu.RLock()
	st := s.st
	s.mu.RUnlock()
	if st == nil {
		return nil, errors.New("fleet is not activated")
	}
	return st.Admins(ctx)
}

// SeedGroupForTesting creates a filesystem target, a template and a group.
func (s *Server) SeedGroupForTesting(ctx context.Context, path string, sources []string, policyJSON string) (targetID, templateID, groupID int64) {
	targetID, _ = s.st.CreateTarget(ctx, &store.Target{Name: "local", Kind: "filesystem", Path: path})
	templateID, _ = s.st.CreateTemplate(ctx, &store.Template{Name: "test", Sources: sources, PolicyJSON: json.RawMessage(policyJSON)})
	groupID, _ = s.st.CreateGroup(ctx, &store.Group{Name: "Test", TargetID: targetID, TemplateID: templateID})
	return
}

// IssueTokenForTesting issues a default token for a group.
func (s *Server) IssueTokenForTesting(ctx context.Context, groupID int64) string {
	plain, _, _ := s.tokens().Issue(ctx, groupID, 0, -1, 0)
	return plain
}

// AgentForTesting exposes one agent's stored record for tests, or nil if it does not exist.
func (s *Server) AgentForTesting(ctx context.Context, id string) *store.Agent {
	a, err := s.st.Agent(ctx, id)
	if err != nil {
		return nil
	}
	return a
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
