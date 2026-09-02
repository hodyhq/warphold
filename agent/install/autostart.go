package install

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/pkg/errors"
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

// execArg quotes the binary for a desktop-entry Exec key. The value is
// written into a file the session autostarts, so a path carrying a newline
// (which would add further Desktop Entry keys) is refused rather than
// escaped; inside the quoted value, the reserved characters the spec lists
// are backslash-escaped, and a literal "%" is doubled so it is not read as a
// field code (%f, %U, ...).
func execArg(binary string) (string, error) {
	if binary == "" {
		return "", errors.New("empty binary path")
	}

	if strings.ContainsAny(binary, "\n\r") {
		return "", errors.Errorf("binary path %q must not contain newlines", binary)
	}

	r := strings.NewReplacer(`\`, `\\\\`, `"`, `\"`, "`", "\\`", "$", `\$`, "%", "%%")

	return `"` + r.Replace(binary) + `"`, nil
}
