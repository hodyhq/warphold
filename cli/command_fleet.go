package cli

import (
	"context"
	"errors"
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
		RegisterServerHandlers(func(srv *server.Server, m *mux.Router, configFile string) {
			fs := api.New(fleet.StateDirFor(configFile))
			fs.Mount(m)

			// The closure runs once per `server start`, and the in-process
			// test runner starts several servers in the same process, so the
			// api.Server built here must hand its Fleet state DB back when
			// this server shuts down. command_server_start.go has already
			// installed its own OnShutdown by now, so chain rather than
			// replace it, and close the DB only after it has drained the
			// in-flight requests that are still using it.
			prev := srv.OnShutdown
			srv.OnShutdown = func(ctx context.Context) error {
				var err error
				if prev != nil {
					err = prev(ctx)
				}

				return errors.Join(err, fs.Close())
			}
		})
	})
}
