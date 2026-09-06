package install_test

import (
	"os"
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

// TestResolveAppUnitAbsent pins that agent install is a no-op toward the app
// when the app was never installed on this machine: no command is added, and
// the caller gets AppUnitAbsent.
func TestResolveAppUnitAbsent(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	p := install.Plan{Commands: [][]string{{"systemctl", "--user", "daemon-reload"}}}
	status, err := install.ResolveAppUnit(&p, "user")
	require.NoError(t, err)
	require.Equal(t, install.AppUnitAbsent, status)
	require.Equal(t, [][]string{{"systemctl", "--user", "daemon-reload"}}, p.Commands)
}

// TestResolveAppUnitSuperseded pins that a user-scope agent install stops and
// disables an app unit installed at the same scope, before the agent's own
// enable/start command so the two engines never run at once, and touches
// only the unit: the app's state directory is never referenced, let alone
// removed.
func TestResolveAppUnitSuperseded(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	writeAppUnit(t, cfg)

	p := install.Plan{Commands: [][]string{{"systemctl", "--user", "enable", "--now", "warphold-agent"}}}
	status, err := install.ResolveAppUnit(&p, "user")
	require.NoError(t, err)
	require.Equal(t, install.AppUnitSuperseded, status)
	require.Equal(t, [][]string{
		{"systemctl", "--user", "disable", "--now", "warphold-app"},
		{"systemctl", "--user", "enable", "--now", "warphold-agent"},
	}, p.Commands)
}

// TestResolveAppUnitOtherScope pins that a system-scope agent install never
// guesses which user's session to touch: it reports the mismatch instead of
// appending a command, since the app unit is always user-scope.
func TestResolveAppUnitOtherScope(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	writeAppUnit(t, cfg)

	p := install.Plan{Commands: [][]string{{"systemctl", "daemon-reload"}}}
	status, err := install.ResolveAppUnit(&p, "system")
	require.NoError(t, err)
	require.Equal(t, install.AppUnitOtherScope, status)
	require.Equal(t, [][]string{{"systemctl", "daemon-reload"}}, p.Commands)
}

// TestResolveAppUnitOtherScopeCannotSeeUserApp pins that a system-scope
// install reports AppUnitOtherScope even when it cannot find an app unit
// under its own (root) config directory: it must not read that as proof no
// user on the machine has the app, so it never silently falls back to
// AppUnitAbsent for system scope.
func TestResolveAppUnitOtherScopeCannotSeeUserApp(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	p := install.Plan{Commands: [][]string{{"systemctl", "daemon-reload"}}}
	status, err := install.ResolveAppUnit(&p, "system")
	require.NoError(t, err)
	require.Equal(t, install.AppUnitOtherScope, status)
	require.Equal(t, [][]string{{"systemctl", "daemon-reload"}}, p.Commands)
}

func writeAppUnit(t *testing.T, cfg string) {
	t.Helper()
	path := install.AppUnitPath(cfg)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("[Unit]\n"), 0o644))
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
