package cli_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet"
	"github.com/kopia/kopia/fleet/seal"
	"github.com/kopia/kopia/fleet/store"
	"github.com/kopia/kopia/internal/apiclient"
	"github.com/kopia/kopia/internal/testutil"
	"github.com/kopia/kopia/repo"
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

	// The device-facing S3 gateway is mounted on the same mux, at the bucket
	// path, and it is mounted whether or not the UI is. Unauthenticated is 403
	// with S3's error document, not the UI's 404.
	gwRes, err := http.Get(sp.BaseURL + "/warphold/some-device/some-blob") //nolint:noctx
	require.NoError(t, err)

	gwBody, err := io.ReadAll(gwRes.Body)
	gwRes.Body.Close()
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, gwRes.StatusCode, "the gateway must be mounted at /warphold/")
	require.Contains(t, string(gwBody), "<Code>AccessDenied</Code>")

	// --no-ui must also disable the public SPA bundle: the deep-link handler
	// probes for upstream's static catch-all and serves nothing without it.
	for _, p := range []string{"/", "/fleet", "/fleet/login", "/agent", "/assets/"} {
		r, err := http.Get(sp.BaseURL + p) //nolint:noctx
		require.NoError(t, err)
		r.Body.Close()
		require.Equal(t, http.StatusNotFound, r.StatusCode, "%s must be 404 under --no-ui", p)
	}
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

// TestServerServesSPAWithoutUIAuth pins the fix for the Fleet dashboard's
// chicken-and-egg: with `--server-username/--server-password` upstream's
// ServeStaticFiles answers 401 to every UI path, so the dashboard could never
// load the login page it needs in order to authenticate. The bundle is public
// static code and is served without credentials; the APIs behind it are not.
func TestServerServesSPAWithoutUIAuth(t *testing.T) {
	runner := testenv.NewInProcRunner(t)
	e := testenv.NewCLITest(t, testenv.RepoFormatNotImportant, runner)

	e.RunAndExpectSuccess(t, "repo", "create", "filesystem", "--path", e.RepoDir)

	t.Cleanup(func() {
		e.RunAndExpectSuccess(t, "repo", "disconnect")
	})

	var sp testutil.ServerParameters

	wait, kill := e.RunAndProcessStderr(t, sp.ProcessOutput,
		"server", "start",
		"--insecure",
		"--no-grpc",
		"--address=127.0.0.1:0",
		"--server-username", "admin",
		"--server-password", "admin-pwd",
		"--server-control-password", "control-pwd",
	)

	defer func() {
		kill()
		wait() //nolint:errcheck
	}()

	require.NotEmpty(t, sp.BaseURL, "server did not report its address")

	jar, err := cookiejar.New(nil)
	require.NoError(t, err)

	// the jar carries the session cookie the public index hands out, which is
	// what the CSRF token is bound to.
	client := &http.Client{Jar: jar}

	var csrfToken string

	get := func(t *testing.T, path string, withCredentials bool) (*http.Response, string) {
		t.Helper()

		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, sp.BaseURL+path, nil)
		require.NoError(t, err)

		if withCredentials {
			req.SetBasicAuth("admin", "admin-pwd")
			req.Header.Set(apiclient.CSRFTokenHeader, csrfToken)
		}

		res, err := client.Do(req)
		require.NoError(t, err)

		body, err := io.ReadAll(res.Body)
		res.Body.Close() //nolint:errcheck,gosec
		require.NoError(t, err)

		return res, string(body)
	}

	// the index, and every client-side route that has to render before the
	// user has any credentials to send.
	for _, p := range []string{"/", "/fleet", "/fleet/login", "/fleet/devices", "/agent"} {
		res, body := get(t, p, false)

		require.Equal(t, http.StatusOK, res.StatusCode, p)
		require.Contains(t, body, "<title>WarpHold", p)
		require.Equal(t, "no-cache", res.Header.Get("Cache-Control"), p)
	}

	// the hashed bundle the index actually references.
	_, index := get(t, "/", false)

	asset := regexp.MustCompile(`/assets/[A-Za-z0-9._-]+\.css`).FindString(index)
	require.NotEmpty(t, asset, "index.html should reference a hashed stylesheet")

	res, css := get(t, asset, false)
	require.Equal(t, http.StatusOK, res.StatusCode, asset)
	require.NotEmpty(t, css)
	require.Contains(t, res.Header.Get("Cache-Control"), "immutable", asset)

	// no directory listing of the bundle.
	res, _ = get(t, "/assets/", false)
	require.Equal(t, http.StatusNotFound, res.StatusCode)

	// Fleet's own routes carry their own authentication.
	res, _ = get(t, "/api/v1/fleet/status", false)
	require.Equal(t, http.StatusOK, res.StatusCode)

	// ... while Kopia's stay behind the UI credentials.
	res, _ = get(t, "/api/v1/sources", false)
	require.Equal(t, http.StatusUnauthorized, res.StatusCode)

	// the session the public index handed out is a working one: with it, the
	// credentials the user can now type into the page are accepted.
	csrfMatch := regexp.MustCompile(`kopia-csrf-token" content="([a-f0-9]+)"`).FindStringSubmatch(index)
	require.Len(t, csrfMatch, 2, "public index must carry a CSRF token")
	csrfToken = csrfMatch[1]

	res, _ = get(t, "/api/v1/sources", true)
	require.Equal(t, http.StatusOK, res.StatusCode)

	// paths we do not serve keep upstream's behavior: the auth check runs
	// before its file server, so an unknown path is a 401, not a 404.
	for _, p := range []string{"/nope", "/fleeting", "/api/v1/nope"} {
		res, _ := get(t, p, false)
		require.Equal(t, http.StatusUnauthorized, res.StatusCode, p)
	}
}

// ---------------------------------------------------------------------------
// Task 20: non-interactive setup - `fleet activate --public-url` and the Fleet
// host's own repository.

func fleetConfigFile(e *testenv.CLITest) string {
	return filepath.Join(e.ConfigDir, ".kopia.config")
}

func fleetSetting(t *testing.T, e *testenv.CLITest, key string) string {
	t.Helper()

	st, err := store.Open(filepath.Join(fleet.StateDirFor(fleetConfigFile(e)), "fleet.db"))
	require.NoError(t, err)

	defer st.Close() //nolint:errcheck

	v, err := st.Setting(t.Context(), key)
	require.NoError(t, err)

	return v
}

// fleetRepoPassword unseals the Fleet host's own repository password exactly
// the way the server does: read the sealed setting, open it with the key file
// activation wrote.
func fleetRepoPassword(t *testing.T, e *testenv.CLITest) string {
	t.Helper()

	key, err := seal.ReadKeyFile(filepath.Join(fleet.StateDirFor(fleetConfigFile(e)), "seal.key"))
	require.NoError(t, err)

	sealed, err := hex.DecodeString(fleetSetting(t, e, "fleet_repo_password"))
	require.NoError(t, err)

	pw, err := key.Open(sealed)
	require.NoError(t, err)

	return string(pw)
}

// TestFleetActivateReadsTheInstallerEnvironment pins the contract with
// scripts/install/fleet.sh: it exports WARPHOLD_SETUP_EMAIL, _PASSWORD,
// _PASSPHRASE and _PUBLIC_URL, and none of those secrets may have to appear in
// argv, where "ps" shows them to every user on the host.
func TestFleetActivateReadsTheInstallerEnvironment(t *testing.T) {
	runner := testenv.NewInProcRunner(t)
	e := testenv.NewCLITest(t, nil, runner)

	e.Environment["WARPHOLD_SETUP_EMAIL"] = "env@hody.dev"
	e.Environment["WARPHOLD_SETUP_PASSWORD"] = "pw12345678"
	e.Environment["WARPHOLD_SETUP_PASSPHRASE"] = "seal-me-please"
	e.Environment["WARPHOLD_SETUP_PUBLIC_URL"] = "https://env.example.com"

	e.RunAndExpectSuccess(t, "fleet", "activate")

	require.Equal(t, "https://env.example.com", fleetSetting(t, e, "public_url"))
	require.NotEmpty(t, fleetSetting(t, e, "instance_id"), "activation mints the instance id, so --verify-public-url has one to compare")
}

// TestFleetActivateBackwardCompatibleSecretEnvironment pins that the names the
// installer used before the WARPHOLD_SETUP_* set existed still work.
func TestFleetActivateBackwardCompatibleSecretEnvironment(t *testing.T) {
	runner := testenv.NewInProcRunner(t)
	e := testenv.NewCLITest(t, nil, runner)

	e.Environment["WARPHOLD_ADMIN_PASSWORD"] = "pw12345678"
	e.Environment["WARPHOLD_SEAL_PASSPHRASE"] = "seal-me-please"

	e.RunAndExpectSuccess(t, "fleet", "activate", "--email", "old@hody.dev")

	require.FileExists(t, filepath.Join(fleet.StateDirFor(fleetConfigFile(e)), "seal.key"))
}

// TestFleetActivateFlagsBeatTheEnvironment: an operator who overrides the
// installer's environment on the command line must get the override.
func TestFleetActivateFlagsBeatTheEnvironment(t *testing.T) {
	runner := testenv.NewInProcRunner(t)
	e := testenv.NewCLITest(t, nil, runner)

	e.Environment["WARPHOLD_SETUP_EMAIL"] = "env@hody.dev"
	e.Environment["WARPHOLD_SETUP_PASSWORD"] = "pw12345678"
	e.Environment["WARPHOLD_SETUP_PASSPHRASE"] = "seal-me-please"
	e.Environment["WARPHOLD_SETUP_PUBLIC_URL"] = "https://env.example.com"

	e.RunAndExpectSuccess(t, "fleet", "activate",
		"--email", "flag@hody.dev",
		"--public-url", "https://flag.example.com")

	require.Equal(t, "https://flag.example.com", fleetSetting(t, e, "public_url"))

	st, err := store.Open(filepath.Join(fleet.StateDirFor(fleetConfigFile(e)), "fleet.db"))
	require.NoError(t, err)

	defer st.Close() //nolint:errcheck

	_, err = st.AdminByEmail(t.Context(), "flag@hody.dev")
	require.NoError(t, err, "the admin is the one named on the command line")
}

// TestFleetActivateRejectsBadPublicURLBeforeWriting: activation happens once,
// so a URL the server would refuse must fail before any state exists - not
// after, when the operator can no longer redo the step.
func TestFleetActivateRejectsBadPublicURLBeforeWriting(t *testing.T) {
	for _, bad := range []string{
		"fleet.example.com",              // not absolute
		"http://fleet.example.com",       // plaintext, and not loopback
		"https://fleet.example.com/path", // a path would vanish from every URL built by concatenation
		"https://user:pw@fleet.example.com",
	} {
		t.Run(bad, func(t *testing.T) {
			runner := testenv.NewInProcRunner(t)
			e := testenv.NewCLITest(t, nil, runner)

			_, stderr := e.RunAndExpectFailure(t, "fleet", "activate",
				"--email", "hody@hody.dev",
				"--admin-password", "pw12345678",
				"--passphrase", "seal-me-please",
				"--public-url", bad)

			require.Contains(t, strings.Join(stderr, "\n"), "public_url")

			stateDir := fleet.StateDirFor(fleetConfigFile(e))
			require.NoFileExists(t, filepath.Join(stateDir, "seal.key"))
			require.NoFileExists(t, filepath.Join(stateDir, "fleet.db"))
		})
	}
}

// TestFleetActivateVerifiesPublicURLEndToEnd drives the whole installer shape:
// the service is already running when `fleet activate` is invoked, the CLI
// activates and then fetches its own public URL through the network to prove
// the URL reaches this Fleet. It also pins that the running server picks up an
// activation performed by another process - without that it would answer "not
// activated" until someone restarted it, and the probe could never pass.
func TestFleetActivateVerifiesPublicURLEndToEnd(t *testing.T) {
	runner := testenv.NewInProcRunner(t)
	e := testenv.NewCLITest(t, nil, runner)

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

	stdout := e.RunAndExpectSuccess(t, "fleet", "activate",
		"--email", "hody@hody.dev",
		"--admin-password", "pw12345678",
		"--passphrase", "seal-me-please",
		"--public-url", sp.BaseURL,
		"--verify-public-url")

	require.Contains(t, strings.Join(stdout, "\n"), "answers as this Fleet")
	require.Equal(t, sp.BaseURL, fleetSetting(t, e, "public_url"))

	res, err := http.Get(sp.BaseURL + "/api/v1/fleet/status") //nolint:noctx
	require.NoError(t, err)

	defer res.Body.Close()

	var body map[string]any
	require.NoError(t, json.NewDecoder(res.Body).Decode(&body))
	require.Equal(t, true, body["activated"], "the running server must see the CLI's activation without a restart")
	require.Equal(t, fleetSetting(t, e, "instance_id"), body["instance_id"])
}

// TestFleetActivateFailsWhenPublicURLDoesNotAnswer: a URL that answers as
// something other than this Fleet fails the command and prints the proxy
// checklist. The Fleet itself is activated by then - the probe needs an
// activated Fleet to answer - so the message says so rather than pretending
// nothing happened.
func TestFleetActivateFailsWhenPublicURLDoesNotAnswer(t *testing.T) {
	notFleet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"activated":false}`)) //nolint:errcheck
	}))
	defer notFleet.Close()

	runner := testenv.NewInProcRunner(t)
	e := testenv.NewCLITest(t, nil, runner)

	_, stderr := e.RunAndExpectFailure(t, "fleet", "activate",
		"--email", "hody@hody.dev",
		"--admin-password", "pw12345678",
		"--passphrase", "seal-me-please",
		"--public-url", notFleet.URL,
		"--verify-public-url")

	joined := strings.Join(stderr, "\n")
	require.Contains(t, joined, "did not answer as an activated WarpHold Fleet")
	require.Contains(t, joined, "forward the Host header unchanged", "the proxy checklist comes with the failure")
	require.FileExists(t, filepath.Join(fleet.StateDirFor(fleetConfigFile(e)), "seal.key"), "the fleet is activated; only the probe failed")
}

// TestFleetActivateCreatesTheHostRepository pins the other half of setup: the
// Fleet host is a machine that needs backing up too, so activation leaves it
// with a repository of its own, connected, openable with the sealed password
// and created exactly once.
func TestFleetActivateCreatesTheHostRepository(t *testing.T) {
	runner := testenv.NewInProcRunner(t)
	e := testenv.NewCLITest(t, nil, runner)

	e.RunAndExpectSuccess(t, "fleet", "activate",
		"--email", "hody@hody.dev",
		"--admin-password", "pw12345678",
		"--passphrase", "seal-me-please")

	repoDir := filepath.Join(e.ConfigDir, "data", "fleet-repo")
	require.Equal(t, repoDir, fleetSetting(t, e, "fleet_repo_path"))

	formatBlob, err := os.ReadFile(filepath.Join(repoDir, "kopia.repository.f"))
	require.NoError(t, err)
	require.NotEmpty(t, formatBlob)

	// The password in the Fleet DB is the repository's real password, and the
	// Fleet host's repository has a config file of its own - the operator's
	// --config-file is never repointed at it.
	require.NoFileExists(t, fleetConfigFile(e), "activation does not connect this installation's own config file")

	rep, err := repo.Open(t.Context(), filepath.Join(fleet.StateDirFor(fleetConfigFile(e)), "fleet-repo.config"), fleetRepoPassword(t, e), nil)
	require.NoError(t, err, "the sealed password opens the Fleet host's repository")
	require.NoError(t, rep.Close(t.Context()))

	// Re-running activate is refused, and does not build a second repository.
	_, stderr := e.RunAndExpectFailure(t, "fleet", "activate",
		"--email", "someone@hody.dev",
		"--admin-password", "pw12345678",
		"--passphrase", "seal-me-please")
	require.Contains(t, strings.Join(stderr, "\n"), "already activated")

	again, err := os.ReadFile(filepath.Join(repoDir, "kopia.repository.f"))
	require.NoError(t, err)
	require.Equal(t, formatBlob, again, "the repository was not re-created")
}

// TestFleetActivateAdminPasswordNameWins pins the precedence of the two names
// for the same secret, and - in the same run - that a Fleet activated by
// another process is usable over HTTP immediately: the login below goes
// through requireHost, which is where the running server notices the
// activation, and it must succeed with the password the winning variable set.
func TestFleetActivateAdminPasswordNameWins(t *testing.T) {
	runner := testenv.NewInProcRunner(t)
	e := testenv.NewCLITest(t, nil, runner)

	e.Environment["WARPHOLD_ADMIN_PASSWORD"] = "admin-name-pw"
	e.Environment["WARPHOLD_SETUP_PASSWORD"] = "setup-name-pw"
	e.Environment["WARPHOLD_SETUP_PASSPHRASE"] = "seal-me-please"

	var sp testutil.ServerParameters

	wait, kill := e.RunAndProcessStderr(t, sp.ProcessOutput,
		"server", "start",
		"--insecure", "--without-password", "--no-ui", "--no-grpc",
		"--address=127.0.0.1:0", "--server-control-password=admin-pwd",
	)

	defer func() {
		kill()
		wait() //nolint:errcheck
	}()

	require.NotEmpty(t, sp.BaseURL, "server did not report its address")

	e.RunAndExpectSuccess(t, "fleet", "activate", "--email", "hody@hody.dev")

	login := func(password string) int {
		body, err := json.Marshal(map[string]string{"email": "hody@hody.dev", "password": password})
		require.NoError(t, err)

		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, sp.BaseURL+"/api/v1/fleet/session", bytes.NewReader(body))
		require.NoError(t, err)

		req.Header.Set("Content-Type", "application/json")

		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		res.Body.Close() //nolint:errcheck,gosec

		return res.StatusCode
	}

	require.Equal(t, http.StatusNoContent, login("admin-name-pw"), "WARPHOLD_ADMIN_PASSWORD wins, and the running server sees the activation")
	require.Equal(t, http.StatusUnauthorized, login("setup-name-pw"))
}

// TestFleetActivateDataDirIsAbsoluteAndNotASymlink: --data-dir names a
// directory a privileged service writes repository data into. A relative path
// is stored resolved (so a later `server start` in another working directory
// finds the same place) and a symlinked directory is refused before anything
// is activated.
func TestFleetActivateDataDirIsAbsoluteAndNotASymlink(t *testing.T) {
	t.Run("relative is stored absolute", func(t *testing.T) {
		runner := testenv.NewInProcRunner(t)
		e := testenv.NewCLITest(t, nil, runner)

		wd, err := os.Getwd()
		require.NoError(t, err)

		rel, err := filepath.Rel(wd, filepath.Join(t.TempDir(), "warphold-data"))
		require.NoError(t, err)

		e.RunAndExpectSuccess(t, "fleet", "activate",
			"--email", "hody@hody.dev",
			"--admin-password", "pw12345678",
			"--passphrase", "seal-me-please",
			"--data-dir", rel)

		got := fleetSetting(t, e, "fleet_repo_path")
		require.True(t, filepath.IsAbs(got), "recorded path must be absolute, got %v", got)
		require.Equal(t, "fleet-repo", filepath.Base(got))
	})

	t.Run("a symlinked data directory is refused", func(t *testing.T) {
		runner := testenv.NewInProcRunner(t)
		e := testenv.NewCLITest(t, nil, runner)

		base := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(base, "real"), 0o700))
		require.NoError(t, os.Symlink(filepath.Join(base, "real"), filepath.Join(base, "link")))

		_, stderr := e.RunAndExpectFailure(t, "fleet", "activate",
			"--email", "hody@hody.dev",
			"--admin-password", "pw12345678",
			"--passphrase", "seal-me-please",
			"--data-dir", filepath.Join(base, "link"))

		require.Contains(t, strings.Join(stderr, "\n"), "symlink")
		require.NoFileExists(t, filepath.Join(fleet.StateDirFor(fleetConfigFile(e)), "seal.key"), "refused before activation")
	})
}

// TestFleetActivateCreatesDefaultsAndPrintsTheOneLiner: the point of the
// non-interactive path is that the machine is ready to enroll a device when
// the command returns, so setup creates the first target, template and group
// and prints the command that joins a device to it.
func TestFleetActivateCreatesDefaultsAndPrintsTheOneLiner(t *testing.T) {
	runner := testenv.NewInProcRunner(t)
	e := testenv.NewCLITest(t, nil, runner)

	stdout := e.RunAndExpectSuccess(t, "fleet", "activate",
		"--email", "hody@hody.dev",
		"--admin-password", "pw12345678",
		"--passphrase", "seal-me-please",
		"--public-url", "https://fleet.example.com")

	joined := strings.Join(stdout, "\n")
	hostedRoot := filepath.Join(e.ConfigDir, "data", "hosted")
	require.Contains(t, joined, hostedRoot)
	require.Contains(t, joined, "Enrollment token (paste when prompted): wh_")
	require.Contains(t, joined, "https://fleet.example.com/enroll.sh")
	require.DirExists(t, hostedRoot)

	st, err := store.Open(filepath.Join(fleet.StateDirFor(fleetConfigFile(e)), "fleet.db"))
	require.NoError(t, err)

	defer st.Close() //nolint:errcheck

	targets, err := st.Targets(t.Context())
	require.NoError(t, err)
	require.Len(t, targets, 1)
	require.Equal(t, "Fleet disk", targets[0].Name)
	require.Equal(t, "hosted", targets[0].Kind)
	require.Equal(t, "disk", targets[0].StorageMode)
	require.Equal(t, hostedRoot, targets[0].Path)

	groups, err := st.Groups(t.Context())
	require.NoError(t, err)
	require.Len(t, groups, 1)
	require.Equal(t, "Devices", groups[0].Name)
	require.Equal(t, targets[0].ID, groups[0].TargetID)
}

// --storage cloud cannot be finished without credentials, so it fails before
// anything is written rather than half-way through setup.
func TestFleetActivateRefusesCloudStorage(t *testing.T) {
	runner := testenv.NewInProcRunner(t)
	e := testenv.NewCLITest(t, nil, runner)

	_, stderr := e.RunAndExpectFailure(t, "fleet", "activate",
		"--email", "hody@hody.dev",
		"--admin-password", "pw12345678",
		"--passphrase", "seal-me-please",
		"--storage", "cloud")

	require.Contains(t, strings.Join(stderr, "\n"), "add the cloud target in the dashboard")
	require.NoFileExists(t, filepath.Join(fleet.StateDirFor(fleetConfigFile(e)), "seal.key"))
}

// `server start` must refuse to serve a Fleet whose pending sealing key could
// not be resolved: that key may be the only one that opens the store, and
// serving on the old one would seal new secrets into a store nothing can read
// back. A malformed seal.key.new is the cheapest way to produce that state.
func TestServerStartRefusesUnresolvedPendingSealKey(t *testing.T) {
	runner := testenv.NewInProcRunner(t)
	e := testenv.NewCLITest(t, nil, runner)

	stateDir := fleet.StateDirFor(filepath.Join(e.ConfigDir, ".kopia.config"))

	e.RunAndExpectSuccess(t, "fleet", "activate",
		"--email", "hody@hody.dev", "--admin-password", "pw12345678", "--passphrase", "seal-me-please")

	pending := filepath.Join(stateDir, "seal.key.new")
	require.NoError(t, os.WriteFile(pending, []byte("nonsense\n"), 0o600))

	var (
		mu     sync.Mutex
		stderr []string
		sp     testutil.ServerParameters
	)

	// Not RunAndExpectFailure: its wait() never returns if the refusal
	// regresses and the server serves, which turns a regression into a hung
	// package instead of a failing test. ProcessOutput is what unblocks the
	// scan in that case - it returns false once the server announces its
	// address, which only a server that came up ever prints.
	wait, kill := e.RunAndProcessStderr(t, func(line string) bool {
		mu.Lock()
		defer mu.Unlock()

		stderr = append(stderr, line)

		return sp.ProcessOutput(line)
	}, "server", "start",
		"--insecure", "--without-password", "--no-ui", "--no-grpc",
		"--address=127.0.0.1:0", "--server-control-password=admin-pwd")

	done := make(chan error, 1)

	go func() { done <- wait() }()

	select {
	case err := <-done:
		require.Error(t, err, "server start must fail on an unresolved pending sealing key")
	case <-time.After(30 * time.Second):
		// Only here: the in-process runner's interrupt channel is closed once
		// the command returns, so killing a finished one panics.
		kill()
		t.Fatal("server start did not refuse; it is serving on an unresolved pending seal key")
	}

	mu.Lock()
	defer mu.Unlock()

	require.Contains(t, strings.Join(stderr, "\n"), "cannot be used")
	require.FileExists(t, pending, "the pending key must not be deleted to make the server start")
}

// TestFleetJobsRunQueuesARow pins the CLI half of the jobs surface: the
// command writes a pending row the Fleet server's scheduler will claim, and
// refuses to invent a fleet database for a WarpHold that has none.
func TestFleetJobsRunQueuesARow(t *testing.T) {
	runner := testenv.NewInProcRunner(t)
	e := testenv.NewCLITest(t, nil, runner)

	configFile := filepath.Join(e.ConfigDir, ".kopia.config")
	stateDir := fleet.StateDirFor(configFile)

	// Before activation there is no fleet, and no database is created either.
	e.RunAndExpectFailure(t, "fleet", "jobs", "run", "--kind", "verify")
	require.NoFileExists(t, filepath.Join(stateDir, "fleet.db"))

	e.RunAndExpectSuccess(t, "fleet", "activate",
		"--email", "hody@hody.dev",
		"--admin-password", "pw12345678",
		"--passphrase", "seal-me-please")

	out := e.RunAndExpectSuccess(t, "fleet", "jobs", "run", "--kind", "test-restore")
	require.Contains(t, strings.Join(out, "\n"), "Queued test-restore job")

	e.RunAndExpectFailure(t, "fleet", "jobs", "run", "--kind", "nonesuch")
	e.RunAndExpectFailure(t, "fleet", "jobs", "run", "--kind", "verify", "--agent", "ag_nope")

	st, err := store.Open(fleet.PathsFor(stateDir).DB)
	require.NoError(t, err)

	defer st.Close() //nolint:errcheck // test cleanup

	js, err := st.RecentJobs(context.Background(), "", 50)
	require.NoError(t, err)

	// Not "exactly one row": `fleet activate` now provisions the host's own
	// repository and the setup defaults (Task 20), which takes long enough
	// that the scheduler it starts has already enqueued and run this fleet's
	// interval-driven kinds. What this test owns is what the CLI queued.
	var queued []store.Job

	for _, j := range js {
		if j.Status == "pending" {
			queued = append(queued, j)
		}

		require.NotEqual(t, "nonesuch", j.Kind, "an unknown kind is rejected before it is written")
		require.NotEqual(t, "ag_nope", j.AgentID, "an unknown agent is rejected before it is written")
	}

	require.Len(t, queued, 1, "only the accepted kind was queued")
	require.Equal(t, "test-restore", queued[0].Kind)
	require.Empty(t, queued[0].AgentID)
}
