package cli

import (
	"context"
	"os"

	"github.com/kopia/kopia/agent/engine"
	"github.com/kopia/kopia/agent/state"
)

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
		os.Exit(engineDownExitCode)
	}

	c.out.printStdout("%v\n", u)

	return nil
}
