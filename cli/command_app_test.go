package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/tests/testenv"
)

// TestAppInstallDryRun pins what 'app install' would write on a machine that
// is not enrolled anywhere: a user-scope unit running "app run" and a tray
// entry pointed at the app's engine - and, being a dry run, nothing on disk.
func TestAppInstallDryRun(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("WARPHOLD_STATE_DIR", "")

	e := testenv.NewCLITest(t, nil, testenv.NewInProcRunner(t))
	out := strings.Join(e.RunAndExpectSuccess(t, "app", "install", "--dry-run"), "\n")

	require.Contains(t, out, filepath.Join(cfg, "systemd", "user", "warphold-app.service"))
	require.Contains(t, out, "app run")
	require.Contains(t, out, "agent tray --scope app")
	require.Contains(t, out, "systemctl --user enable --now warphold-app")
	require.Contains(t, out, filepath.Join(cfg, "autostart", "warphold-app-tray.desktop"))

	require.NoFileExists(t, filepath.Join(cfg, "systemd", "user", "warphold-app.service"))
	require.NoFileExists(t, filepath.Join(cfg, "autostart", "warphold-app-tray.desktop"))
}

// TestAppRefusesWithoutCredentialPersistence pins that the app will not
// install or start a service whose repository password is never written down:
// it would open its repository once, in this terminal, and never again after
// a restart.
func TestAppRefusesWithoutCredentialPersistence(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("WARPHOLD_STATE_DIR", "")

	e := testenv.NewCLITest(t, nil, testenv.NewInProcRunner(t))

	for _, args := range [][]string{
		{"--no-persist-credentials", "app", "install", "--dry-run"},
		{"--no-persist-credentials", "app", "run"},
	} {
		stdout, stderr := e.RunAndExpectFailure(t, args...)
		require.Contains(t, strings.Join(append(stdout, stderr...), "\n"), "--no-persist-credentials")
	}
}

// TestAppUninstallKeepsAnAgentTray pins the one thing an uninstall must not
// do: on a machine that is both a Fleet device and a standalone app, removing
// the agent's tray entry here would leave that install with no status icon.
func TestAppUninstallKeepsAnAgentTray(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("WARPHOLD_STATE_DIR", "")
	// No systemctl on PATH: the uninstall is best-effort about systemd and
	// must still remove the files, and this test must never touch the
	// developer's own units.
	t.Setenv("PATH", "")

	unit := filepath.Join(cfg, "systemd", "user", "warphold-app.service")
	appTray := filepath.Join(cfg, "autostart", "warphold-app-tray.desktop")
	agentUnit := filepath.Join(cfg, "systemd", "user", "warphold-agent.service")
	agentTray := filepath.Join(cfg, "autostart", "warphold-tray.desktop")

	require.NoError(t, os.MkdirAll(filepath.Dir(unit), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Dir(appTray), 0o755))

	for _, f := range []string{unit, appTray, agentUnit, agentTray} {
		require.NoError(t, os.WriteFile(f, []byte("x\n"), 0o644))
	}

	e := testenv.NewCLITest(t, nil, testenv.NewInProcRunner(t))
	out := strings.Join(e.RunAndExpectSuccess(t, "app", "uninstall"), "\n")

	require.NoFileExists(t, unit)
	require.NoFileExists(t, appTray)
	require.FileExists(t, agentUnit, "an agent install is left alone")
	require.FileExists(t, agentTray, "an agent install is left alone")
	require.Contains(t, out, "untouched")

	// It claims only what it actually removed.
	require.Contains(t, out, "- removed "+unit)
	require.Contains(t, out, "- removed "+appTray)

	again := strings.Join(e.RunAndExpectSuccess(t, "app", "uninstall"), "\n")
	require.NotContains(t, again, "- removed ")
}
