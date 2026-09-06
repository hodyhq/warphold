package cli

import (
	"context"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/agent/engine"
	"github.com/kopia/kopia/agent/state"
	"github.com/kopia/kopia/internal/passwordpersist"
)

// commandAppRun runs the standalone local engine: Kopia's scheduler, uploads
// and tasks plus the WarpHold UI, on loopback, over the app's own repository.
//
// Unlike 'agent run' it talks to no Fleet server and needs no enrollment, and
// unlike 'server start' it writes engine.json, so the tray and 'app url' can
// find it. The first run has no repository at all: the engine comes up
// unconfigured and the UI's setup wizard creates one.
type commandAppRun struct {
	svc advancedAppServices
	out textOutput
}

func (c *commandAppRun) setup(svc advancedAppServices, parent commandParent) {
	cmd := parent.Command("run", "Run this machine's backup engine and serve the app on loopback. Needs credential persistence (do not pass --no-persist-credentials).")
	c.svc = svc
	c.out.setup(svc)
	cmd.Action(svc.noRepositoryAction(c.run))
}

func (c *commandAppRun) run(ctx context.Context) error {
	cfg := state.RepoConfigPath(state.ScopeApp)

	persist, err := appPersist(c.svc)
	if err != nil {
		return err
	}

	// No password yet is the first run, not a failure. Anything else - an
	// unreadable password file - is: starting with an empty password would
	// look to the user like the repository had gone missing.
	password, err := persist.GetPassword(ctx, cfg)
	if err != nil && !errors.Is(err, passwordpersist.ErrPasswordNotFound) {
		return errors.Wrap(err, "unable to read the repository password")
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// SIGTERM (systemctl stop, a reboot) must run Stop: it is what removes
	// engine.json, and a stale one points the tray at a dead port.
	c.svc.onTerminate(cancel)

	h, err := engine.StartHeadless(ctx, cfg, password, state.ScopeApp, persist)
	if err != nil {
		return err
	}

	c.out.printStdout("WarpHold is listening on %v\n", h.BaseURL)

	<-ctx.Done()

	return h.Stop(context.WithoutCancel(ctx))
}
