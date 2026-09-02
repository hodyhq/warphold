package engine_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/agent/engine"
	"github.com/kopia/kopia/agent/poll"
	"github.com/kopia/kopia/fleet/enroll"
	"github.com/kopia/kopia/internal/uitask"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/content"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/policy"
)

// provisionedRepo makes a filesystem repo the way Fleet does and connects a config to it.
func provisionedRepo(t *testing.T) (configFile, password string) {
	t.Helper()
	ctx := context.Background()
	p := &enroll.Provisioner{Owner: "fleet@test"}
	b, err := p.Provision(ctx, enroll.TargetSpec{Kind: "filesystem", Path: t.TempDir()}, "ag_e")
	require.NoError(t, err)
	ci, pw, err := repo.DecodeToken(b.ConnectToken)
	require.NoError(t, err)
	st, err := blob.NewStorage(ctx, ci, false)
	require.NoError(t, err)
	dir := t.TempDir()
	cfg := filepath.Join(dir, "repository.config")
	require.NoError(t, repo.Connect(ctx, cfg, st, pw, &repo.ConnectOptions{CachingOptions: content.CachingOptions{CacheDirectory: filepath.Join(dir, "cache")}}))
	return cfg, pw
}

func TestApplySnapshotAndReport(t *testing.T) {
	ctx := context.Background()
	cfg, pw := provisionedRepo(t)
	h, err := engine.StartHeadless(ctx, cfg, pw, t.TempDir())
	require.NoError(t, err)
	defer h.Stop(ctx)
	api, err := h.Client()
	require.NoError(t, err)
	l, err := engine.NewLocal(api)
	require.NoError(t, err)
	require.NotEmpty(t, l.Host)

	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello"), 0o600))
	pol, _ := json.Marshal(map[string]any{"retention": map[string]any{"keepLatest": 2}, "scheduling": map[string]any{"manual": true}})
	require.NoError(t, l.Apply(ctx, []poll.Source{{Path: src, Policy: pol}}))

	// the policy landed in the repository
	r, err := repo.Open(ctx, cfg, pw, nil)
	require.NoError(t, err)
	got, err := policy.GetDefinedPolicy(ctx, r, snapshot.SourceInfo{Host: l.Host, UserName: l.User, Path: src})
	require.NoError(t, err)
	require.EqualValues(t, 2, *got.RetentionPolicy.KeepLatest)
	r.Close(ctx)

	require.NoError(t, l.Snapshot(ctx, src))
	var done []uitask.Info
	require.Eventually(t, func() bool {
		tasks, err := l.Tasks(ctx)
		if err != nil {
			return false
		}
		done = nil
		for _, tk := range tasks {
			if tk.EndTime != nil && tk.Kind == "Snapshot" {
				done = append(done, tk)
			}
		}
		return len(done) == 1
	}, 60*time.Second, 200*time.Millisecond)
	rep := engine.ToReport(done[0], src)
	require.Equal(t, "ok", rep.Status)
	require.Equal(t, "snapshot", rep.Kind)
	require.Equal(t, src, rep.Source)
	require.Greater(t, rep.Files, int64(0))

	// removing the source deletes its policy
	require.NoError(t, l.Apply(ctx, nil))
	r, _ = repo.Open(ctx, cfg, pw, nil)
	_, err = policy.GetDefinedPolicy(ctx, r, snapshot.SourceInfo{Host: l.Host, UserName: l.User, Path: src})
	require.ErrorIs(t, err, policy.ErrPolicyNotFound)
	r.Close(ctx)

	require.Equal(t, filepath.Join(os.Getenv("HOME"), "x"), engine.ExpandHome("~/x"))
}

func TestStatus(t *testing.T) {
	ctx := context.Background()
	cfg, pw := provisionedRepo(t)
	h, err := engine.StartHeadless(ctx, cfg, pw, t.TempDir())
	require.NoError(t, err)
	defer h.Stop(ctx)
	api, err := h.Client()
	require.NoError(t, err)
	l, err := engine.NewLocal(api)
	require.NoError(t, err)

	// no sources yet: connected, idle (not vacuously "paused").
	status, connected := l.Status(ctx)
	require.True(t, connected)
	require.Equal(t, "idle", status)

	src := t.TempDir()
	pol, _ := json.Marshal(map[string]any{"scheduling": map[string]any{"manual": true}})
	require.NoError(t, l.Apply(ctx, []poll.Source{{Path: src, Policy: pol}}))
	require.NoError(t, l.Pause(ctx, src))
	// the source only flips its reported status to PAUSED the next time it
	// evaluates a snapshot request, so nudge it once.
	require.NoError(t, l.Snapshot(ctx, src))
	require.Eventually(t, func() bool {
		status, connected = l.Status(ctx)
		return connected && status == "paused"
	}, 10*time.Second, 100*time.Millisecond)
}
