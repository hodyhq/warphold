package cli

import (
	"context"
	"fmt"
	"sort"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/fleet"
	"github.com/kopia/kopia/fleet/api"
)

type commandFleetRotatePassphrase struct {
	current string
	next    string
	dryRun  bool
	svc     appServices
	out     textOutput
}

func (c *commandFleetRotatePassphrase) setup(svc appServices, parent commandParent) {
	cmd := parent.Command("rotate-passphrase",
		"Re-seal every stored secret under a new sealing passphrase. Run it with the Fleet server stopped; while it is running, rotate through the UI or POST /api/v1/fleet/settings/passphrase instead.")
	cmd.Flag("passphrase", "Current sealing passphrase; prompted if omitted").Envar(svc.EnvName("WARPHOLD_SEAL_PASSPHRASE")).StringVar(&c.current)
	cmd.Flag("new-passphrase", "New sealing passphrase (12+ chars); prompted twice if omitted").Envar(svc.EnvName("WARPHOLD_SEAL_PASSPHRASE_NEW")).StringVar(&c.next)
	cmd.Flag("dry-run", "Report what would be re-sealed and write nothing").BoolVar(&c.dryRun)
	c.svc = svc
	c.out.setup(svc)
	cmd.Action(svc.noRepositoryAction(c.run))
}

func (c *commandFleetRotatePassphrase) run(ctx context.Context) error {
	if c.current == "" {
		p, err := askPass(c.out.stdout(), "Current sealing passphrase: ")
		if err != nil {
			return err
		}
		c.current = p
	}

	if c.next == "" {
		p, err := askPass(c.out.stdout(), "New sealing passphrase: ")
		if err != nil {
			return err
		}
		// Asked twice for the same reason activation asks twice: this
		// passphrase is the only way back to every escrowed secret, and a typo
		// is only discovered the next time something has to be unsealed.
		again, err := askPass(c.out.stdout(), "Confirm new sealing passphrase: ")
		if err != nil {
			return err
		}

		if p != again {
			return errors.New("passphrases do not match")
		}

		c.next = p
	}

	stateDir := fleet.StateDirFor(c.svc.repositoryConfigFileName())

	// A running Fleet server holds this lock. Without the check the rotation
	// would succeed - WAL and the busy timeout let both processes write - and
	// the running one would keep sealing new secrets with the key this command
	// just replaced, leaving a store sealed under two keys.
	lock, err := fleet.TryLock(stateDir)
	if err != nil {
		if errors.Is(err, fleet.ErrLocked) {
			return errors.New("the Fleet server is running (" + fleet.PathsFor(stateDir).LockFile +
				" is held). Stop it first, or rotate through the UI, which does it live.")
		}

		return errors.Wrap(err, "locking the fleet state directory")
	}

	defer lock.Unlock() //nolint:errcheck

	// NewOffline, not New: this runs under the state-dir lock that says the
	// Fleet server is stopped, and a scheduler started here for the length of
	// this command would race the rotation over the very rows it re-seals.
	s := api.NewOffline(stateDir)
	defer s.Close() //nolint:errcheck

	counts, err := s.RotatePassphrase(ctx, c.current, c.next, c.dryRun)
	if err != nil {
		return errors.Wrap(err, "rotate sealing passphrase")
	}

	what := "Re-sealed"
	if c.dryRun {
		what = "Would re-seal"
	}

	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	for _, k := range keys {
		fmt.Fprintf(c.out.stdout(), "%s %d %s\n", what, counts[k], k) //nolint:errcheck
	}

	if c.dryRun {
		fmt.Fprintln(c.out.stdout(), "Dry run: nothing was written.") //nolint:errcheck
		return nil
	}

	fmt.Fprintln(c.out.stdout(), "The sealing passphrase is rotated. Keep the new one with the recovery kit.") //nolint:errcheck

	return nil
}
