package gateway

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/seal"
	"github.com/kopia/kopia/fleet/store"
	"github.com/kopia/kopia/internal/clock"
)

const (
	testAgent  = "agent-1"
	testKeyID  = "WHAAAAAAAAAAAAAAAAAA"
	testSecret = "s3cret-not-logged"
)

func testKeys(t *testing.T) (*store.Store, seal.Key) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "fleet.db"))
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })

	ctx := context.Background()
	now := clock.Now().UTC().Truncate(time.Second)
	tid, err := s.CreateTarget(ctx, &store.Target{Name: "hosted", Kind: "hosted", StorageMode: "disk", CreatedAt: now})
	require.NoError(t, err)
	tpl, err := s.CreateTemplate(ctx, &store.Template{Name: "d", Sources: []string{"~"}, PolicyJSON: json.RawMessage(`{}`), CreatedAt: now})
	require.NoError(t, err)
	gid, err := s.CreateGroup(ctx, &store.Group{Name: "g", TargetID: tid, TemplateID: tpl, CreatedAt: now})
	require.NoError(t, err)
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: testAgent, Name: testAgent, Hostname: "h", OS: "linux", Arch: "amd64", Scope: "user", GroupID: gid,
		BearerHash: []byte("hash"), SealedBundle: []byte("sealed"), EnrolledAt: now,
	}))

	k := seal.Derive("passphrase", []byte("0123456789abcdef"))
	sealed, err := k.Seal([]byte(testSecret))
	require.NoError(t, err)
	require.NoError(t, s.CreateDeviceKey(ctx, &store.DeviceKey{
		AccessKeyID: testKeyID, AgentID: testAgent, SealedSecret: sealed, Prefix: testAgent + "/", CreatedAt: now,
	}))
	return s, k
}

func TestKeysLookupAndCache(t *testing.T) {
	s, k := testKeys(t)
	ctx := context.Background()
	keys := NewKeys(s, k)

	agentID, prefix, secret, readOnly, ok := keys.Lookup(ctx, testKeyID)
	require.True(t, ok)
	require.Equal(t, testAgent, agentID)
	require.Equal(t, testAgent+"/", prefix)
	require.Equal(t, testSecret, secret)
	require.False(t, readOnly)

	// disable the key in the store: a cached lookup must not read the store, so
	// it still resolves.
	n, err := s.DisableDeviceKeysForAgent(ctx, testAgent, clock.Now())
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	_, _, secret, _, ok = keys.Lookup(ctx, testKeyID)
	require.True(t, ok, "should still be served from cache")
	require.Equal(t, testSecret, secret)

	// invalidating by agent id forces the re-read, and the revoke takes effect.
	keys.Invalidate(testAgent)
	_, _, _, _, ok = keys.Lookup(ctx, testKeyID)
	require.False(t, ok, "revoked key must not resolve after invalidation")
}

func TestKeysCacheExpires(t *testing.T) {
	s, k := testKeys(t)
	ctx := context.Background()
	keys := NewKeys(s, k)

	now := clock.Now()
	keys.SetNowForTesting(func() time.Time { return now })
	_, _, _, _, ok := keys.Lookup(ctx, testKeyID)
	require.True(t, ok)

	_, err := s.DisableDeviceKeysForAgent(ctx, testAgent, now)
	require.NoError(t, err)

	_, _, _, _, ok = keys.Lookup(ctx, testKeyID)
	require.True(t, ok, "still inside the TTL")

	keys.SetNowForTesting(func() time.Time { return now.Add(cacheTTL + time.Second) })
	_, _, _, _, ok = keys.Lookup(ctx, testKeyID)
	require.False(t, ok, "entry expired, store says the key is disabled")
}

func TestKeysUnknownAndWrongSealKey(t *testing.T) {
	s, _ := testKeys(t)
	ctx := context.Background()

	wrong := NewKeys(s, seal.Derive("wrong", []byte("0123456789abcdef")))
	_, _, secret, _, ok := wrong.Lookup(ctx, testKeyID)
	require.False(t, ok, "a wrong sealing key must not resolve")
	require.Empty(t, secret)

	_, _, _, _, ok = wrong.Lookup(ctx, "WHUNKNOWN")
	require.False(t, ok)

	wrong.Invalidate("nobody") // must not panic on an empty cache
}
