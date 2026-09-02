// Package enroll implements enrollment tokens and per-agent provisioning.
package enroll

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"time"

	"github.com/kopia/kopia/fleet/store"
)

const (
	// DefaultTTL is the token lifetime when none is given.
	DefaultTTL = time.Hour
	// MaxTTL is the longest an admin may set.
	MaxTTL = 30 * 24 * time.Hour
)

var (
	// ErrTokenInvalid is returned when a token cannot be consumed.
	ErrTokenInvalid = errors.New("enrollment token is invalid, expired, revoked, or used up")
	// ErrTTLTooLong is returned when Issue is asked for a TTL beyond MaxTTL.
	ErrTTLTooLong = errors.New("token lifetime exceeds 30 days")
)

// NewToken returns a random token and its hash.
func NewToken() (string, []byte, error) {
	b := make([]byte, 24)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", nil, err
	}
	plain := "wh_" + base64.RawURLEncoding.EncodeToString(b)
	return plain, HashToken(plain), nil
}

// HashToken hashes a token for storage/lookup.
func HashToken(plain string) []byte {
	h := sha256.Sum256([]byte(plain))
	return h[:]
}

// Tokens issues and consumes enrollment tokens.
type Tokens struct {
	st  *store.Store
	now func() time.Time
}

// NewTokens wraps a store.
func NewTokens(st *store.Store) *Tokens { return &Tokens{st: st, now: time.Now} }

// SetNowForTesting overrides the clock.
func (t *Tokens) SetNowForTesting(f func() time.Time) { t.now = f }

// Issue creates a token for a group. ttl<=0 means DefaultTTL; maxUses<0 means 1; 0 means unlimited.
func (t *Tokens) Issue(ctx context.Context, groupID int64, ttl time.Duration, maxUses int, by int64) (string, *store.Token, error) {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if ttl > MaxTTL {
		return "", nil, ErrTTLTooLong
	}
	if maxUses < 0 {
		maxUses = 1
	}
	plain, hash, err := NewToken()
	if err != nil {
		return "", nil, err
	}
	tok := &store.Token{Hash: hash, GroupID: groupID, ExpiresAt: t.now().Add(ttl), MaxUses: maxUses, CreatedBy: by}
	id, err := t.st.CreateToken(ctx, tok)
	if err != nil {
		return "", nil, err
	}
	tok.ID = id
	return plain, tok, nil
}

// Consume validates a token and counts one use. Validation and the increment
// are one conditional UPDATE in the database (store.ConsumeToken), not a
// read-check-write here: a fresh Tokens is built per request, so a mutex on
// this struct would guard nothing and two concurrent enrollments could both
// spend a single-use token.
func (t *Tokens) Consume(ctx context.Context, plain string) (*store.Token, error) {
	hash := HashToken(plain)

	tok, err := t.st.TokenByHash(ctx, hash)
	if err != nil {
		return nil, ErrTokenInvalid
	}

	ok, err := t.st.ConsumeToken(ctx, tok.ID, t.now())
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, ErrTokenInvalid
	}

	// Re-read so the returned token carries the post-update use count.
	if fresh, err := t.st.TokenByHash(ctx, hash); err == nil {
		tok = fresh
	}

	return tok, nil
}
