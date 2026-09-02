package engine_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
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
	t.Setenv("WARPHOLD_STATE_DIR", t.TempDir())
	cfg, pw := provisionedRepo(t)
	h, err := engine.StartHeadless(ctx, cfg, pw, "user")
	require.NoError(t, err)
	defer h.Stop(ctx)
	api, err := h.Client()
	require.NoError(t, err)
	l, err := engine.NewLocal(context.Background(), api)
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

	// the manifest written by that task is findable from the task's start time
	id, err := l.LatestSnapshotID(ctx, src, done[0].StartTime, *done[0].EndTime)
	require.NoError(t, err)
	require.NotEmpty(t, id)
	// ...but not from a window that starts after it finished
	later := done[0].EndTime.Add(time.Hour)
	id, err = l.LatestSnapshotID(ctx, src, later, later.Add(time.Minute))
	require.NoError(t, err)
	require.Empty(t, id, "no matching manifest yields an empty id, never a wrong one")

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
	t.Setenv("WARPHOLD_STATE_DIR", t.TempDir())
	cfg, pw := provisionedRepo(t)
	h, err := engine.StartHeadless(ctx, cfg, pw, "user")
	require.NoError(t, err)
	defer h.Stop(ctx)
	api, err := h.Client()
	require.NoError(t, err)
	l, err := engine.NewLocal(context.Background(), api)
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

// TestHeadlessServesUI pins that agent mode serves the WarpHold SPA itself:
// the tray's handoff URL has to land on a real page, not a 404 from the API
// router.
func TestHeadlessServesUI(t *testing.T) {
	ctx := context.Background()
	t.Setenv("WARPHOLD_STATE_DIR", t.TempDir())
	cfg, pw := provisionedRepo(t)
	h, err := engine.StartHeadless(ctx, cfg, pw, "user")
	require.NoError(t, err)

	defer h.Stop(ctx) //nolint:errcheck

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.BaseURL+"/", nil)
	require.NoError(t, err)
	req.SetBasicAuth(h.User, h.Password)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	defer resp.Body.Close() //nolint:errcheck

	require.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	require.Contains(t, string(body), "<title>WarpHold")

	// client-side routes survive a direct navigation or a refresh: upstream's
	// isKnownUIRoute allowlist does not know them, so they must be served the
	// index rather than the file server's 404.
	for _, p := range []string{"/agent", "/fleet/devices"} {
		deepReq, err := http.NewRequestWithContext(ctx, http.MethodGet, h.BaseURL+p, nil)
		require.NoError(t, err)
		deepReq.SetBasicAuth(h.User, h.Password)

		deep, err := http.DefaultClient.Do(deepReq)
		require.NoError(t, err)

		deepBody, err := io.ReadAll(deep.Body)
		deep.Body.Close() //nolint:errcheck,gosec
		require.NoError(t, err)

		require.Equal(t, http.StatusOK, deep.StatusCode, p)
		require.Contains(t, string(deepBody), "<title>WarpHold", p)
	}

	// ... and an unknown path is still a 404.
	nopeReq, err := http.NewRequestWithContext(ctx, http.MethodGet, h.BaseURL+"/nope", nil)
	require.NoError(t, err)
	nopeReq.SetBasicAuth(h.User, h.Password)

	nope, err := http.DefaultClient.Do(nopeReq)
	require.NoError(t, err)

	defer nope.Body.Close() //nolint:errcheck

	require.Equal(t, http.StatusNotFound, nope.StatusCode)

	// the SPA's mode detection: no Fleet routes in agent mode.
	fleetReq, err := http.NewRequestWithContext(ctx, http.MethodGet, h.BaseURL+"/api/v1/fleet/status", nil)
	require.NoError(t, err)
	fleetReq.SetBasicAuth(h.User, h.Password)

	fleet, err := http.DefaultClient.Do(fleetReq)
	require.NoError(t, err)

	defer fleet.Body.Close() //nolint:errcheck

	require.Equal(t, http.StatusNotFound, fleet.StatusCode)

	// the bundle itself is public - it is static code, and the tray hands the
	// browser a URL that has to render before the session cookie exists.
	for _, p := range []string{"/", "/agent"} {
		anon, err := http.Get(h.BaseURL + p) //nolint:noctx
		require.NoError(t, err)

		anonBody, err := io.ReadAll(anon.Body)
		anon.Body.Close() //nolint:errcheck,gosec
		require.NoError(t, err)

		require.Equal(t, http.StatusOK, anon.StatusCode, p)
		require.Contains(t, string(anonBody), "<title>WarpHold", p)
	}

	// ... while the API behind it still is not.
	anonAPI, err := http.Get(h.BaseURL + "/api/v1/sources") //nolint:noctx
	require.NoError(t, err)

	defer anonAPI.Body.Close() //nolint:errcheck

	require.Equal(t, http.StatusUnauthorized, anonAPI.StatusCode)
}
