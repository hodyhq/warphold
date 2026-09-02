package cli

import (
	"context"

	"github.com/kopia/kopia/agent/tray"
)

// commandAgentTray runs the system-tray companion: it polls the engine this
// machine's agent runs and shows its state in the desktop's notification
// area.
type commandAgentTray struct {
	scope string
	svc   advancedAppServices
}

func (c *commandAgentTray) setup(svc advancedAppServices, parent commandParent) {
	cmd := parent.Command("tray", "Show backup status in the system tray (Linux).")
	cmd.Flag("scope", "user or system").Default("user").EnumVar(&c.scope, "user", "system")
	c.svc = svc
	cmd.Action(svc.noRepositoryAction(c.run))
}

func (c *commandAgentTray) run(ctx context.Context) error {
	return tray.Run(ctx, tray.Options{Scope: c.scope})
}
