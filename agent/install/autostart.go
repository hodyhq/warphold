package install

import (
	"fmt"
	"path/filepath"
	"strings"
)

// autostartName is the XDG autostart entry that starts the tray at login.
const autostartName = "warphold-tray.desktop"

const autostartTmpl = `[Desktop Entry]
Type=Application
Name=WarpHold
Comment=WarpHold backup status
Exec=%s agent tray
Terminal=false
NoDisplay=false
X-GNOME-Autostart-enabled=true
`

// AutostartPath is where the tray's autostart entry lives for a user-scope
// install. dir is the user's config directory.
func AutostartPath(dir string) string { return filepath.Join(dir, "autostart", autostartName) }

// Autostart renders ~/.config/autostart/warphold-tray.desktop.
func Autostart(binary string) (string, error) {
	exec, err := execArg(binary)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf(autostartTmpl, exec), nil
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
