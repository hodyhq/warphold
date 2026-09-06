package cli

import (
	"context"
	"os"
	"os/exec"
	"strings"

	"github.com/kopia/kopia/agent/install"
)

// commandAppInstall installs the standalone app: a user-scope systemd service
// running 'app run' and the tray autostart entry pointed at it.
//
// There is no system scope. The app is one person's backup engine: it runs as
// them, reads their files, and answers on their loopback only.
type commandAppInstall struct {
	dryRun bool
	svc    advancedAppServices
	out    textOutput
}

func (c *commandAppInstall) setup(svc advancedAppServices, parent commandParent) {
	cmd := parent.Command("install", "Install the app as a service that starts at login. Needs credential persistence (do not pass --no-persist-credentials).")
	cmd.Flag("dry-run", "Print what would be written and run").BoolVar(&c.dryRun)
	c.svc = svc
	c.out.setup(svc)
	cmd.Action(svc.noRepositoryAction(c.run))
}

func (c *commandAppInstall) run(ctx context.Context) error {
	// Fail before writing a unit that could never open its repository at boot.
	if _, err := appPersist(c.svc); err != nil {
		return err
	}

	bin, err := os.Executable()
	if err != nil {
		return err
	}

	p, err := install.SystemdApp(bin)
	if err != nil {
		return err
	}

	if c.dryRun {
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

	c.out.printStdout("Open the app with:\n    %s app url\n", bin)

	return nil
}
