package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/agent/install"
	"github.com/kopia/kopia/agent/state"
)

// commandAppUninstall removes the service and the tray entry. It removes no
// data: the repository configuration, the password and the cache stay where
// they are, so a re-install picks the same backups up again.
type commandAppUninstall struct {
	svc advancedAppServices
	out textOutput
}

func (c *commandAppUninstall) setup(svc advancedAppServices, parent commandParent) {
	cmd := parent.Command("uninstall", "Remove the app service and tray entry. Backups and settings are kept.")
	c.svc = svc
	c.out.setup(svc)
	cmd.Action(svc.noRepositoryAction(c.run))
}

func (c *commandAppUninstall) run(ctx context.Context) error {
	cfg, err := install.UserConfigDir()
	if err != nil {
		return err
	}

	// Stop and disable before the unit file goes away: systemd cannot stop a
	// unit whose file it can no longer read.
	c.systemctl(ctx, "--user", "disable", "--now", install.AppUnitName)

	// Only the app's own tray entry: an agent install has its own file, and
	// on a machine that is both, taking that one away would leave the agent
	// with no status icon and no obvious reason why.
	for _, path := range []string{install.AppUnitPath(cfg), install.AppAutostartPath(cfg)} {
		removed, err := removeIfPresent(path)
		if err != nil {
			return err
		}

		if removed {
			c.out.printStdout("- removed %s\n", path)
		}
	}

	c.systemctl(ctx, "--user", "daemon-reload")

	c.out.printStdout("Your backups and settings are untouched in %s\n", state.Dir(state.ScopeApp))

	return nil
}

// systemctl runs a best-effort systemd command: an uninstall must finish
// removing the files whether or not there is a session bus to talk to (a
// remote shell, a container, a user who is not logged in).
func (c *commandAppUninstall) systemctl(ctx context.Context, args ...string) {
	cmd := exec.CommandContext(ctx, "systemctl", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		c.out.printStderr("systemctl %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// removeIfPresent deletes a file that may already be gone, and reports
// whether it was there, so an uninstall claims only what it actually did.
func removeIfPresent(path string) (bool, error) {
	err := os.Remove(path)

	switch {
	case err == nil:
		return true, nil
	case os.IsNotExist(err):
		return false, nil
	default:
		return false, errors.Wrapf(err, "unable to remove %s", filepath.Clean(path))
	}
}
