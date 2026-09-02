package api

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPasswordHashVerify(t *testing.T) {
	h, err := HashPassword("s3cret")
	require.NoError(t, err)
	require.Contains(t, h, "$argon2id$")
	require.True(t, VerifyPassword("s3cret", h))
	require.False(t, VerifyPassword("wrong", h))
	require.False(t, VerifyPassword("s3cret", "garbage"))
}

func TestSessionsIssueVerifyExpire(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s := newSessions([]byte("secret"), time.Hour)
	s.now = func() time.Time { return now }
	tok := s.issue(42)
	id, ok := s.verify(tok)
	require.True(t, ok)
	require.EqualValues(t, 42, id)
	_, ok = s.verify(tok + "x")
	require.False(t, ok)
	s.now = func() time.Time { return now.Add(2 * time.Hour) }
	_, ok = s.verify(tok)
	require.False(t, ok, "expired")
	other := newSessions([]byte("other"), time.Hour)
	_, ok = other.verify(tok)
	require.False(t, ok, "different secret")
}

func TestLimiter(t *testing.T) {
	l := newLimiter(3, time.Minute)
	now := time.Unix(0, 0)
	l.now = func() time.Time { return now }
	for range 3 {
		require.True(t, l.allow("1.2.3.4"))
	}
	require.False(t, l.allow("1.2.3.4"))
	require.True(t, l.allow("5.6.7.8"))
	now = now.Add(61 * time.Second)
	require.True(t, l.allow("1.2.3.4"))
}
