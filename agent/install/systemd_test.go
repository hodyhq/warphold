package install_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/agent/install"
)

func TestSystemdPlans(t *testing.T) {
	t.Setenv("HOME", "/home/hody")
	t.Setenv("XDG_CONFIG_HOME", "")
	p, err := install.Systemd("user", "/home/hody/.local/bin/warphold")
	require.NoError(t, err)
	unit, ok := p.Files["/home/hody/.config/systemd/user/warphold-agent.service"]
	require.True(t, ok)
	require.Contains(t, unit, "ExecStart=/home/hody/.local/bin/warphold agent run --scope user")
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
