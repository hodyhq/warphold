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
	"github.com/kopia/kopia/fleet/jobs"
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
	login *limiter
	// nowFn is the server clock, read through now() under mu so
	// SetNowForTesting can move it between requests without racing handlers.
	nowFn func() time.Time
	b2    b2api.API
	cloud cloudAPI

	// sched runs the scheduled jobs (spec §10). It exists only while the Fleet
	// is activated: load starts it, Close stops it, so no job ever runs against
	// a Fleet that has no store or sealing key.
	sched *jobs.Scheduler
	// closed is set by Close instead of nilling st: handlers read st through
	// store() and would otherwise have to re-check for nil between every
	// call. A closed *sql.DB returns "database is closed" from each query,
	// which the handlers already surface as an error.
	closed bool

	// instanceMu serializes the lazy generation of instance_id, so two
	// concurrent status probes cannot each mint one.
	instanceMu sync.Mutex
	// csrfWarnOnce keeps the "public_url is unset, origin check disabled"
	// warning to one line per server rather than one per request.
	csrfWarnOnce sync.Once

	// setupTokenPath and setupToken gate POST /activate before the Fleet is
	// activated (see handleActivate); both are cleared once activation succeeds.
	setupTokenPath string
	setupToken     string

	// gwDeps carries the device-facing S3 gateway, built on first use because
	// it needs the store and sealing key that activation creates.
	gwDeps gatewayDeps

	// tp* memoize the parsed trusted_proxies setting; the login limiter reads
	// it per attempt and must not turn a flood into database reads.
	tpMu     sync.Mutex
	tpNets   []net.IPNet
	tpLoaded bool
}

// New creates a Server for stateDir; if Fleet was activated before, its state is loaded.
func New(stateDir string) *Server {
	s := &Server{paths: fleet.PathsFor(stateDir), login: newLimiter(loginMaxAttempts, loginWindow), nowFn: time.Now, b2: b2api.New(nil), cloud: gatewayCloud{}}
	// A missing key file just means "never activated"; anything else (bad
	// permissions, a corrupt DB) must be loud, because the server would
	// otherwise report "not activated" and print the setup-token path while
	// real state sits on disk. Activate refuses to overwrite it.
	if err := s.load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("warphold fleet: cannot load state from %s: %v", stateDir, err)
	}
	if !s.Activated() {
		// Without the setup token POST /activate cannot be authorized, so a
		// failure here is the difference between "not activated yet" and "can
		// never be activated"; it has to be visible in the log.
		if path, token, err := ensureSetupToken(s.paths.StateDir); err != nil {
			log.Printf("warphold fleet: cannot create setup token in %s: %v", s.paths.StateDir, err)
		} else {
			s.mu.Lock()
			s.setupTokenPath, s.setupToken = path, token
			s.mu.Unlock()
		}
	}
	return s
}

// ensureSetupToken returns the setup token in dir, creating it (32 random
// bytes, hex-encoded, mode 0600 in a 0700 dir) if it does not already exist.
// An existing file whose contents are empty or whitespace is regenerated: a
// truncated write (or a hand-cleared file) would otherwise leave Fleet with an
// empty setup token, which compares equal to an empty submission.
func ensureSetupToken(dir string) (path, token string, err error) {
	path = filepath.Join(dir, setupTokenFile)
	if b, err := os.ReadFile(path); err == nil {
		if tok := strings.TrimSpace(string(b)); tok != "" {
			return path, tok, nil
		}
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
	s.invalidateTrustedProxies()
	s.mu.Lock()
	s.key, s.st, s.closed = key, st, false
	s.mu.Unlock()
	s.startJobs()
	return nil
}

// startJobs starts the job scheduler for the now-activated Fleet, replacing any
// previous one. Only load calls it, so an unactivated Fleet runs nothing.
//
// The whole thing runs under s.mu, including Start: Close takes the same lock,
// so it can neither pick the store between this swap and the start - which
// would leave a scheduler running against a closed database with nothing left
// to stop it - nor start one on an already closed Server. Start only spawns a
// goroutine and never calls back into the Server, so holding the lock is cheap
// and cannot deadlock.
func (s *Server) startJobs() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st == nil || s.closed {
		return
	}
	old := s.sched
	s.sched = jobs.NewScheduler(s.st, map[string]jobs.Runner{"mirror": jobs.Mirror(s.st, s.key)}, jobs.DefaultTick)
	s.sched.Start(context.Background())
	if old != nil {
		// In a goroutine: Stop waits for the running job, which must not
		// freeze every handler waiting on s.mu.
		go old.Stop()
	}
}

// now returns the server clock.
func (s *Server) now() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.nowFn()
}

// SetNowForTesting overrides the server clock, so a test can step past a
// session TTL instead of sleeping for it.
func (s *Server) SetNowForTesting(f func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nowFn = f
}

// Activated reports whether the store and key are loaded.
func (s *Server) Activated() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.st != nil && !s.closed
}

// Close stops the scheduler and closes the store. The scheduler is stopped
// first and outside the lock: Stop waits for the running job, which is still
// using the store, and a job must never see a closed database.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.st == nil || s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	st, sched := s.st, s.sched
	s.sched = nil
	s.mu.Unlock()
	if sched != nil {
		sched.Stop()
	}
	return st.Close()
}

// store returns the active store, or nil before activation. load and Close
// write s.st under s.mu, so every handler must read it through this accessor
// rather than touching the field.
func (s *Server) store() *store.Store {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.st
}

// sealKey returns the sealing key; the zero Key before activation.
func (s *Server) sealKey() seal.Key {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.key
}

// Activate creates the DB, seals with a key derived from passphrase, and creates the first admin.
func (s *Server) Activate(ctx context.Context, passphrase, email, password string) error {
	s.activateMu.Lock()
	defer s.activateMu.Unlock()

	if s.Activated() {
		return ErrAlreadyActivated
	}
	// Not activated can also mean "state exists but load() failed" (see New).
	// Writing a fresh seal.key here would derive a new key from a new salt and
	// permanently destroy every escrowed repo password and B2 key.
	for _, p := range []string{s.paths.KeyFile, s.paths.DB} {
		if _, err := os.Stat(p); err == nil {
			return errors.New("fleet state exists but could not be loaded; refusing to overwrite seal.key - fix or remove " + s.paths.StateDir + " first")
		}
	}
	email = normalizeEmail(email)
	if len(passphrase) < 8 || len(password) < 8 || !strings.Contains(email, "@") {
		return ErrInvalidActivation
	}
	salt, err := seal.NewSalt()
	if err != nil {
		return err
	}
	if err := s.writeActivation(ctx, salt, passphrase, email, password); err != nil {
		// Activation is not atomic, so a partial run must not leave a seal.key
		// or a half-built DB behind: the guard above keys on exactly those two
		// files and would reject every retry with "state exists but could not
		// be loaded" until an operator cleared the directory by hand.
		s.removePartialState()
		return err
	}
	s.clearSetupToken()
	return nil
}

// writeActivation performs the write half of Activate: the DB, the first
// admin and the settings first, then - last - the seal.key that Activate's
// overwrite guard keys on, then loads the result.
func (s *Server) writeActivation(ctx context.Context, salt []byte, passphrase, email, password string) error {
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
	if _, err := st.CreateAdmin(ctx, email, pwHash, s.now()); err != nil {
		return err
	}
	if err := st.SetSetting(ctx, "seal_salt", hex.EncodeToString(salt)); err != nil {
		return err
	}
	if err := st.Close(); err != nil {
		return err
	}
	st = nil
	// seal.key goes last, once the DB and the admin row exist.
	if err := seal.WriteKeyFile(s.paths.KeyFile, seal.Derive(passphrase, salt)); err != nil {
		return err
	}
	return s.load()
}

// removePartialState deletes whatever a failed activation created, so the next
// attempt starts from "never activated" instead of tripping Activate's guard.
func (s *Server) removePartialState() {
	for _, p := range []string{s.paths.KeyFile, s.paths.DB, s.paths.DB + "-wal", s.paths.DB + "-shm"} {
		_ = os.Remove(p)
	}
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
	// Every Fleet route goes through requireHost: once public_url is set, a
	// request arriving under any other name (bar loopback) is misrouted.
	m.HandleFunc("/api/v1/fleet/status", s.requireHost(s.handleStatus)).Methods(http.MethodGet)
	m.HandleFunc("/api/v1/fleet/activate", s.requireHost(s.handleActivate)).Methods(http.MethodPost)
	m.HandleFunc("/api/v1/fleet/session", s.requireHost(s.handleLogin)).Methods(http.MethodPost)
	m.HandleFunc("/api/v1/fleet/session", s.requireHost(s.handleLogout)).Methods(http.MethodDelete)
	s.mountAdmin(m)    // Task 7
	s.mountAgent(m)    // Tasks 11, 14
	s.mountDownload(m) // Task 2
	s.mountGateway(m)  // Plan 3, Task 5 - must precede the SPA catch-all
}

func decode(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(v)
}

// handleStatus is the unauthenticated probe target: it is what a reverse
// proxy, a load balancer and the public_url validation all fetch. instance_id
// is what lets that validation tell this Fleet apart from another one behind
// the same URL; it is opaque and authorizes nothing.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"activated": s.Activated()}
	if s.Activated() {
		if id, err := s.instanceID(r.Context()); err == nil {
			out["instance_id"] = id
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// setupAllowed reports whether r may call POST /activate: it must carry the
// one-time setup token, with no loopback exception - RemoteAddr is the proxy
// behind a reverse proxy, so "from loopback" says nothing about who is
// calling. This only gates the HTTP endpoint; the `fleet activate` CLI calls
// Activate directly and covers local use.
func (s *Server) setupAllowed(r *http.Request) bool {
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
		writeErr(w, http.StatusForbidden, "activation requires the "+setupTokenHeader+" header")
		return
	}
	var in struct {
		Passphrase, Email, Password string
		// PublicURL is optional here so the setup wizard can set it in the
		// same call that activates; it is validated for syntax only, because
		// the end-to-end probe needs a running, activated Fleet to answer.
		PublicURL string `json:"public_url"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed body")
		return
	}
	// The URL is checked before anything is written: activation happens once,
	// and finishing it with a rejected public_url would leave the wizard with
	// an activated Fleet and no way to redo the step it thought it just did.
	if in.PublicURL != "" {
		if _, err := parsePublicURL(in.PublicURL); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
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
	if in.PublicURL != "" {
		if err := s.SetPublicURL(r.Context(), in.PublicURL); err != nil {
			// The syntax was checked above, so this is a store failure, and
			// the Fleet is now activated without a public URL. Say so rather
			// than reporting a success the operator would have to discover
			// was partial.
			log.Printf("warphold fleet: activated but public_url could not be stored: %v", err)
			writeErr(w, http.StatusInternalServerError, "fleet activated, but the public URL could not be stored; set it in Settings")
			return
		}
	}
	a, err := s.store().AdminByEmail(r.Context(), normalizeEmail(in.Email))
	if err != nil {
		log.Printf("warphold fleet: activated but admin lookup failed: %v", err)
		writeErr(w, http.StatusInternalServerError, "activation failed")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"admin_id": a.ID})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.Activated() {
		writeErr(w, http.StatusConflict, "fleet is not activated")
		return
	}
	if pu, _ := s.PublicURL(r.Context()); pu != nil {
		if !originAllowed(r, pu) {
			writeErr(w, http.StatusForbidden, "request origin does not match the configured public URL")
			return
		}
		// Signing in over plain HTTP when the fleet is published on https
		// would set a Secure cookie the browser then drops, leaving the admin
		// in a login loop with no visible cause. Loopback is exempt, since
		// that is the local operator, not a browser on the public URL.
		if pu.Scheme == "https" && !requestIsHTTPS(r) && !isLoopbackHost(hostOnly(r.Host)) {
			writeErr(w, http.StatusBadRequest, "reach the fleet over https")
			return
		}
	}
	if !s.login.allow(s.clientIP(r)) {
		writeErr(w, http.StatusTooManyRequests, "too many attempts, wait a minute")
		return
	}
	var in struct{ Email, Password string }
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed body")
		return
	}
	st := s.store()
	if st == nil {
		writeErr(w, http.StatusConflict, "fleet is not activated")
		return
	}
	a, err := st.AdminByEmail(r.Context(), normalizeEmail(in.Email))
	hash := dummyPWHash()
	if err == nil {
		hash = a.PWHash
	}
	// VerifyPassword runs on every attempt, against a fixed dummy hash when the
	// email is unknown. Returning early on the lookup failure skipped a 64MiB
	// argon2id verification, so an unregistered address answered measurably
	// faster than a wrong password and login doubled as an account enumerator.
	if ok := VerifyPassword(in.Password, hash); !ok || err != nil {
		writeErr(w, http.StatusUnauthorized, "wrong email or password")
		return
	}
	if err := s.startSession(r.Context(), w, r, a.ID); err != nil {
		adminFailed(w, "create session", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleLogout revokes the session row behind the cookie, so the cookie is
// dead server-side even if the client keeps a copy. Signing out is a state
// change, so a live session must carry the CSRF token; a request with no live
// session has nothing to revoke and just gets its cookies cleared.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if sess := s.currentSession(r); sess != nil {
		// Origin first, token second: a foreign origin must never learn
		// whether its guessed token was right (same order as requireCSRF).
		if pu, _ := s.PublicURL(r.Context()); !originAllowed(r, pu) {
			writeErr(w, http.StatusForbidden, "request origin does not match the configured public URL")
			return
		}
		if !csrfOK(r) {
			writeErr(w, http.StatusForbidden, "missing or invalid "+csrfHeader+" header")
			return
		}
		if err := s.store().RevokeSession(r.Context(), sess.ID, s.now()); err != nil {
			adminFailed(w, "revoke session", err)
			return
		}
	}
	s.clearAuthCookies(w, r)
	w.WriteHeader(http.StatusNoContent)
}

// SetupTokenPathForTesting returns the path of the one-time setup-token file,
// or "" once activation has cleared it.
func (s *Server) SetupTokenPathForTesting() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.setupTokenPath
}

// SetB2ForTesting swaps the B2 client.
func (s *Server) SetB2ForTesting(b b2api.API) { s.b2 = b }

// SetCloudForTesting swaps the S3-compatible bucket verifier, so a test never
// reaches a real provider.
func (s *Server) SetCloudForTesting(c cloudAPI) { s.cloud = c }

// AdminsForTesting exposes the admin list for tests.
func (s *Server) AdminsForTesting(ctx context.Context) ([]store.Admin, error) {
	st := s.store()
	if st == nil {
		return nil, errors.New("fleet is not activated")
	}
	return st.Admins(ctx)
}

// SeedGroupForTesting creates a filesystem target, a template and a group.
func (s *Server) SeedGroupForTesting(ctx context.Context, path string, sources []string, policyJSON string) (targetID, templateID, groupID int64) {
	st, now := s.store(), s.now()
	targetID, _ = st.CreateTarget(ctx, &store.Target{Name: "local", Kind: "filesystem", Path: path, CreatedAt: now})
	templateID, _ = st.CreateTemplate(ctx, &store.Template{Name: "test", Sources: sources, PolicyJSON: json.RawMessage(policyJSON), CreatedAt: now})
	groupID, _ = st.CreateGroup(ctx, &store.Group{Name: "Test", TargetID: targetID, TemplateID: templateID, CreatedAt: now})
	return
}

// IssueTokenForTesting issues a default token for a group.
func (s *Server) IssueTokenForTesting(ctx context.Context, groupID int64) string {
	plain, _, _ := s.tokens().Issue(ctx, groupID, 0, -1, 0)
	return plain
}

// PendingReapsForTesting exposes the reap jobs queued for an agent.
func (s *Server) PendingReapsForTesting(ctx context.Context, agentID string) []time.Time {
	at, err := s.store().PendingReaps(ctx, agentID)
	if err != nil {
		return nil
	}
	return at
}

// AgentForTesting exposes one agent's stored record for tests, or nil if it does not exist.
func (s *Server) AgentForTesting(ctx context.Context, id string) *store.Agent {
	a, err := s.store().Agent(ctx, id)
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
