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
	rotate   commandFleetRotatePassphrase
	jobs     commandFleetJobs
}

// registerFleetHandlersOnce guards RegisterServerHandlers: the in-process
// test runner calls App.setup (and so commandFleet.setup) once per CLI
// invocation within the same process, so without this every `server start`
// would run one more accumulated mount closure than the last.
var registerFleetHandlersOnce sync.Once

func (c *commandFleet) setup(svc advancedAppServices, parent commandParent) {
	cmd := parent.Command("fleet", "WarpHold Fleet: manage enrolled machines.")
	c.activate.setup(svc, cmd)
	c.rotate.setup(svc, cmd)
	c.jobs.setup(svc, cmd)

	registerFleetHandlersOnce.Do(func() {
		RegisterServerHandlers(func(ctx context.Context, srv *server.Server, m *mux.Router, configFile string) error {
			stateDir := fleet.StateDirFor(configFile)
			fs := api.New(stateDir)

			// State that cannot be used safely is fatal here, not a warning:
			// serving Fleet on a key that may no longer open its own store
			// would seal every new secret into a store nothing can read back.
			if err := fs.StateError(); err != nil {
				fs.Close() //nolint:errcheck

				return errors.Join(errors.New("fleet state in "+stateDir+" cannot be used"), err)
			}

			// Held for as long as this server serves. Failing to take it is
			// fatal either way, and ErrLocked most of all: one Fleet process
			// per state directory is a real invariant, not a nicety. Two
			// servers on one state dir each run a scheduler, and both would
			// maintain the same device repositories through their own scratch
			// configs -- Kopia's .mlock is keyed to the config file, so it
			// would not keep them apart (see fleet/jobs/repo.go). It is also
			// what lets the offline `fleet rotate-passphrase` tell a running
			// Fleet from a stopped one. Warning and serving anyway made the
			// lock a one-way signal and left both claims untrue.
			lock, lockErr := fleet.TryLock(stateDir)
			if lockErr != nil {
				fs.Close() //nolint:errcheck

				if errors.Is(lockErr, fleet.ErrLocked) {
					return errors.New("another WarpHold Fleet is already serving " + stateDir +
						" (it holds " + fleet.PathsFor(stateDir).LockFile +
						"); stop it before starting this one")
				}

				return errors.Join(errors.New("cannot hold "+fleet.PathsFor(stateDir).LockFile), lockErr)
			}

			fs.Mount(m)

			// Setup gives the Fleet host a repository of its own; this opens
			// it (and says so), rather than leaving a fresh Fleet server
			// reporting "Repository not configured". The same work runs again
			// if the installer activates this Fleet while the server is
			// already up, which is the one-command install's normal order.
			serveFleetRepo(ctx, srv, fs, configFile)
			fs.OnActivated(func() { serveFleetRepo(ctx, srv, fs, configFile) })

			// This hook is the one place that runs before setupHandlers
			// registers the UI's "/" catch-all, so the SPA bundle is served
			// here - unauthenticated, and refresh-safe on WarpHold's own
			// client-side routes - rather than from upstream's UI-auth-gated
			// file server and its isKnownUIRoute allowlist.
			//
			// The bundle is always the embedded one: this hook cannot see
			// `server start --html-path`, which only redirects upstream's
			// (still registered, still authenticated) file server.
			srv.ServeSPAPublic(m, server.AssetFile())

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

				err = errors.Join(err, lock.Unlock())

				return errors.Join(err, fs.Close())
			}

			return nil
		})
	})
}
