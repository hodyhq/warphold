// Package install writes service units so the agent runs at boot.
package install

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/internal/ospath"
)

// unitName is the service file both scopes install; systemUnitDir is the only
// directory a system-scope install may write to.
const (
	unitName      = "warphold-agent.service"
	systemUnitDir = "/etc/systemd/system"
)

// Plan is what an install will do, so it can be printed (--dry-run) or applied.
type Plan struct {
	Files    map[string]string
	Commands [][]string
}

// StartLimitIntervalSec/StartLimitBurst bound Restart=on-failure: without
// them a unit that fails immediately on every start would be restarted
// forever. Five starts inside ten minutes (RestartSec=30 between them) put the
// unit into "failed", where an operator can see it.
const unitTmpl = `[Unit]
Description=WarpHold backup agent
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=600
StartLimitBurst=5

[Service]
%sExecStart="%s" agent run --scope %s
Restart=on-failure
RestartSec=30
RestartPreventExitStatus=3
Nice=19
IOSchedulingClass=idle

[Install]
WantedBy=%s
`

// Systemd returns the plan for a scope. User scope resolves the unit
// directory from XDG_CONFIG_HOME or the user's home directory; an
// unresolvable or relative directory is an error rather than a unit written
// somewhere relative to the current working directory, where systemd will
// never find it.
func Systemd(scope, binary string) (Plan, error) {
	if scope == "system" {
		u, err := unit(binary, "system", "multi-user.target")
		if err != nil {
			return Plan{}, err
		}

		return planUnder(systemUnitDir, map[string]string{
			filepath.Join(systemUnitDir, unitName): u,
		}, [][]string{{"systemctl", "daemon-reload"}, {"systemctl", "enable", "--now", "warphold-agent"}})
	}

	cfg, err := userConfigDir()
	if err != nil {
		return Plan{}, err
	}

	u, err := unit(binary, "user", "default.target")
	if err != nil {
		return Plan{}, err
	}

	// The tray is a login-session program, not a service: it needs the
	// user's D-Bus session bus and their panel, so it autostarts with the
	// desktop rather than with systemd.
	desktop, err := Autostart(binary)
	if err != nil {
		return Plan{}, err
	}

	return planUnder(cfg, map[string]string{
		filepath.Join(cfg, "systemd", "user", unitName): u,
		AutostartPath(cfg): desktop,
	}, [][]string{{"systemctl", "--user", "daemon-reload"}, {"systemctl", "--user", "enable", "--now", "warphold-agent"}, {"loginctl", "enable-linger"}})
}

// userConfigDir resolves the user's config directory for a user-scope
// install: XDG_CONFIG_HOME when set, otherwise ~/.config. An unresolvable or
// relative directory is an error rather than a unit written somewhere
// relative to the current working directory, where systemd will never find
// it.
func userConfigDir() (string, error) {
	cfg := os.Getenv("XDG_CONFIG_HOME")
	if cfg == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", errors.Wrap(err, "unable to determine home directory for user-scope install")
		}

		cfg = filepath.Join(home, ".config")
	}

	if !ospath.IsAbs(cfg) {
		return "", errors.Errorf("config directory %q is not absolute", cfg)
	}

	// XDG_CONFIG_HOME is trusted to be absolute but not to be free of "..": a
	// value containing it (e.g. "/home/user/../../etc") still passes IsAbs and
	// Clean resolves it lexically, so the unit could land outside the user's
	// config directory entirely. Reject ".." explicitly rather than rejecting
	// every path Clean would change: a trailing separator or a redundant "."
	// is harmless and normalizing it is the whole job of Clean.
	for _, e := range strings.Split(filepath.ToSlash(cfg), "/") {
		if e == ".." {
			return "", errors.Errorf("config directory %q must not contain \"..\"", cfg)
		}
	}

	return filepath.Clean(cfg), nil
}

// planUnder builds a plan after checking that every file it would write
// resolves inside root - the user's config directory for a user-scope
// install, /etc/systemd/system for a system-scope one. The agent runs
// unattended, and both roots are partly derived from the environment
// (XDG_CONFIG_HOME, HOME), so this is the single place that refuses to let an
// install touch host configuration anywhere else.
func planUnder(root string, files map[string]string, commands [][]string) (Plan, error) {
	root = filepath.Clean(root)

	for path := range files {
		rel, err := filepath.Rel(root, filepath.Clean(path))
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return Plan{}, errors.Errorf("refusing to write %q outside the install directory %q", path, root)
		}
	}

	return Plan{Files: files, Commands: commands}, nil
}

// unit renders the service file. A WARPHOLD_STATE_DIR set at install time is
// carried into the unit: systemd starts services in a clean environment, so
// without it the installed service would look for agent.json in the default
// directory and report itself unenrolled.
func unit(binary, scope, wantedBy string) (string, error) {
	env := ""

	if d := os.Getenv("WARPHOLD_STATE_DIR"); d != "" {
		// A newline would let the value append arbitrary directives to the
		// unit (ExecStartPre=, User=root); a double quote would break out of
		// the quoted value it lands in.
		if strings.ContainsAny(d, "\"\n\r") {
			return "", errors.Errorf("WARPHOLD_STATE_DIR %q must not contain quotes or newlines", d)
		}

		// Quoted, so a directory containing spaces survives systemd's word
		// splitting; "%" doubled, because systemd expands "%x" specifiers.
		env = fmt.Sprintf("Environment=\"WARPHOLD_STATE_DIR=%s\"\n", strings.ReplaceAll(d, "%", "%%"))
	}

	return strings.TrimSpace(fmt.Sprintf(unitTmpl, env, binary, scope, wantedBy)) + "\n", nil
}

// Apply writes the files then runs the commands.
func Apply(p Plan, runCmd func(name string, args ...string) error) error {
	for path, content := range p.Files {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}

		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return err
		}
	}

	for _, c := range p.Commands {
		if err := runCmd(c[0], c[1:]...); err != nil {
			return err
		}
	}

	return nil
}
