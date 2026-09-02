package cli

import (
	"sync"

	"github.com/gorilla/mux"

	"github.com/kopia/kopia/fleet"
	"github.com/kopia/kopia/fleet/api"
	"github.com/kopia/kopia/internal/server"
)

// commandFleet groups the Fleet control-plane commands.
type commandFleet struct {
	activate commandFleetActivate
}

// registerFleetHandlersOnce guards RegisterServerHandlers: the in-process
// test runner calls App.setup (and so commandFleet.setup) once per CLI
// invocation within the same process, so without this every `server start`
// would run one more accumulated mount closure than the last.
var registerFleetHandlersOnce sync.Once

func (c *commandFleet) setup(svc advancedAppServices, parent commandParent) {
	cmd := parent.Command("fleet", "WarpHold Fleet: manage enrolled machines.")
	c.activate.setup(svc, cmd)

	registerFleetHandlersOnce.Do(func() {
		RegisterServerHandlers(func(_ *server.Server, m *mux.Router, configFile string) {
			api.New(fleet.StateDirFor(configFile)).Mount(m)
		})
	})
}
