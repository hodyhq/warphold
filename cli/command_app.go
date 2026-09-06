package cli

import (
	"github.com/pkg/errors"

	"github.com/kopia/kopia/internal/passwordpersist"
)

// commandApp groups the standalone single-machine commands: this machine
// backing itself up, with no Fleet server anywhere. It is the same engine the
// agent runs, over its own repository in its own state directory
// (state.ScopeApp), so a machine can be a Fleet device and still run the
// standalone app without the two sharing a repository, a port or a password.
type commandApp struct {
	run       commandAppRun
	install   commandAppInstall
	uninstall commandAppUninstall
	status    commandAppStatus
	url       commandAppURL
}

func (c *commandApp) setup(svc advancedAppServices, parent commandParent) {
	cmd := parent.Command("app", "WarpHold app: back up this machine on its own, with no Fleet server.")
	c.run.setup(svc, cmd)
	c.install.setup(svc, cmd)
	c.uninstall.setup(svc, cmd)
	c.status.setup(svc, cmd)
	c.url.setup(svc, cmd)
}

// appPersist is where the app's repository password is stored. Its repository
// is created by the UI's setup wizard, so unlike an agent's there is no
// enrollment to have saved the password already - and the service has to
// reopen that repository unattended at every boot, which it cannot do if the
// password was never written down.
//
// The strategy itself is the CLI's own (keyring then file, or file alone), so
// the app stores its password exactly where every other Kopia command on this
// machine does.
func appPersist(svc advancedAppServices) (passwordpersist.Strategy, error) {
	p := svc.passwordPersistenceStrategy()
	if p == passwordpersist.None() {
		return nil, errors.New("the app must store its repository password to restart unattended: re-run without --no-persist-credentials (or KOPIA_PERSIST_CREDENTIALS_ON_CONNECT=false)")
	}

	return p, nil
}
