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

// TestAgentEnrollTakesTokenFromEnvironment pins that the enrollment token can
// be supplied without ever appearing in argv, where "ps" and shell history
// would expose it. It also pins that --token stays Required(): kingpin's
// needsValue() treats an envar value as provided, so omitting the flag is only
// an error when the variable is unset too.
func TestAgentEnrollTakesTokenFromEnvironment(t *testing.T) {
	url, tok := fleetForTest(t)
	t.Setenv("WARPHOLD_STATE_DIR", t.TempDir())
	runner := testenv.NewInProcRunner(t)
	e := testenv.NewCLITest(t, nil, runner)

	// The runner prefixes the name, matching App.EnvName in tests.
	e.Environment["WARPHOLD_ENROLL_TOKEN"] = tok
	e.RunAndExpectSuccess(t, "agent", "enroll", "--server", url, "--scope", "user")

	cfg, err := state.Load("user")
	require.NoError(t, err)
	require.Equal(t, url, cfg.Server)
	require.NotEmpty(t, cfg.Bearer)
}

func TestAgentEnrollRequiresATokenFromSomewhere(t *testing.T) {
	url, _ := fleetForTest(t)
	t.Setenv("WARPHOLD_STATE_DIR", t.TempDir())
	runner := testenv.NewInProcRunner(t)
	e := testenv.NewCLITest(t, nil, runner)

	// e.Environment carries no token, and the runner gives each CLI run its
	// own generated name prefix, so nothing in the ambient environment can
	// satisfy the flag here.
	require.NotContains(t, e.Environment, "WARPHOLD_ENROLL_TOKEN")

	// e.Run, not RunAndExpectFailure: kingpin reports a missing required flag
	// through the returned error, which RunAndExpectFailure discards.
	_, _, err := e.Run(t, true, "agent", "enroll", "--server", url, "--scope", "user")
	require.ErrorContains(t, err, "--token", "the failure must be the missing token, not something else")
}
