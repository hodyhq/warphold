package cli

import (
	"context"
	"os"
	"os/exec"

	"github.com/kopia/kopia/agent/install"
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

func (c *commandAgentInstall) run(_ context.Context) error {
	bin, err := os.Executable()
	if err != nil {
		return err
	}

	p := install.Systemd(c.scope, bin)

	if c.dryRun {
		for path, content := range p.Files {
			c.out.printStdout("--- %s\n%s\n", path, content)
		}

		for _, cmd := range p.Commands {
			c.out.printStdout("$ %s\n", cmd)
		}

		return nil
	}

	return install.Apply(p, func(name string, args ...string) error {
		cmd := exec.Command(name, args...) //nolint:gosec
		cmd.Stdout, cmd.Stderr = c.out.stdout(), c.out.stderr()

		return cmd.Run()
	})
}
