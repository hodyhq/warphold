package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kopia/kopia/agent/install"
	"github.com/kopia/kopia/agent/state"
)

// commandAgentInstall installs the agent as a systemd service that starts at
// boot (user-scope with lingering enabled, or system-scope).
type commandAgentInstall struct {
	scope  string
	dryRun bool
	svc    advancedAppServices
	out    textOutput
}

func (c *commandAgentInstall) setup(svc advancedAppServices, parent commandParent) {
	cmd := parent.Command("install", "Install the agent as a systemd service that starts at boot.")
	cmd.Flag("scope", "user or system").Default("user").EnumVar(&c.scope, "user", "system")
	cmd.Flag("dry-run", "Print what would be written and run").BoolVar(&c.dryRun)
	c.svc = svc
	c.out.setup(svc)
	cmd.Action(svc.noRepositoryAction(c.run))
}

func (c *commandAgentInstall) run(ctx context.Context) error {
	bin, err := os.Executable()
	if err != nil {
		return err
	}

	p, err := install.Systemd(c.scope, bin)
	if err != nil {
		return err
	}

	c.out.printStdout("%s", pathHint(bin))

	// Enrollment supersedes the standalone app on the same user: two engines
	// on one machine would mean two repositories and two trays.
	status, err := install.ResolveAppUnit(&p, c.scope)
	if err != nil {
		return err
	}

	if status == install.AppUnitOtherScope {
		c.out.printStderr("warning: this %s-scope agent install cannot tell whether a user on this machine already runs the standalone app (%s) - a system-scope install can't see into a user's session to check or stop it safely. Both may now run; disable the app yourself with 'systemctl --user disable --now %s' from that user's session if that is not wanted.\n",
			c.scope, install.AppUnitName, install.AppUnitName)
	}

	if c.dryRun {
		if status == install.AppUnitSuperseded {
			c.out.printStdout("Would stop and disable the standalone app's service (%s): this agent replaces it.\n", install.AppUnitName)
		}

		for path, content := range p.Files {
			c.out.printStdout("--- %s\n%s\n", path, content)
		}

		for _, cmd := range p.Commands {
			c.out.printStdout("$ %s\n", strings.Join(cmd, " "))
		}

		return nil
	}

	if err := install.Apply(p, func(name string, args ...string) error {
		cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec
		cmd.Stdout, cmd.Stderr = c.out.stdout(), c.out.stderr()

		return cmd.Run()
	}); err != nil {
		return err
	}

	if status == install.AppUnitSuperseded {
		c.out.printStdout("Stopped and disabled the standalone app's service (%s): this agent replaces it.\n"+
			"Your existing backups stay in %s; the agent's tray replaces the app's.\n",
			install.AppUnitName, state.Dir(state.ScopeApp))
	}

	return nil
}

// pathHint is a one-line note when the installed binary's directory is not on
// PATH, or "" when it is. A user-scope install lives in ~/.local/bin, which
// many shells add only for login sessions, and without it 'warphold agent
// status' fails in every other shell. It is printed before the install is
// applied, so a session where systemd refuses still shows it.
func pathHint(bin string) string {
	dir := filepath.Dir(bin)
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		if p != "" && filepath.Clean(p) == dir {
			return ""
		}
	}

	if home, err := os.UserHomeDir(); err == nil && dir == filepath.Join(home, ".local", "bin") {
		dir = "$HOME/.local/bin"
	}

	if strings.HasSuffix(os.Getenv("SHELL"), "/fish") {
		return fmt.Sprintf("note: %s is not in your PATH; add 'fish_add_path %s' to your shell profile\n", dir, dir)
	}

	return fmt.Sprintf("note: %s is not in your PATH; add export PATH=\"%s:$PATH\" to your shell profile\n", dir, dir)
}
