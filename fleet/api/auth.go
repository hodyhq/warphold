package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

// HashPassword hashes with argon2id in PHC string format.
func HashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return "", err
	}
	h := argon2.IDKey([]byte(pw), salt, 3, 64*1024, 4, 32)
	return fmt.Sprintf("$argon2id$v=19$m=65536,t=3,p=4$%s$%s",
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(h)), nil
}

// Parameters are fixed (t=3, m=64MiB, p=4) and not read back from the PHC string; bump both HashPassword and here together if they ever change.
// VerifyPassword checks pw against a HashPassword string in constant time.
func VerifyPassword(pw, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[4])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[5])
	if err1 != nil || err2 != nil {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, 3, 64*1024, 4, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

type limiter struct {
	mu   sync.Mutex
	max  int
	per  time.Duration
	hits map[string][]time.Time
	now  func() time.Time
}

func newLimiter(max int, per time.Duration) *limiter {
	return &limiter{max: max, per: per, hits: map[string][]time.Time{}, now: time.Now}
}

// maxLimiterKeys caps the login limiter's map. Without it every client IP
// that ever logged in keeps an entry forever, because allow only prunes the
// key it is called with.
const maxLimiterKeys = 10000

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if len(l.hits) > maxLimiterKeys {
		for k, hits := range l.hits {
			if len(hits) == 0 || now.Sub(hits[len(hits)-1]) >= l.per {
				delete(l.hits, k)
			}
		}
		// Expiry alone does not bound the map: a distributed flood keeps every
		// entry active. Evict least-recently-used keys until back under the cap.
		// ponytail: linear scan per eviction; the map grows one key per call, so
		// at most one eviction runs per call. Use a heap only if that changes.
		for len(l.hits) > maxLimiterKeys {
			var lruKey string
			var lru time.Time
			found := false
			for k, hits := range l.hits {
				var last time.Time
				if len(hits) > 0 {
					last = hits[len(hits)-1]
				}
				if !found || last.Before(lru) {
					lruKey, lru, found = k, last, true
				}
			}
			if !found {
				break
			}
			delete(l.hits, lruKey)
		}
	}
	var keep []time.Time
	for _, t := range l.hits[key] {
		if now.Sub(t) < l.per {
			keep = append(keep, t)
		}
	}
	if len(keep) == 0 {
		delete(l.hits, key)
	}
	if len(keep) >= l.max {
		l.hits[key] = keep
		return false
	}
	l.hits[key] = append(keep, now)
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// requireAdmin gates a handler behind a live server-side session and, for
// state-changing methods, the CSRF double submit. The session is resolved
// against the store on every request, so a revoked or deleted admin's cookie
// fails on its very next call, and the handler reads the admin id out of the
// request context with adminFrom.
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := s.currentSession(r)
		if sess == nil {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		requireCSRF(next)(w, r.WithContext(context.WithValue(r.Context(), sessionCtxKey, sess)))
	}
}

// dummyPWHash is a real argon2id hash of a fixed password, used by handleLogin
// to spend the same verification cost on an unknown email as on a known one.
// It is computed lazily, on first login rather than at init, so importing this
// package does not cost every binary a 64MiB argon2id run at startup. A failed
// hash attempt is never cached: caching "" would make VerifyPassword
// short-circuit before running argon2id, reintroducing the timing difference
// this exists to hide.
var (
	dummyPWHashMu  sync.Mutex
	dummyPWHashVal string
)

func dummyPWHash() string {
	dummyPWHashMu.Lock()
	defer dummyPWHashMu.Unlock()
	if dummyPWHashVal == "" {
		if h, err := HashPassword("warphold-login-timing-equalizer"); err == nil {
			dummyPWHashVal = h
		}
	}
	return dummyPWHashVal
}
