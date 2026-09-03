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

	s := api.New(fleet.StateDirFor(c.svc.repositoryConfigFileName()))
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
