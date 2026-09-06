package cli

import (
	"context"

	"github.com/kopia/kopia/agent/engine"
	"github.com/kopia/kopia/agent/state"
	"github.com/kopia/kopia/internal/apiclient"
	"github.com/kopia/kopia/internal/serverapi"
)

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
