package store_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/store"
	"github.com/kopia/kopia/internal/clock"
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
	now := clock.Now()
	id, err := s.CreateAdmin(ctx, "hody@hody.dev", "argon2id$fake", now)
	require.NoError(t, err)
	a, err := s.AdminByEmail(ctx, "hody@hody.dev")
	require.NoError(t, err)
	require.Equal(t, id, a.ID)
	require.Equal(t, "owner", a.Role)
	_, err = s.AdminByEmail(ctx, "nobody@x")
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = s.CreateAdmin(ctx, "hody@hody.dev", "x", now)
	require.Error(t, err, "email must be unique")
}

func TestGroupChainAndAgents(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := clock.Now().UTC().Truncate(time.Second)
	tid, err := s.CreateTarget(ctx, &store.Target{Name: "b2", Kind: "b2", Bucket: "hody-backups", SealedAdminKey: []byte("sealed"), CreatedAt: now})
	require.NoError(t, err)
	tpl, err := s.CreateTemplate(ctx, &store.Template{Name: "Home default", Sources: []string{"~"}, PolicyJSON: json.RawMessage(`{"retention":{"keepHourly":24}}`), CreatedAt: now})
	require.NoError(t, err)
	gid, err := s.CreateGroup(ctx, &store.Group{Name: "Laptops", TargetID: tid, TemplateID: tpl, CreatedAt: now})
	require.NoError(t, err)

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
	now := clock.Now().UTC().Truncate(time.Second)

	// Setup: create target, template, group, and agent
	tid, _ := s.CreateTarget(ctx, &store.Target{Name: "b2", Kind: "b2", Bucket: "hody-backups", SealedAdminKey: []byte("sealed"), CreatedAt: now})
	tpl, _ := s.CreateTemplate(ctx, &store.Template{Name: "Home", Sources: []string{"~"}, PolicyJSON: []byte(`{}`), CreatedAt: now})
	gid, _ := s.CreateGroup(ctx, &store.Group{Name: "Laptops", TargetID: tid, TemplateID: tpl, CreatedAt: now})
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

// TestLastOKReportsBatch pins that the batched lookup agrees with the
// per-agent LastOKReport it replaced in the agent-list handler: newest
// successful snapshot per agent, agents without one absent from the map, and
// an ok command report (which backs nothing up) never counted as a backup.
func TestLastOKReportsBatch(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := clock.Now().UTC().Truncate(time.Second)

	tid, _ := s.CreateTarget(ctx, &store.Target{Name: "b2", Kind: "b2", Bucket: "hody-backups", SealedAdminKey: []byte("sealed"), CreatedAt: now})
	tpl, _ := s.CreateTemplate(ctx, &store.Template{Name: "Home", Sources: []string{"~"}, PolicyJSON: []byte(`{}`), CreatedAt: now})
	gid, _ := s.CreateGroup(ctx, &store.Group{Name: "Laptops", TargetID: tid, TemplateID: tpl, CreatedAt: now})
	for _, id := range []string{"ag_1", "ag_2", "ag_3"} {
		require.NoError(t, s.CreateAgent(ctx, &store.Agent{ID: id, Name: id, Hostname: id, OS: "linux", Arch: "amd64", Scope: "user", GroupID: gid, BearerHash: []byte("h_" + id), SealedBundle: []byte("b"), EnrolledAt: now}))
	}

	older, newest := now.Add(-time.Hour), now
	for _, r := range []*store.Report{
		{AgentID: "ag_1", TaskID: "t1", Kind: "snapshot", StartedAt: older, FinishedAt: older, Status: "ok"},
		{AgentID: "ag_1", TaskID: "t2", Kind: "snapshot", StartedAt: newest, FinishedAt: newest, Status: "ok"},
		{AgentID: "ag_1", TaskID: "t3", Kind: "snapshot", StartedAt: newest, FinishedAt: newest.Add(time.Minute), Status: "error"},
		{AgentID: "ag_2", TaskID: "t4", Kind: "command", StartedAt: newest, FinishedAt: newest, Status: "ok"},
		{AgentID: "ag_2", TaskID: "t5", Kind: "snapshot", StartedAt: newest, FinishedAt: newest, Status: "error"},
	} {
		_, err := s.AddReport(ctx, r)
		require.NoError(t, err)
	}

	got, err := s.LastOKReports(ctx)
	require.NoError(t, err)
	require.Equal(t, map[string]time.Time{"ag_1": newest}, got)

	// The batch must agree with the single-agent query it replaced.
	for _, id := range []string{"ag_1", "ag_2", "ag_3"} {
		one, err := s.LastOKReport(ctx, id)
		require.NoError(t, err)
		if one == nil {
			require.NotContains(t, got, id)
			continue
		}
		require.Equal(t, one.FinishedAt, got[id])
	}
}

func TestTokensAndCommandsAndSettings(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := clock.Now().UTC().Truncate(time.Second)

	// Setup: create target, template, group, and agent
	tid, _ := s.CreateTarget(ctx, &store.Target{Name: "b2", Kind: "b2", Bucket: "hody-backups", SealedAdminKey: []byte("sealed"), CreatedAt: now})
	tpl, _ := s.CreateTemplate(ctx, &store.Template{Name: "Home", Sources: []string{"~"}, PolicyJSON: []byte(`{}`), CreatedAt: now})
	gid, _ := s.CreateGroup(ctx, &store.Group{Name: "Laptops", TargetID: tid, TemplateID: tpl, CreatedAt: now})
	s.CreateAgent(ctx, &store.Agent{ID: "ag_1", Name: "hody", Hostname: "fw13", OS: "linux", Arch: "amd64", Scope: "user", GroupID: gid, BearerHash: []byte("h"), SealedBundle: []byte("b"), EnrolledAt: now})

	exp := clock.Now().Add(time.Hour).UTC().Truncate(time.Second)
	id, err := s.CreateToken(ctx, &store.Token{Hash: []byte("th"), GroupID: gid, ExpiresAt: exp, MaxUses: 1, CreatedAt: now})
	require.NoError(t, err)
	tok, err := s.TokenByHash(ctx, []byte("th"))
	require.NoError(t, err)
	require.Equal(t, id, tok.ID)
	require.Equal(t, now, tok.CreatedAt, "created_at round-trips")
	ok, err := s.ConsumeToken(ctx, id, clock.Now())
	require.NoError(t, err)
	require.True(t, ok)
	tok, _ = s.TokenByHash(ctx, []byte("th"))
	require.Equal(t, 1, tok.Uses)
	ok, err = s.ConsumeToken(ctx, id, clock.Now())
	require.NoError(t, err)
	require.False(t, ok, "max_uses=1 token is spent")

	cid, err := s.AddCommand(ctx, &store.Command{AgentID: "ag_1", Kind: "snapshot-now", Source: "/home/hody", CreatedAt: now})
	require.NoError(t, err)
	pend, err := s.PendingCommands(ctx, "ag_1")
	require.NoError(t, err)
	require.Len(t, pend, 1)
	require.ErrorIs(t, s.AckCommand(ctx, cid, "ag_wrong", clock.Now()), store.ErrNotFound, "ack scoped to the wrong agent must not apply")
	require.NoError(t, s.AckCommand(ctx, cid, "ag_1", clock.Now()))
	pend, _ = s.PendingCommands(ctx, "ag_1")
	require.Empty(t, pend)

	v, err := s.Setting(ctx, "poll_interval")
	require.NoError(t, err)
	require.Empty(t, v)
	require.NoError(t, s.SetSetting(ctx, "poll_interval", "300"))
	v, _ = s.Setting(ctx, "poll_interval")
	require.Equal(t, "300", v)
}

// TestTemplatePolicyJSONIsValidatedAtTheStore pins that CreateTemplate and
// UpdateTemplate refuse a policy_json that is not a policy object, and
// normalize an empty one, so no caller can persist a value the list endpoint
// would then stream out as malformed JSON.
func TestTemplatePolicyJSONIsValidatedAtTheStore(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()

	id, err := s.CreateTemplate(ctx, &store.Template{Name: "empty", Sources: []string{"~"}})
	require.NoError(t, err)
	got, err := s.Template(ctx, id)
	require.NoError(t, err)
	require.JSONEq(t, `{}`, string(got.PolicyJSON), "empty policy normalizes to an object")

	for _, bad := range []string{`[1,2]`, `"nope"`, `{`, `null`} {
		_, err := s.CreateTemplate(ctx, &store.Template{Name: "bad", Sources: []string{"~"}, PolicyJSON: []byte(bad)})
		require.ErrorIs(t, err, store.ErrBadPolicyJSON, bad)
		require.ErrorIs(t, s.UpdateTemplate(ctx, &store.Template{ID: id, Name: "bad", Sources: []string{"~"}, PolicyJSON: []byte(bad)}), store.ErrBadPolicyJSON, bad)
	}

	require.NoError(t, s.UpdateTemplate(ctx, &store.Template{ID: id, Name: "ok", Sources: []string{"~"}, PolicyJSON: []byte(`{"retention":{"keepLatest":3}}`)}))
	got, err = s.Template(ctx, id)
	require.NoError(t, err)
	require.JSONEq(t, `{"retention":{"keepLatest":3}}`, string(got.PolicyJSON))
}
