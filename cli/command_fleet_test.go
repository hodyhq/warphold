package cli_test

import (
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet"
	"github.com/kopia/kopia/internal/testutil"
	"github.com/kopia/kopia/tests/testenv"
)

// fleetSeamRoundTrip drives the load-bearing upstream seam end to end:
// `fleet activate` writes state next to the repository config file, and
// `server start` mounts Fleet's routes through RegisterServerHandlers /
// commandServerStart.setupHandlers on the same mux the UI would use.
func fleetSeamRoundTrip(t *testing.T) {
	t.Helper()

	runner := testenv.NewInProcRunner(t)
	e := testenv.NewCLITest(t, nil, runner)

	// NewCLITest passes "--config-file <ConfigDir>/.kopia.config" as a fixed
	// arg, and StateDirFor puts Fleet state in "fleet" next to it.
	configFile := filepath.Join(e.ConfigDir, ".kopia.config")
	stateDir := fleet.StateDirFor(configFile)

	e.RunAndExpectSuccess(t, "fleet", "activate",
		"--email", "hody@hody.dev",
		"--admin-password", "pw12345678",
		"--passphrase", "seal-me-please")

	require.FileExists(t, filepath.Join(stateDir, "seal.key"))
	require.FileExists(t, filepath.Join(stateDir, "fleet.db"))

	var sp testutil.ServerParameters

	wait, kill := e.RunAndProcessStderr(t, sp.ProcessOutput,
		"server", "start",
		"--insecure",
		"--without-password",
		"--no-ui",
		"--no-grpc",
		"--address=127.0.0.1:0",
		"--server-control-password=admin-pwd",
	)

	defer func() {
		kill()
		wait() //nolint:errcheck
	}()

	require.NotEmpty(t, sp.BaseURL, "server did not report its address")

	res, err := http.Get(sp.BaseURL + "/api/v1/fleet/status") //nolint:noctx
	require.NoError(t, err)

	defer res.Body.Close()

	require.Equal(t, http.StatusOK, res.StatusCode)

	var body map[string]any
	require.NoError(t, json.NewDecoder(res.Body).Decode(&body))
	require.Equal(t, true, body["activated"], "fleet routes must be mounted and see the activated state dir")
}

// TestFleetSeamSurvivesRepeatedAppSetup runs the whole round trip twice in one
// test binary. cli.NewApp() re-runs App.setup (and so commandFleet.setup) per
// CLI invocation, so the sync.Once guarding RegisterServerHandlers must keep
// exactly one mount closure registered; a second registration would mount
// Fleet's routes twice and gorilla/mux would serve the first (stale) one.
func TestFleetSeamSurvivesRepeatedAppSetup(t *testing.T) {
	t.Run("first", fleetSeamRoundTrip)
	t.Run("second", fleetSeamRoundTrip)
}

// TestServerServesSPADeepLinks pins the other half of the seam: `server start`
// serves the UI index for WarpHold's own client-side routes, so a bookmark or
// a refresh on /fleet/devices does not hit upstream's file-server 404.
// Upstream's isKnownUIRoute allowlist knows nothing about them, so the routes
// registered by the Fleet hook (which runs before the UI catch-all) are what
// makes this work.
func TestServerServesSPADeepLinks(t *testing.T) {
	runner := testenv.NewInProcRunner(t)
	e := testenv.NewCLITest(t, nil, runner)

	var sp testutil.ServerParameters

	wait, kill := e.RunAndProcessStderr(t, sp.ProcessOutput,
		"server", "start",
		"--insecure",
		"--without-password",
		"--no-grpc",
		"--address=127.0.0.1:0",
		"--server-control-password=admin-pwd",
	)

	defer func() {
		kill()
		wait() //nolint:errcheck
	}()

	require.NotEmpty(t, sp.BaseURL, "server did not report its address")

	for _, tc := range []struct {
		path string
		want int
	}{
		{"/fleet", http.StatusOK},
		{"/fleet/login", http.StatusOK},
		{"/fleet/devices", http.StatusOK},
		{"/agent", http.StatusOK},
		{"/snapshots", http.StatusOK}, // upstream route, still works
		{"/nope", http.StatusNotFound},
		{"/fleeting", http.StatusNotFound}, // prefix match must not be greedy
	} {
		res, err := http.Get(sp.BaseURL + tc.path) //nolint:noctx
		require.NoError(t, err)

		body, err := io.ReadAll(res.Body)
		res.Body.Close() //nolint:errcheck,gosec
		require.NoError(t, err)

		require.Equal(t, tc.want, res.StatusCode, tc.path)

		if tc.want == http.StatusOK {
			require.Contains(t, string(body), "<title>WarpHold", tc.path)
		}
	}
}
