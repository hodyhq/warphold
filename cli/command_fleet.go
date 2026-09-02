package cli

import (
	"github.com/gorilla/mux"

	"github.com/kopia/kopia/fleet"
	"github.com/kopia/kopia/fleet/api"
	"github.com/kopia/kopia/internal/server"
)

// commandFleet groups the Fleet control-plane commands.
type commandFleet struct {
	activate commandFleetActivate
}

func (c *commandFleet) setup(svc advancedAppServices, parent commandParent) {
	cmd := parent.Command("fleet", "WarpHold Fleet: manage enrolled machines.")
	c.activate.setup(svc, cmd)

	RegisterServerHandlers(func(_ *server.Server, m *mux.Router, configFile string) {
		api.New(fleet.StateDirFor(configFile)).Mount(m)
	})
}
