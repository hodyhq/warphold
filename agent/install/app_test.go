package install_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/agent/install"
)

// TestSystemdAppPlan pins the standalone app's install: a user-scope unit
// running "app run" - not "agent run", which would need an enrollment this
// machine does not have - plus a tray entry pointed at the app's engine.
func TestSystemdAppPlan(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("WARPHOLD_STATE_DIR", "")

	p, err := install.SystemdApp("/home/hody/.local/bin/warphold")
	require.NoError(t, err)

	unit, ok := p.Files[filepath.Join(cfg, "systemd", "user", "warphold-app.service")]
	require.True(t, ok, "app install writes warphold-app.service")
	require.Contains(t, unit, `ExecStart="/home/hody/.local/bin/warphold" app run`)
	require.NotContains(t, unit, "agent run")
	require.Contains(t, unit, "Description=WarpHold app")
	require.Contains(t, unit, "Restart=on-failure")
	require.Contains(t, unit, "StartLimitIntervalSec=600")
	require.Contains(t, unit, "StartLimitBurst=5")
	require.Contains(t, unit, "WantedBy=default.target")

	desktop, ok := p.Files[install.AppAutostartPath(cfg)]
	require.True(t, ok, "app install writes the tray autostart entry")
	require.Contains(t, desktop, `agent tray --scope app`)

	// Its own file: installing the app must not take an agent's tray away.
	require.NotContains(t, p.Files, install.AutostartPath(cfg))
	require.Equal(t, filepath.Join(cfg, "autostart", "warphold-app-tray.desktop"), install.AppAutostartPath(cfg))

	require.Equal(t, [][]string{
		{"systemctl", "--user", "daemon-reload"},
		{"systemctl", "--user", "enable", "--now", "warphold-app"},
		{"loginctl", "enable-linger"},
	}, p.Commands)

	requireAllUnder(t, cfg, p)
}

// TestSystemdAppCarriesStateDir pins that an app installed with a
// non-default WARPHOLD_STATE_DIR keeps it once systemd starts the service,
// which otherwise runs in a clean environment and would open a different
// (empty) repository.
func TestSystemdAppCarriesStateDir(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("WARPHOLD_STATE_DIR", "/srv/warphold state")

	p, err := install.SystemdApp("/usr/local/bin/warphold")
	require.NoError(t, err)
	require.Contains(t, unitOf(t, p), `Environment="WARPHOLD_STATE_DIR=/srv/warphold state"`)
}

// TestSystemdAppRejectsHostileInputs pins that the app install refuses the
// same injections the agent install does - it renders the same unit and the
// same desktop entry.
func TestSystemdAppRejectsHostileInputs(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	for name, bin := range map[string]string{
		"double quote": `/tmp/warphold" ExecStartPre=/bin/false"`,
		"newline":      "/tmp/warphold\nExecStartPre=/bin/false",
		"equals":       "/tmp/warp=hold",
		"empty":        "",
	} {
		_, err := install.SystemdApp(bin)
		require.Error(t, err, name)
	}

	t.Setenv("XDG_CONFIG_HOME", t.TempDir()+string(filepath.Separator)+"..")

	_, err := install.SystemdApp("/usr/local/bin/warphold")
	require.Error(t, err)
	require.Contains(t, err.Error(), `must not contain ".."`)
}

// TestAutostartScope pins which engine an autostart entry's tray watches: the
// default scope is left off the command line, so an agent install keeps
// writing the entry it always has.
func TestAutostartScope(t *testing.T) {
	agent, err := install.Autostart("/usr/local/bin/warphold", "user")
	require.NoError(t, err)
	require.Contains(t, agent, "agent tray\n")

	app, err := install.Autostart("/usr/local/bin/warphold", "app")
	require.NoError(t, err)
	require.Contains(t, app, "agent tray --scope app\n")

	sys, err := install.Autostart("/usr/local/bin/warphold", "system")
	require.NoError(t, err)
	require.Contains(t, sys, "agent tray --scope system\n")

	// An unknown scope reaches an Exec line, so it is refused rather than
	// rendered.
	_, err = install.Autostart("/usr/local/bin/warphold", "app --scope evil")
	require.Error(t, err)
}

// TestAppAndAgentUnitsCoexist pins that installing both on one machine writes
// two different units: the app is not a replacement for an enrolled agent.
func TestAppAndAgentUnitsCoexist(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)

	app, err := install.SystemdApp("/usr/local/bin/warphold")
	require.NoError(t, err)

	agent, err := install.Systemd("user", "/usr/local/bin/warphold")
	require.NoError(t, err)

	for path := range app.Files {
		require.NotContains(t, agent.Files, path, "%s would be overwritten by the other install", path)
	}
}
