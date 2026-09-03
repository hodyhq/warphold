package cli

import (
	"context"
	"os"
	"os/user"
	"path/filepath"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/fleet"
	"github.com/kopia/kopia/fleet/api"
	"github.com/kopia/kopia/internal/server"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob/filesystem"
	"github.com/kopia/kopia/repo/content"
)

const (
	// fleetRepoDirName is the Fleet host's own repository, inside the data
	// directory. The Fleet server is a machine that needs backing up like any
	// other, and without this a fresh install starts with "Repository not
	// configured" and nowhere to put its own snapshots.
	fleetRepoDirName = "fleet-repo"

	// fleetRepoConfigName and fleetRepoCacheName live in the Fleet state
	// directory. The Fleet host's repository deliberately does not use the
	// installation's own --config-file: that one belongs to whoever runs the
	// CLI, its password comes from KOPIA_PASSWORD or the persisted credential,
	// and overwriting it would repoint an operator's repository.
	fleetRepoConfigName = "fleet-repo.config"
	fleetRepoCacheName  = "fleet-repo-cache"

	// fleetServiceUser and fleetDataRoot are what scripts/install/fleet.sh
	// creates: the service account and the 0700 data root it owns.
	fleetServiceUser = "warphold"
	fleetDataRoot    = "/srv/warphold"
)

// fleetDataDir is where this host keeps Fleet data: /srv/warphold on a
// packaged install - it exists only because scripts/install/fleet.sh created
// it - and a directory next to the repository config file everywhere else,
// because writing to /srv from a `--config-file /tmp/...` run would be a
// surprise. The installer runs setup as root, so root counts as an owner of
// the data root alongside the service user itself.
func fleetDataDir(configFile string) string {
	if fi, err := os.Stat(fleetDataRoot); err == nil && fi.IsDir() {
		if u, err := user.Current(); err == nil && (u.Username == fleetServiceUser || u.Uid == "0") {
			return fleetDataRoot
		}
	}

	return filepath.Join(filepath.Dir(configFile), "data")
}

// ensureFleetRepo gives the Fleet host a local Kopia repository of its own and
// returns its directory and the config file that connects to it. Every step is
// idempotent - setup runs it once, every `server start` runs it again - and the
// repository password is random and sealed in the Fleet DB, so nothing has to
// prompt for it and nothing but this Fleet can read it.
func ensureFleetRepo(ctx context.Context, fs *api.Server, configFile, dataDir string) (repoDir, repoConfig string, err error) {
	if dataDir == "" {
		dataDir = fleetDataDir(configFile)
	}

	stateDir := fleet.StateDirFor(configFile)

	repoDir, err = fs.FleetRepoPath(ctx, filepath.Join(dataDir, fleetRepoDirName))
	if err != nil {
		return "", "", errors.Wrap(err, "fleet repository path")
	}

	password, err := fs.FleetRepoPassword(ctx)
	if err != nil {
		return "", "", errors.Wrap(err, "fleet repository password")
	}

	if err := os.MkdirAll(repoDir, 0o700); err != nil {
		return "", "", errors.Wrap(err, "create fleet repository directory")
	}

	st, err := filesystem.New(ctx, &filesystem.Options{Path: repoDir}, true)
	if err != nil {
		return "", "", errors.Wrap(err, "open fleet repository storage")
	}

	defer st.Close(ctx) //nolint:errcheck

	if err := repo.Initialize(ctx, st, nil, password); err != nil && !errors.Is(err, repo.ErrAlreadyInitialized) {
		return "", "", errors.Wrap(err, "initialize fleet repository")
	}

	repoConfig = filepath.Join(stateDir, fleetRepoConfigName)
	if _, err := os.Stat(repoConfig); err == nil {
		return repoDir, repoConfig, nil
	}

	if err := repo.Connect(ctx, repoConfig, st, password, &repo.ConnectOptions{
		CachingOptions: content.CachingOptions{CacheDirectory: filepath.Join(stateDir, fleetRepoCacheName)},
	}); err != nil {
		return "", "", errors.Wrap(err, "connect to the fleet repository")
	}

	return repoDir, repoConfig, nil
}

// serveFleetRepo runs at `server start`, from the Fleet hook: it makes the
// Fleet host's own repository the one this server serves, unless the operator
// already connected this installation to a repository of their choosing. It
// never fails the server - a Fleet server with no repository of its own still
// runs every other machine's backups - but it always says which case it is in,
// because "Repository not configured" with no further explanation is what this
// replaces.
func serveFleetRepo(ctx context.Context, srv *server.Server, fs *api.Server, configFile string) {
	if !fs.Activated() {
		log(ctx).Info("WarpHold Fleet: not activated, so this host has no repository of its own yet.")
		return
	}

	repoDir, repoConfig, err := ensureFleetRepo(ctx, fs, configFile, "")
	if err != nil {
		log(ctx).Warnf("WarpHold Fleet: this host has no usable repository of its own: %v", err)
		return
	}

	if _, err := os.Stat(configFile); err == nil {
		log(ctx).Infof("WarpHold Fleet: this host's own repository is %v; serving the repository this installation is connected to instead.", repoDir)
		return
	}

	password, err := fs.FleetRepoPassword(ctx)
	if err != nil {
		log(ctx).Warnf("WarpHold Fleet: cannot unseal this host's repository password: %v", err)
		return
	}

	if _, err := srv.InitRepositoryAsync(ctx, "Fleet", func(ctx context.Context) (repo.Repository, error) {
		return repo.Open(ctx, repoConfig, password, nil) //nolint:wrapcheck
	}, true); err != nil {
		log(ctx).Warnf("WarpHold Fleet: cannot open this host's own repository in %v: %v", repoDir, err)
		return
	}

	log(ctx).Infof("WarpHold Fleet: serving this host's own repository from %v.", repoDir)
}
