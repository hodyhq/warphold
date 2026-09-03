package cli

import (
	"context"
	"os"

	"github.com/kopia/kopia/agent/engine"
	"github.com/kopia/kopia/agent/state"
	"github.com/kopia/kopia/internal/apiclient"
	"github.com/kopia/kopia/internal/serverapi"
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

// commandAppStatus reports what the standalone app's engine is backing up. It
// is 'agent status' pointed at the app's state directory: the same
// engine.json, the same API and the same exit codes, so one monitoring check
// covers both.
type commandAppStatus struct {
	svc advancedAppServices
	out textOutput
}

func (c *commandAppStatus) setup(svc advancedAppServices, parent commandParent) {
	cmd := parent.Command("status", "Show what the running app engine is backing up.")
	c.svc = svc
	c.out.setup(svc)
	cmd.Action(svc.noRepositoryAction(c.run))
}

func (c *commandAppStatus) run(ctx context.Context) error {
	// Before the source list: a running engine with no repository yet is the
	// first-run state, not a broken one, and "not reachable" (which is what
	// the source listing reports for it) would send the user looking for a
	// service that is in fact up. Every error here is ignored on purpose -
	// the agent-status path below reports them properly.
	if info, err := engine.ReadInfo(state.ScopeApp); err == nil {
		if api, err := apiclient.NewKopiaAPIClient(apiclient.Options{BaseURL: info.BaseURL, Username: info.User, Password: info.Password}); err == nil {
			var st serverapi.StatusResponse

			if err := api.Get(ctx, "repo/status", nil, &st); err == nil && !st.Connected {
				c.out.printStdout("no backups configured yet - open the app to set one up:\n    warphold app url\n")

				return nil
			}
		}
	}

	s := commandAgentStatus{scope: state.ScopeApp, svc: c.svc, out: c.out}

	return s.run(ctx)
}

// commandAppURL prints the one URL that opens the running app in a browser.
//
// That URL carries the engine's local session token, which is why this is a
// command a person runs rather than something printed on every start: it
// belongs in a browser, not in a log or a chat window. The token is minted per
// engine process and dies with it, and any process that could read this
// command's output could read engine.json - which holds the same token, plus
// the engine's password - just as easily.
type commandAppURL struct {
	svc advancedAppServices
	out textOutput
}

func (c *commandAppURL) setup(svc advancedAppServices, parent commandParent) {
	cmd := parent.Command("url", "Print the local URL that opens the app in a browser.")
	c.svc = svc
	c.out.setup(svc)
	cmd.Action(svc.noRepositoryAction(c.run))
}

func (c *commandAppURL) run(_ context.Context) error {
	u, err := engine.SessionURL(state.ScopeApp)
	if err != nil {
		// Same exit code as 'agent status': "not running" is not "broke".
		c.out.printStderr("%v\n", err)
		os.Exit(engineDownExitCode) //nolint:forbidigo
	}

	c.out.printStdout("%v\n", u)

	return nil
}
