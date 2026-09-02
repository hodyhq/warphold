package store_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/store"
)

func openTemp(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "fleet.db"))
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpenIsIdempotent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fleet.db")
	s, err := store.Open(p)
	require.NoError(t, err)
	require.NoError(t, s.Close())
	s, err = store.Open(p) // schema already applied
	require.NoError(t, err)
	require.NoError(t, s.Close())
}

func TestAdminsRoundTrip(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	id, err := s.CreateAdmin(ctx, "hody@hody.dev", "argon2id$fake")
	require.NoError(t, err)
	a, err := s.AdminByEmail(ctx, "hody@hody.dev")
	require.NoError(t, err)
	require.Equal(t, id, a.ID)
	require.Equal(t, "owner", a.Role)
	_, err = s.AdminByEmail(ctx, "nobody@x")
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = s.CreateAdmin(ctx, "hody@hody.dev", "x")
	require.Error(t, err, "email must be unique")
}

func TestGroupChainAndAgents(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	tid, err := s.CreateTarget(ctx, &store.Target{Name: "b2", Kind: "b2", Bucket: "hody-backups", SealedAdminKey: []byte("sealed")})
	require.NoError(t, err)
	tpl, err := s.CreateTemplate(ctx, &store.Template{Name: "Home default", Sources: []string{"~"}, PolicyJSON: json.RawMessage(`{"retention":{"keepHourly":24}}`)})
	require.NoError(t, err)
	gid, err := s.CreateGroup(ctx, &store.Group{Name: "Laptops", TargetID: tid, TemplateID: tpl})
	require.NoError(t, err)

	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{ID: "ag_1", Name: "hody-fw13", Hostname: "fw13", OS: "linux", Arch: "amd64", Scope: "user", GroupID: gid, BearerHash: []byte("h"), SealedBundle: []byte("b"), EnrolledAt: now}))
	a, err := s.AgentByBearerHash(ctx, []byte("h"))
	require.NoError(t, err)
	require.Equal(t, "ag_1", a.ID)
	require.Nil(t, a.LastSeenAt)
	require.NoError(t, s.TouchAgent(ctx, "ag_1", now, "0.1.0", "etag1"))
	a, _ = s.Agent(ctx, "ag_1")
	require.Equal(t, now, a.LastSeenAt.UTC())
	require.Equal(t, "etag1", a.PolicyETag)

	got, err := s.Template(ctx, tpl)
	require.NoError(t, err)
	require.Equal(t, []string{"~"}, got.Sources)
	require.JSONEq(t, `{"retention":{"keepHourly":24}}`, string(got.PolicyJSON))
}

func TestReportsDedupeAndLatest(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	// Setup: create target, template, group, and agent
	tid, _ := s.CreateTarget(ctx, &store.Target{Name: "b2", Kind: "b2", Bucket: "hody-backups", SealedAdminKey: []byte("sealed")})
	tpl, _ := s.CreateTemplate(ctx, &store.Template{Name: "Home", Sources: []string{"~"}, PolicyJSON: []byte(`{}`)})
	gid, _ := s.CreateGroup(ctx, &store.Group{Name: "Laptops", TargetID: tid, TemplateID: tpl})
	s.CreateAgent(ctx, &store.Agent{ID: "ag_1", Name: "hody", Hostname: "fw13", OS: "linux", Arch: "amd64", Scope: "user", GroupID: gid, BearerHash: []byte("h"), SealedBundle: []byte("b"), EnrolledAt: now})

	r := &store.Report{AgentID: "ag_1", TaskID: "t1", Kind: "snapshot", Source: "/home/hody", StartedAt: now.Add(-time.Minute), FinishedAt: now, Status: "ok", Bytes: 10, Files: 2, SnapshotID: "k1"}
	id1, err := s.AddReport(ctx, r)
	require.NoError(t, err)
	id2, err := s.AddReport(ctx, r)
	require.NoError(t, err)
	require.Equal(t, id1, id2, "same (agent, task) must not duplicate")
	_, err = s.AddReport(ctx, &store.Report{AgentID: "ag_1", TaskID: "t2", Kind: "snapshot", Source: "/home/hody", StartedAt: now, FinishedAt: now.Add(time.Minute), Status: "error", Stderr: "kopia: error: boom"})
	require.NoError(t, err)
	latest, err := s.LatestReports(ctx)
	require.NoError(t, err)
	require.Equal(t, "t2", latest["ag_1"].TaskID)
	rs, err := s.ReportsForAgent(ctx, "ag_1", 10)
	require.NoError(t, err)
	require.Len(t, rs, 2)
	require.Equal(t, "t2", rs[0].TaskID)
}

func TestTokensAndCommandsAndSettings(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	// Setup: create target, template, group, and agent
	tid, _ := s.CreateTarget(ctx, &store.Target{Name: "b2", Kind: "b2", Bucket: "hody-backups", SealedAdminKey: []byte("sealed")})
	tpl, _ := s.CreateTemplate(ctx, &store.Template{Name: "Home", Sources: []string{"~"}, PolicyJSON: []byte(`{}`)})
	gid, _ := s.CreateGroup(ctx, &store.Group{Name: "Laptops", TargetID: tid, TemplateID: tpl})
	s.CreateAgent(ctx, &store.Agent{ID: "ag_1", Name: "hody", Hostname: "fw13", OS: "linux", Arch: "amd64", Scope: "user", GroupID: gid, BearerHash: []byte("h"), SealedBundle: []byte("b"), EnrolledAt: now})

	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	id, err := s.CreateToken(ctx, &store.Token{Hash: []byte("th"), GroupID: gid, ExpiresAt: exp, MaxUses: 1})
	require.NoError(t, err)
	tok, err := s.TokenByHash(ctx, []byte("th"))
	require.NoError(t, err)
	require.Equal(t, id, tok.ID)
	require.NoError(t, s.IncrementTokenUses(ctx, id))
	tok, _ = s.TokenByHash(ctx, []byte("th"))
	require.Equal(t, 1, tok.Uses)

	cid, err := s.AddCommand(ctx, &store.Command{AgentID: "ag_1", Kind: "snapshot-now", Source: "/home/hody"})
	require.NoError(t, err)
	pend, err := s.PendingCommands(ctx, "ag_1")
	require.NoError(t, err)
	require.Len(t, pend, 1)
	require.ErrorIs(t, s.AckCommand(ctx, cid, "ag_wrong", time.Now()), store.ErrNotFound, "ack scoped to the wrong agent must not apply")
	require.NoError(t, s.AckCommand(ctx, cid, "ag_1", time.Now()))
	pend, _ = s.PendingCommands(ctx, "ag_1")
	require.Empty(t, pend)

	v, err := s.Setting(ctx, "poll_interval")
	require.NoError(t, err)
	require.Equal(t, "", v)
	require.NoError(t, s.SetSetting(ctx, "poll_interval", "300"))
	v, _ = s.Setting(ctx, "poll_interval")
	require.Equal(t, "300", v)
}
