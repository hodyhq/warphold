package api

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

const sessionCookie = "wh_session"

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

type sessions struct {
	secret []byte
	ttl    time.Duration
	now    func() time.Time
}

func newSessions(secret []byte, ttl time.Duration) *sessions {
	return &sessions{secret: secret, ttl: ttl, now: time.Now}
}

func (s *sessions) sign(body string) string {
	m := hmac.New(sha256.New, s.secret)
	m.Write([]byte(body))
	return hex.EncodeToString(m.Sum(nil))
}

func (s *sessions) issue(adminID int64) string {
	body := strconv.FormatInt(adminID, 10) + "." + strconv.FormatInt(s.now().Add(s.ttl).Unix(), 10)
	return body + "." + s.sign(body)
}

func (s *sessions) verify(tok string) (int64, bool) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return 0, false
	}
	body := parts[0] + "." + parts[1]
	if !hmac.Equal([]byte(s.sign(body)), []byte(parts[2])) {
		return 0, false
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || s.now().Unix() > exp {
		return 0, false
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	return id, err == nil
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

// requireAdmin gates a handler behind a valid session cookie.
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		sess := s.signer()
		if sess == nil {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if _, ok := sess.verify(c.Value); !ok {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r)
	}
}
