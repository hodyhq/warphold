package engine_test

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/agent/engine"
	"github.com/kopia/kopia/internal/passwordpersist"
	"github.com/kopia/kopia/repo"
)

// TestAgentWaitsForTheFleet pins the 2026-09 field finding: a Fleet restart
// answered the agent's storage-config fetch with 502 Bad Gateway and the
// agent exited. The open must retry a transient failure, serve the local UI
// while it waits, and come up connected once storage answers.
func TestAgentWaitsForTheFleet(t *testing.T) {
	ctx := context.Background()

	t.Setenv("WARPHOLD_STATE_DIR", t.TempDir())
	cfg, pw, _ := provisionedRepo(t)

	var calls atomic.Int32

	release := make(chan struct{})

	defer engine.SetOpenRepoForTest(func(ctx context.Context, c, p string, o *repo.Options) (repo.Repository, error) {
		calls.Add(1)

		select {
		case <-release:
			return repo.Open(ctx, c, p, o)
		default:
			return nil, minio.ErrorResponse{StatusCode: http.StatusBadGateway, Code: "BadGateway"}
		}
	})()

	type result struct {
		h   *engine.Headless
		err error
	}

	done := make(chan result, 1)

	go func() {
		h, err := engine.StartHeadless(ctx, cfg, pw, "user", passwordpersist.None())
		done <- result{h, err}
	}()

	// While storage fails the engine is already serving: engine.json is
	// written and the UI answers.
	var info *engine.Info

	require.Eventually(t, func() bool {
		i, err := engine.ReadInfo("user")
		info = i

		return err == nil && calls.Load() >= 3
	}, 10*time.Second, 10*time.Millisecond)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, info.BaseURL+"/", http.NoBody)
	require.NoError(t, err)
	req.SetBasicAuth(info.User, info.Password)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close() //nolint:errcheck
	require.Equal(t, http.StatusOK, resp.StatusCode)

	select {
	case r := <-done:
		t.Fatalf("StartHeadless returned while storage was still failing: %v", r.err)
	default:
	}

	close(release)

	var r result
	select {
	case r = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("StartHeadless did not return once storage answered")
	}

	require.NoError(t, r.err)

	defer r.h.Stop(ctx) //nolint:errcheck

	api, err := r.h.Client()
	require.NoError(t, err)
	l, err := engine.NewLocal(ctx, api)
	require.NoError(t, err)

	_, connected := l.Status(ctx)
	require.True(t, connected)
}

// TestAgentPermanentOpenErrorExits: a wrong password is not going to get
// better by waiting, so StartHeadless still fails, and cleans up its
// engine.json.
func TestAgentPermanentOpenErrorExits(t *testing.T) {
	ctx := context.Background()

	t.Setenv("WARPHOLD_STATE_DIR", t.TempDir())
	cfg, _, _ := provisionedRepo(t)

	_, err := engine.StartHeadless(ctx, cfg, "wrong-password", "user", passwordpersist.None())
	require.ErrorIs(t, err, repo.ErrInvalidPassword)

	_, err = engine.ReadInfo("user")
	require.Error(t, err)
}
