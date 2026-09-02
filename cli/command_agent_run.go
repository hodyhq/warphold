package cli

import (
	"context"
	"os"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/agent/engine"
	"github.com/kopia/kopia/agent/poll"
	"github.com/kopia/kopia/agent/run"
	"github.com/kopia/kopia/agent/state"
)

// commandAgentRun runs the agent's poll/snapshot loop: it loads the agent's
// enrollment state, opens its repository headlessly, and drives run.Loop
// against the Fleet server until revoked or (with --once) for a single cycle.
type commandAgentRun struct {
	scope string
	once  bool
	svc   advancedAppServices
	out   textOutput
}

func (c *commandAgentRun) setup(svc advancedAppServices, parent commandParent) {
	cmd := parent.Command("run", "Run the agent: back up on schedule and report to the Fleet server.")
	cmd.Flag("scope", "user or system").Default("user").EnumVar(&c.scope, "user", "system")
	cmd.Flag("once", "Poll once, report once, exit (for tests and cron).").Hidden().BoolVar(&c.once)
	c.svc = svc
	c.out.setup(svc)
	cmd.Action(svc.noRepositoryAction(c.run))
}

func (c *commandAgentRun) run(ctx context.Context) error {
	st, err := state.Load(c.scope)
	if err != nil {
		return errors.Wrap(err, "not enrolled; run 'warphold agent enroll' first")
	}

	cfg := state.RepoConfigPath(c.scope)

	password, err := c.svc.passwordPersistenceStrategy().GetPassword(ctx, cfg)
	if err != nil {
		return errors.Wrap(err, "repository password not found; re-enroll")
	}

	h, err := engine.StartHeadless(ctx, cfg, password, state.Dir(c.scope))
	if err != nil {
		return err
	}
	defer h.Stop(context.WithoutCancel(ctx)) //nolint:errcheck

	api, err := h.Client()
	if err != nil {
		return err
	}

	local, err := engine.NewLocal(api)
	if err != nil {
		return err
	}

	loop := run.New(run.Deps{Fleet: &poll.Client{Server: st.Server, Bearer: st.Bearer}, Local: local, State: st, Log: log(ctx).Warnf})

	err = loop.Run(ctx, c.once)
	if errors.Is(err, poll.ErrRevoked) {
		log(ctx).Error("this agent was revoked by the Fleet server; not restarting")
		os.Exit(3) //nolint:forbidigo
	}

	return err
}
