package install_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/agent/install"
)

func TestSystemdPlans(t *testing.T) {
	// XDG_CONFIG_HOME, not HOME: os.UserHomeDir reads a different variable on
	// Windows, so a HOME-only test pins a path this code never produces there.
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	p, err := install.Systemd("user", "/home/hody/.local/bin/warphold")
	require.NoError(t, err)
	unit, ok := p.Files[filepath.Join(cfg, "systemd", "user", "warphold-agent.service")]
	require.True(t, ok)
	require.Contains(t, unit, "ExecStart=\"/home/hody/.local/bin/warphold\" agent run --scope user")
	require.Contains(t, unit, "RestartPreventExitStatus=3")
	require.Contains(t, unit, "StartLimitIntervalSec=600")
	require.Contains(t, unit, "StartLimitBurst=5")
	require.Contains(t, unit, "WantedBy=default.target")
	require.Equal(t, [][]string{{"systemctl", "--user", "daemon-reload"}, {"systemctl", "--user", "enable", "--now", "warphold-agent"}, {"loginctl", "enable-linger"}}, p.Commands)

	s, err := install.Systemd("system", "/usr/local/bin/warphold")
	require.NoError(t, err)
	unit = s.Files["/etc/systemd/system/warphold-agent.service"]
	require.Contains(t, unit, "--scope system")
	require.Contains(t, unit, "WantedBy=multi-user.target")
	require.Equal(t, [][]string{{"systemctl", "daemon-reload"}, {"systemctl", "enable", "--now", "warphold-agent"}}, s.Commands)
}

func TestApplyWritesAndRuns(t *testing.T) {
	dir := t.TempDir()
	p := install.Plan{Files: map[string]string{filepath.Join(dir, "a", "x.service"): "unit"}, Commands: [][]string{{"systemctl", "daemon-reload"}}}
	var ran []string
	require.NoError(t, install.Apply(p, func(name string, args ...string) error {
		ran = append(ran, name+" "+strings.Join(args, " "))
		return nil
	}))
	require.Equal(t, []string{"systemctl daemon-reload"}, ran)
	require.FileExists(t, filepath.Join(dir, "a", "x.service"))
}

func TestSystemdRejectsRelativeConfigDir(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "relative/config")
	_, err := install.Systemd("user", "/home/hody/.local/bin/warphold")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not absolute")
}

// TestSystemdRejectsTraversalInConfigDir pins the fix for an XDG_CONFIG_HOME
// that is absolute but escapes its own directory via "..": IsAbs alone would
// accept it, and the unit would land wherever filepath.Join resolves it to
// (e.g. /etc) rather than under the user's config directory.
func TestSystemdRejectsTraversalInConfigDir(t *testing.T) {
	// Concatenated, not filepath.Join: Join cleans, and cleaning is exactly
	// what this test needs Systemd to refuse to do on its behalf.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()+string(filepath.Separator)+"..")
	_, err := install.Systemd("user", "/home/hody/.local/bin/warphold")
	require.Error(t, err)
	require.Contains(t, err.Error(), `must not contain ".."`)
}

// TestSystemdAcceptsTrailingSeparatorInConfigDir pins that a trailing
// separator is normalized rather than rejected: it escapes nothing, and
// "XDG_CONFIG_HOME=$HOME/.config/" is a perfectly ordinary shell export.
func TestSystemdAcceptsTrailingSeparatorInConfigDir(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg+string(filepath.Separator))
	p, err := install.Systemd("user", "/home/hody/.local/bin/warphold")
	require.NoError(t, err)
	require.Contains(t, p.Files, filepath.Join(cfg, "systemd", "user", "warphold-agent.service"))
}

// TestSystemdCarriesStateDirEnvironment pins that an agent enrolled with a
// non-default WARPHOLD_STATE_DIR keeps that directory once systemd is the one
// starting it: the unit runs in a clean environment, so without this the
// service would look for agent.json in the default location and report itself
// unenrolled.
func TestSystemdCarriesStateDirEnvironment(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("WARPHOLD_STATE_DIR", "/srv/warphold state")

	p, err := install.Systemd("user", "/usr/local/bin/warphold")
	require.NoError(t, err)
	require.Contains(t, unitOf(t, p), `Environment="WARPHOLD_STATE_DIR=/srv/warphold state"`)

	s, err := install.Systemd("system", "/usr/local/bin/warphold")
	require.NoError(t, err)
	require.Contains(t, s.Files["/etc/systemd/system/warphold-agent.service"], `Environment="WARPHOLD_STATE_DIR=/srv/warphold state"`)
}

func TestSystemdOmitsStateDirEnvironmentWhenUnset(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("WARPHOLD_STATE_DIR", "")

	p, err := install.Systemd("user", "/usr/local/bin/warphold")
	require.NoError(t, err)
	require.NotContains(t, unitOf(t, p), "WARPHOLD_STATE_DIR")
}

// TestSystemdRejectsInjectableStateDir pins that a state directory containing
// a newline cannot smuggle extra directives (ExecStartPre=, User=root, ...)
// into the generated unit.
func TestSystemdRejectsInjectableStateDir(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("WARPHOLD_STATE_DIR", "/srv/warphold\nExecStartPre=/bin/sh -c evil")

	_, err := install.Systemd("user", "/usr/local/bin/warphold")
	require.Error(t, err)
	require.Contains(t, err.Error(), "WARPHOLD_STATE_DIR")
}

func unitOf(t *testing.T, p install.Plan) string {
	t.Helper()
	require.Len(t, p.Files, 1)

	for _, content := range p.Files {
		return content
	}

	return ""
}

// TestSystemdWritesOnlyUnderInstallRoot pins that every file an install would
// write stays inside its approved directory - the user's config directory, or
// /etc/systemd/system - since the agent runs unattended and both roots are
// derived from the environment.
func TestSystemdWritesOnlyUnderInstallRoot(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)

	p, err := install.Systemd("user", "/usr/local/bin/warphold")
	require.NoError(t, err)
	requireAllUnder(t, cfg, p)

	s, err := install.Systemd("system", "/usr/local/bin/warphold")
	require.NoError(t, err)
	requireAllUnder(t, "/etc/systemd/system", s)
}

func requireAllUnder(t *testing.T, root string, p install.Plan) {
	t.Helper()
	require.NotEmpty(t, p.Files)

	for path := range p.Files {
		rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
		require.NoError(t, err)
		require.NotEqual(t, "..", rel)
		require.False(t, strings.HasPrefix(rel, ".."+string(filepath.Separator)), "%q escapes %q", path, root)
	}
}
