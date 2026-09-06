package cli

import (
	"context"
	"os"
	"time"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/agent/engine"
	"github.com/kopia/kopia/agent/state"
	"github.com/kopia/kopia/internal/apiclient"
	"github.com/kopia/kopia/internal/clock"
	"github.com/kopia/kopia/internal/serverapi"
)

// engineDownExitCode is what 'agent status' exits with when there is no
// engine to ask: distinct from 1 (a real failure) so a monitoring check can
// tell "the agent is not running" from "the status command broke".
const engineDownExitCode = 2

// commandAgentStatus reports what the locally running agent engine is doing,
// by reading engine.json and querying the engine's own API.
type commandAgentStatus struct {
	scope string
	svc   advancedAppServices
	out   textOutput
}

func (c *commandAgentStatus) setup(svc advancedAppServices, parent commandParent) {
	cmd := parent.Command("status", "Show what the running agent engine is backing up.")
	cmd.Flag("scope", "user or system").Default("user").EnumVar(&c.scope, "user", "system")
	c.svc = svc
	c.out.setup(svc)
	cmd.Action(svc.noRepositoryAction(c.run))
}

func (c *commandAgentStatus) run(ctx context.Context) error {
	info, err := engine.ReadInfo(c.scope)
	if err != nil {
		// Only a missing engine.json means "no engine". A corrupt or
		// unreadable one is a real failure and must not be reported as a
		// stopped agent.
		if os.IsNotExist(err) {
			return c.engineDown(errors.Errorf("the %s engine is not running; start it with %s", c.noun(), c.startHint()))
		}

		return errors.Wrap(err, "unable to read engine.json")
	}

	api, err := apiclient.NewKopiaAPIClient(apiclient.Options{BaseURL: info.BaseURL, Username: info.User, Password: info.Password})
	if err != nil {
		return errors.Wrap(err, "bad engine address in engine.json")
	}

	var sr serverapi.SourcesResponse

	if err := api.Get(ctx, "sources", nil, &sr); err != nil {
		// engine.json points at a loopback port; nothing answering there
		// means the process that wrote it is gone.
		return c.engineDown(errors.Wrapf(err, "the %s engine is not reachable", c.noun()))
	}

	if len(sr.Sources) == 0 {
		c.out.printStdout("no sources configured\n")

		return nil
	}

	for _, s := range sr.Sources {
		c.out.printStdout("%v\t%v\tlast %v\tnext %v\n", s.Source.Path, s.Status, lastSnapshotAge(s), nextSnapshotTime(s))
	}

	return nil
}

// noun and startHint name what the caller is actually looking at: 'app
// status' shares this command's body, and telling someone with no Fleet
// server to run 'agent run' sends them to a command that will refuse.
func (c *commandAgentStatus) noun() string {
	if c.scope == state.ScopeApp {
		return "app"
	}

	return "agent"
}

func (c *commandAgentStatus) startHint() string {
	if c.scope == state.ScopeApp {
		return "'warphold app install' (or 'warphold app run' to run it in this terminal)"
	}

	return "'warphold agent run'"
}

// engineDown prints why and exits 2. It returns an error only so callers can
// 'return c.engineDown(...)' and keep the control flow obvious.
func (c *commandAgentStatus) engineDown(err error) error {
	c.out.printStderr("%v\n", err)
	os.Exit(engineDownExitCode)

	return err
}

func lastSnapshotAge(s *serverapi.SourceStatus) string {
	if s.LastSnapshot == nil {
		return "never"
	}

	return clock.Now().Sub(s.LastSnapshot.StartTime.ToTime()).Truncate(time.Second).String() + " ago"
}

func nextSnapshotTime(s *serverapi.SourceStatus) string {
	if s.NextSnapshotTime == nil {
		return "-"
	}

	return s.NextSnapshotTime.Local().Format(time.RFC3339)
}
