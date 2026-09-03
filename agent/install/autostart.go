package install

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/agent/state"
)

// The XDG autostart entries that start a tray at login. The agent's and the
// app's are separate files on purpose: a machine can be both an enrolled
// Fleet device and a standalone app, and one file name would mean whichever
// was installed last silently took the other one's tray away.
const (
	autostartName    = "warphold-tray.desktop"
	appAutostartName = "warphold-app-tray.desktop"
)

const autostartTmpl = `[Desktop Entry]
Type=Application
Name=WarpHold
Comment=WarpHold backup status
Exec=%s agent tray%s
Terminal=false
NoDisplay=false
X-GNOME-Autostart-enabled=true
`

// AutostartPath is where the agent tray's autostart entry lives for a
// user-scope install. dir is the user's config directory.
func AutostartPath(dir string) string { return filepath.Join(dir, "autostart", autostartName) }

// AppAutostartPath is the same for the standalone app's tray.
func AppAutostartPath(dir string) string { return filepath.Join(dir, "autostart", appAutostartName) }

// Autostart renders ~/.config/autostart/warphold-tray.desktop. scope is the
// state scope the tray reads its engine.json from; it is left off the command
// line when it is the tray's own default, so an agent install writes the same
// entry it always has.
//
// The scope reaches a Desktop Entry's Exec line, so only the scopes this
// package knows are allowed there - a caller cannot smuggle further arguments
// or keys in through it.
func Autostart(binary, scope string) (string, error) {
	exec, err := execArg(binary)
	if err != nil {
		return "", err
	}

	args := ""

	switch scope {
	case "", state.ScopeUser:
	case state.ScopeSystem, state.ScopeApp:
		args = " --scope " + scope
	default:
		return "", errors.Errorf("unknown tray scope %q", scope)
	}

	return fmt.Sprintf(autostartTmpl, exec, args), nil
}

// execArg quotes the binary for a desktop-entry Exec key. Two layers unescape
// this value: the desktop file's own string rules first, then the argument
// quoting rules. A character that must reach the argument layer as "\c" has
// to be written "\\c" in the file, and a literal backslash - which both
// layers consume - needs four. "%" is doubled so it is not read as a field
// code (%f, %U, ...). A double quote, a newline and an "=" never get here:
// checkBinary refuses them.
func execArg(binary string) (string, error) {
	if err := checkBinary(binary); err != nil {
		return "", err
	}

	r := strings.NewReplacer(
		`\`, `\\\\`,
		"$", `\\$`,
		"`", "\\\\`",
		"%", "%%",
	)

	return `"` + r.Replace(binary) + `"`, nil
}
