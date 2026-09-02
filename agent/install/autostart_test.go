package install_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/agent/install"
)

func TestAutostartEntry(t *testing.T) {
	e, err := install.Autostart("/home/user/.local/bin/warphold")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(e, "[Desktop Entry]\n"))
	require.Contains(t, e, "Type=Application")
	require.Contains(t, e, "Exec=\"/home/user/.local/bin/warphold\" agent tray")
	require.Contains(t, e, "X-GNOME-Autostart-enabled=true")
	require.Contains(t, e, "Terminal=false")
}

// TestAutostartRejectsInjectableBinary pins that a binary path carrying a
// newline cannot append further Desktop Entry keys, and that the reserved
// characters of a quoted Exec value are escaped rather than passed through.
func TestAutostartRejectsInjectableBinary(t *testing.T) {
	_, err := install.Autostart("/tmp/x\nExec=/bin/sh -c evil")
	require.Error(t, err)
	require.Contains(t, err.Error(), "newlines")

	_, err = install.Autostart("")
	require.Error(t, err)

	e, err := install.Autostart("/tmp/we`ird$bin")
	require.NoError(t, err)
	require.Contains(t, e, "Exec=\"/tmp/we\\`ird\\$bin\" agent tray")
}

// TestUserInstallWritesAutostart pins that 'agent install' in user scope
// installs the tray's autostart entry alongside the service unit, and that
// the system scope does not (it has no user session to autostart into).
func TestUserInstallWritesAutostart(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)

	p, err := install.Systemd("user", "/home/user/.local/bin/warphold")
	require.NoError(t, err)

	desktop, ok := p.Files[install.AutostartPath(cfg)]
	require.True(t, ok, "user install writes %s", install.AutostartPath(cfg))
	require.Contains(t, desktop, "agent tray")
	require.Equal(t, filepath.Join(cfg, "autostart", "warphold-tray.desktop"), install.AutostartPath(cfg))

	s, err := install.Systemd("system", "/usr/local/bin/warphold")
	require.NoError(t, err)

	for path := range s.Files {
		require.NotContains(t, path, "autostart")
	}
}

// TestAutostartEscapesFieldCodes pins that a "%" in the binary path is
// doubled: an un-escaped "%f" in Exec is a field code the desktop expands
// into a file argument, not part of the path.
func TestAutostartEscapesFieldCodes(t *testing.T) {
	e, err := install.Autostart("/tmp/100%f/warphold")
	require.NoError(t, err)
	require.Contains(t, e, "Exec=\"/tmp/100%%f/warphold\" agent tray")
}
