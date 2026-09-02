package cli_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/agent/state"
	"github.com/kopia/kopia/fleet/api"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/tests/testenv"
)

// fleetForTest activates a Fleet with one filesystem group and returns its URL and a fresh token.
func fleetForTest(t *testing.T) (string, string) {
	t.Helper()
	s := api.New(t.TempDir())
	t.Cleanup(func() { s.Close() })
	m := mux.NewRouter()
	s.Mount(m)
	ts := httptest.NewServer(m)
	t.Cleanup(ts.Close)
	ctx := context.Background()
	require.NoError(t, s.Activate(ctx, "seal-me-please", "hody@hody.dev", "pw12345678"))
	tid, tpl, gid := s.SeedGroupForTesting(ctx, t.TempDir(), []string{"~"}, `{"retention":{"keepLatest":3}}`)
	_ = tid
	_ = tpl
	plain := s.IssueTokenForTesting(ctx, gid)
	return ts.URL, plain
}

func TestAgentEnrollWritesStateAndConnectsRepo(t *testing.T) {
	url, tok := fleetForTest(t)
	stateDir := t.TempDir()
	t.Setenv("WARPHOLD_STATE_DIR", stateDir)
	runner := testenv.NewInProcRunner(t)
	e := testenv.NewCLITest(t, nil, runner)
	e.RunAndExpectSuccess(t, "agent", "enroll", "--server", url, "--token", tok, "--scope", "user")

	cfg, err := state.Load("user")
	require.NoError(t, err)
	require.Equal(t, url, cfg.Server)
	require.NotEmpty(t, cfg.Bearer)
	_, err = repo.Open(context.Background(), filepath.Join(stateDir, "repository.config"), "", nil)
	require.Error(t, err, "password is not persisted in the config file")
	raw, _ := json.Marshal(cfg)
	require.NotContains(t, string(raw), "connect_token")
}
