package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/store"
	"github.com/kopia/kopia/internal/clock"
)

// seedGroup creates a target, a template and a group on them, returning the
// group id alongside both parent ids so a test can point a second group or a
// repoint attempt at a different target.
func seedGroup(t *testing.T, s *store.Store, now time.Time) (gid, tid, tpl int64) {
	t.Helper()
	ctx := context.Background()
	var err error
	tid, err = s.CreateTarget(ctx, &store.Target{Name: "t", Kind: "filesystem", Path: t.TempDir(), CreatedAt: now})
	require.NoError(t, err)
	tpl, err = s.CreateTemplate(ctx, &store.Template{Name: "tpl", Sources: []string{"~"}, PolicyJSON: []byte(`{}`), CreatedAt: now})
	require.NoError(t, err)
	gid, err = s.CreateGroup(ctx, &store.Group{Name: "g", TargetID: tid, TemplateID: tpl, CreatedAt: now})
	require.NoError(t, err)
	return gid, tid, tpl
}

func TestUpdateGroupIsPartial(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := clock.Now().UTC().Truncate(time.Second)
	gid, tid, tpl := seedGroup(t, s, now)

	tid2, err := s.CreateTarget(ctx, &store.Target{Name: "t2", Kind: "filesystem", Path: t.TempDir(), CreatedAt: now})
	require.NoError(t, err)

	newName := "renamed"
	require.NoError(t, s.UpdateGroup(ctx, gid, &newName, nil, nil))
	g, err := s.Group(ctx, gid)
	require.NoError(t, err)
	require.Equal(t, "renamed", g.Name)
	require.Equal(t, tid, g.TargetID, "target untouched by a name-only update")
	require.Equal(t, tpl, g.TemplateID)

	require.NoError(t, s.UpdateGroup(ctx, gid, nil, &tid2, nil))
	g, err = s.Group(ctx, gid)
	require.NoError(t, err)
	require.Equal(t, tid2, g.TargetID)
	require.Equal(t, "renamed", g.Name, "name untouched by a target-only update")

	require.ErrorIs(t, s.UpdateGroup(ctx, 999999, &newName, nil, nil), store.ErrNotFound)
}

func TestGroupHasAgentsCountsRevoked(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := clock.Now().UTC().Truncate(time.Second)
	gid, _, _ := seedGroup(t, s, now)

	has, err := s.GroupHasAgents(ctx, gid)
	require.NoError(t, err)
	require.False(t, has, "a fresh group has never enrolled a device")

	require.NoError(t, s.CreateAgent(ctx, &store.Agent{ID: "a1", Name: "n", Hostname: "h", OS: "linux", Arch: "amd64", Scope: "user", GroupID: gid, BearerHash: []byte("h1"), SealedBundle: []byte("b"), EnrolledAt: now}))
	has, err = s.GroupHasAgents(ctx, gid)
	require.NoError(t, err)
	require.True(t, has)

	require.NoError(t, s.RevokeAgent(ctx, "a1", now))
	has, err = s.GroupHasAgents(ctx, gid)
	require.NoError(t, err)
	require.True(t, has, "a revoked agent's repository still lives on the group's target")
}

func TestDeleteGroup(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := clock.Now().UTC().Truncate(time.Second)

	t.Run("ok when unused", func(t *testing.T) {
		gid, _, _ := seedGroup(t, s, now)
		require.NoError(t, s.DeleteGroup(ctx, gid, now))
		_, err := s.Group(ctx, gid)
		require.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("unknown id", func(t *testing.T) {
		require.ErrorIs(t, s.DeleteGroup(ctx, 999999, now), store.ErrNotFound)
	})

	t.Run("refused with a non-revoked agent", func(t *testing.T) {
		gid, _, _ := seedGroup(t, s, now)
		require.NoError(t, s.CreateAgent(ctx, &store.Agent{ID: "a2", Name: "n", Hostname: "h", OS: "linux", Arch: "amd64", Scope: "user", GroupID: gid, BearerHash: []byte("h2"), SealedBundle: []byte("b"), EnrolledAt: now}))
		require.ErrorIs(t, s.DeleteGroup(ctx, gid, now), store.ErrGroupInUse)
	})

	t.Run("refused once revoked, since its repository history still points here", func(t *testing.T) {
		gid, _, _ := seedGroup(t, s, now)
		require.NoError(t, s.CreateAgent(ctx, &store.Agent{ID: "a3", Name: "n", Hostname: "h", OS: "linux", Arch: "amd64", Scope: "user", GroupID: gid, BearerHash: []byte("h3"), SealedBundle: []byte("b"), EnrolledAt: now}))
		require.NoError(t, s.RevokeAgent(ctx, "a3", now))
		require.ErrorIs(t, s.DeleteGroup(ctx, gid, now), store.ErrGroupInUse)
	})

	t.Run("refused with an unexpired token, ok once it expires", func(t *testing.T) {
		gid, _, _ := seedGroup(t, s, now)
		_, err := s.CreateToken(ctx, &store.Token{Hash: []byte("th1"), GroupID: gid, ExpiresAt: now.Add(time.Hour), MaxUses: 1, CreatedAt: now})
		require.NoError(t, err)
		require.ErrorIs(t, s.DeleteGroup(ctx, gid, now), store.ErrGroupInUse)

		// Past expiry, the stale token is cleaned up as part of the delete.
		require.NoError(t, s.DeleteGroup(ctx, gid, now.Add(2*time.Hour)))
	})

	t.Run("revoked token does not block", func(t *testing.T) {
		gid, _, _ := seedGroup(t, s, now)
		tokID, err := s.CreateToken(ctx, &store.Token{Hash: []byte("th2"), GroupID: gid, ExpiresAt: now.Add(time.Hour), MaxUses: 1, CreatedAt: now})
		require.NoError(t, err)
		require.NoError(t, s.RevokeToken(ctx, tokID, now))
		require.NoError(t, s.DeleteGroup(ctx, gid, now))
	})
}
