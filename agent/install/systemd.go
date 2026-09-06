// Package install writes service units so the agent runs at boot.
package install

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/agent/state"
	"github.com/kopia/kopia/internal/ospath"
)

// unitName is the service file both agent scopes install, appUnitName the one
// the standalone app installs; systemUnitDir is the only directory a
// system-scope install may write to.
const (
	unitName      = "warphold-agent.service"
	appUnitName   = "warphold-app.service"
	systemUnitDir = "/etc/systemd/system"
)

// AppUnitName is the standalone app's systemd unit, without the extension:
// what "systemctl --user <verb>" takes.
const AppUnitName = "warphold-app"

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
Description=%s
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=600
StartLimitBurst=5

[Service]
%sExecStart="%s" %s
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
	if scope == state.ScopeSystem {
		u, err := unit(binary, agentUnit(state.ScopeSystem), "multi-user.target")
		if err != nil {
			return Plan{}, err
		}

		return planUnder(systemUnitDir, map[string]string{
			filepath.Join(systemUnitDir, unitName): u,
		}, [][]string{{"systemctl", "daemon-reload"}, {"systemctl", "enable", "--now", "warphold-agent"}})
	}

	cfg, err := UserConfigDir()
	if err != nil {
		return Plan{}, err
	}

	u, err := unit(binary, agentUnit(state.ScopeUser), "default.target")
	if err != nil {
		return Plan{}, err
	}

	// The tray is a login-session program, not a service: it needs the
	// user's D-Bus session bus and their panel, so it autostarts with the
	// desktop rather than with systemd.
	desktop, err := Autostart(binary, state.ScopeUser)
	if err != nil {
		return Plan{}, err
	}

	return planUnder(cfg, map[string]string{
		filepath.Join(cfg, "systemd", "user", unitName): u,
		AutostartPath(cfg): desktop,
	}, [][]string{{"systemctl", "--user", "daemon-reload"}, {"systemctl", "--user", "enable", "--now", "warphold-agent"}, {"loginctl", "enable-linger"}})
}

// agentUnit is the ExecStart argument list and unit description for an agent
// of the given scope.
func agentUnit(scope string) unitSpec {
	return unitSpec{desc: "WarpHold backup agent", args: "agent run --scope " + scope}
}

// SystemdApp is the standalone single-machine app: a user-scope service
// running the local engine, plus the tray autostart pointed at it. There is
// no system scope - the app is one person's backup engine, running as them,
// reachable only on their loopback.
func SystemdApp(binary string) (Plan, error) {
	cfg, err := UserConfigDir()
	if err != nil {
		return Plan{}, err
	}

	u, err := unit(binary, unitSpec{desc: "WarpHold app", args: "app run"}, "default.target")
	if err != nil {
		return Plan{}, err
	}

	desktop, err := Autostart(binary, state.ScopeApp)
	if err != nil {
		return Plan{}, err
	}

	return planUnder(cfg, map[string]string{
		AppUnitPath(cfg):      u,
		AppAutostartPath(cfg): desktop,
	}, [][]string{{"systemctl", "--user", "daemon-reload"}, {"systemctl", "--user", "enable", "--now", AppUnitName}, {"loginctl", "enable-linger"}})
}

// AppUnitPath is where the standalone app's unit lives. dir is the user's
// config directory.
func AppUnitPath(dir string) string {
	return filepath.Join(dir, "systemd", "user", appUnitName)
}

// AppUnitStatus is what ResolveAppUnit found about the standalone app's unit
// on this machine.
type AppUnitStatus int

const (
	// AppUnitAbsent means the app was never installed here: nothing to do.
	AppUnitAbsent AppUnitStatus = iota
	// AppUnitSuperseded means the app unit is installed at the same (user)
	// scope as the agent being installed, and has been added to the plan's
	// commands to be stopped and disabled.
	AppUnitSuperseded
	// AppUnitOtherScope means the agent is installing at system scope, which
	// runs as root and cannot see into any user's session: it can neither
	// confirm nor rule out a user-scope app unit on this machine, so this is
	// left to the caller to warn about instead of guessing.
	AppUnitOtherScope
)

// ResolveAppUnit checks whether the standalone app's unit - always installed
// at user scope - is present, and prepends the commands to stop and disable
// it to p.Commands when agentScope matches (enrollment supersedes the
// standalone app on the same user): the app is stopped before the agent's own
// enable/start command runs, so the two engines never run at once. Only the
// unit is touched: its state directory and repository are left on disk, so
// existing local backups stay recoverable.
//
// A system-scope install runs as root, so checking root's own config
// directory cannot tell us anything about a real user's app unit: it always
// reports AppUnitOtherScope without probing the filesystem, rather than
// risking a false AppUnitAbsent that would let two engines run undetected.
func ResolveAppUnit(p *Plan, agentScope string) (AppUnitStatus, error) {
	if agentScope != state.ScopeUser {
		return AppUnitOtherScope, nil
	}

	cfg, err := UserConfigDir()
	if err != nil {
		return AppUnitAbsent, err
	}

	if _, err := os.Stat(AppUnitPath(cfg)); err != nil {
		if os.IsNotExist(err) {
			return AppUnitAbsent, nil
		}

		return AppUnitAbsent, err
	}

	p.Commands = append([][]string{{"systemctl", "--user", "disable", "--now", AppUnitName}}, p.Commands...)

	return AppUnitSuperseded, nil
}

// UserConfigDir resolves the user's config directory for a user-scope
// install: XDG_CONFIG_HOME when set, otherwise ~/.config. An unresolvable or
// relative directory is an error rather than a unit written somewhere
// relative to the current working directory, where systemd will never find
// it.
func UserConfigDir() (string, error) {
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

// unitSpec is what differs between the units this package writes: the
// Description and the arguments after the binary in ExecStart. Both are
// package constants, never user input.
type unitSpec struct {
	desc string
	args string
}

// unit renders the service file. A WARPHOLD_STATE_DIR set at install time is
// carried into the unit: systemd starts services in a clean environment, so
// without it the installed service would look for its state in the default
// directory - an agent would report itself unenrolled, and the app would open
// the wrong repository.
func unit(binary string, spec unitSpec, wantedBy string) (string, error) {
	if err := checkBinary(binary); err != nil {
		return "", err
	}

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

	return strings.TrimSpace(fmt.Sprintf(unitTmpl, spec.desc, env, systemdArg(binary), spec.args, wantedBy)) + "\n", nil
}

// systemdArg escapes a path for the inside of a double-quoted systemd
// argument. systemd unescapes C-style sequences there, so a literal backslash
// must be doubled, and "%" is doubled because systemd expands "%x"
// specifiers. A quote or newline never reaches this - checkBinary refuses
// those outright.
func systemdArg(s string) string {
	return strings.NewReplacer(`\`, `\\`, "%", "%%").Replace(s)
}

// checkBinary refuses binary paths that cannot be embedded safely in the
// quoted argument of a systemd unit or of a Desktop Entry. Escaping some of
// them would be possible, but both callers embed os.Executable() - an
// installed program's own path - so a path carrying a quote, a newline or an
// "=" is an injection attempt far more likely than a real install location.
func checkBinary(binary string) error {
	if binary == "" {
		return errors.New("empty binary path")
	}

	// A double quote closes the quoted argument and everything after it is
	// read as further arguments, or past a newline as further directives:
	//   ExecStart="/tmp/warphold" ExecStartPre=/bin/false"
	if strings.ContainsAny(binary, "\"\n\r") {
		return errors.Errorf("binary path %q must not contain quotes or newlines", binary)
	}

	// Desktop Entry and systemd unit lines are both key=value.
	if strings.Contains(binary, "=") {
		return errors.Errorf("binary path %q must not contain \"=\"", binary)
	}

	return nil
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
