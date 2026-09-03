package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/store"
	"github.com/kopia/kopia/internal/clock"
)

// seedAgent creates the target/template/group/agent chain a device key needs.
func seedAgent(t *testing.T, s *store.Store, id string) time.Time {
	t.Helper()
	ctx := context.Background()
	now := clock.Now().UTC().Truncate(time.Second)
	tid, err := s.CreateTarget(ctx, &store.Target{Name: "hosted", Kind: "hosted", StorageMode: "disk", Path: "/srv/warphold/hosted", CreatedAt: now})
	require.NoError(t, err)
	tpl, err := s.CreateTemplate(ctx, &store.Template{Name: "default", Sources: []string{"~"}, PolicyJSON: json.RawMessage(`{}`), CreatedAt: now})
	require.NoError(t, err)
	gid, err := s.CreateGroup(ctx, &store.Group{Name: "Laptops", TargetID: tid, TemplateID: tpl, CreatedAt: now})
	require.NoError(t, err)
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: id, Name: id, Hostname: id, OS: "linux", Arch: "amd64", Scope: "user", GroupID: gid,
		BearerHash: []byte("hash-" + id), SealedBundle: []byte("sealed"), EnrolledAt: now,
	}))
	return now
}

func TestDeviceKeysRoundTrip(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := seedAgent(t, s, "agent-1")

	k := &store.DeviceKey{AccessKeyID: "WHAAAAAAAAAAAAAAAAAA", AgentID: "agent-1", SealedSecret: []byte("sealed-secret"), Prefix: "agent-1/", CreatedAt: now}
	require.NoError(t, s.CreateDeviceKey(ctx, k))

	got, err := s.DeviceKey(ctx, "WHAAAAAAAAAAAAAAAAAA")
	require.NoError(t, err)
	require.Equal(t, "agent-1", got.AgentID)
	require.Equal(t, []byte("sealed-secret"), got.SealedSecret)
	require.Equal(t, "agent-1/", got.Prefix)
	require.False(t, got.ReadOnly)
	require.Equal(t, now, got.CreatedAt)
	require.Nil(t, got.DisabledAt)

	_, err = s.DeviceKey(ctx, "WHNOPE")
	require.ErrorIs(t, err, store.ErrNotFound)

	// the same access key id cannot be issued twice
	require.Error(t, s.CreateDeviceKey(ctx, k))
}

func TestDeviceKeysReadOnlyAndPerAgent(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := seedAgent(t, s, "agent-1")

	require.NoError(t, s.CreateDeviceKey(ctx, &store.DeviceKey{AccessKeyID: "WH1", AgentID: "agent-1", SealedSecret: []byte("a"), Prefix: "agent-1/", CreatedAt: now}))
	require.NoError(t, s.CreateDeviceKey(ctx, &store.DeviceKey{AccessKeyID: "WH2", AgentID: "agent-1", SealedSecret: []byte("b"), Prefix: "agent-1/", ReadOnly: true, CreatedAt: now.Add(time.Second)}))

	ro, err := s.DeviceKey(ctx, "WH2")
	require.NoError(t, err)
	require.True(t, ro.ReadOnly)

	all, err := s.DeviceKeysForAgent(ctx, "agent-1")
	require.NoError(t, err)
	require.Len(t, all, 2)
	require.Equal(t, "WH1", all[0].AccessKeyID)

	none, err := s.DeviceKeysForAgent(ctx, "agent-nope")
	require.NoError(t, err)
	require.Empty(t, none)
}

func TestDisableDeviceKeysForAgent(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := seedAgent(t, s, "agent-1")
	seedAgent(t, s, "agent-2")

	require.NoError(t, s.CreateDeviceKey(ctx, &store.DeviceKey{AccessKeyID: "WH1", AgentID: "agent-1", SealedSecret: []byte("a"), Prefix: "agent-1/", CreatedAt: now}))
	require.NoError(t, s.CreateDeviceKey(ctx, &store.DeviceKey{AccessKeyID: "WH2", AgentID: "agent-1", SealedSecret: []byte("b"), Prefix: "agent-1/", CreatedAt: now}))
	require.NoError(t, s.CreateDeviceKey(ctx, &store.DeviceKey{AccessKeyID: "WH3", AgentID: "agent-2", SealedSecret: []byte("c"), Prefix: "agent-2/", CreatedAt: now}))

	at := now.Add(time.Hour)
	n, err := s.DisableDeviceKeysForAgent(ctx, "agent-1", at)
	require.NoError(t, err)
	require.EqualValues(t, 2, n)

	// a disabled key no longer resolves
	_, err = s.DeviceKey(ctx, "WH1")
	require.ErrorIs(t, err, store.ErrNotFound)

	// but is still listed, with disabled_at set, so rotation can re-seal it
	all, err := s.DeviceKeysForAgent(ctx, "agent-1")
	require.NoError(t, err)
	require.Len(t, all, 2)
	require.NotNil(t, all[0].DisabledAt)
	require.Equal(t, at, *all[0].DisabledAt)

	// another agent's key is untouched
	other, err := s.DeviceKey(ctx, "WH3")
	require.NoError(t, err)
	require.Nil(t, other.DisabledAt)

	// disabling again is a no-op
	n, err = s.DisableDeviceKeysForAgent(ctx, "agent-1", at)
	require.NoError(t, err)
	require.Zero(t, n)
}
