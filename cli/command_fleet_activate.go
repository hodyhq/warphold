package cli

import (
	"context"
	"fmt"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/fleet"
	"github.com/kopia/kopia/fleet/api"
)

type commandFleetActivate struct {
	email      string
	password   string
	passphrase string
	svc        appServices
	out        textOutput
}

func (c *commandFleetActivate) setup(svc appServices, parent commandParent) {
	cmd := parent.Command("activate", "Turn this WarpHold into a Fleet server (creates the state DB and the first admin).")
	cmd.Flag("email", "First admin email").Required().StringVar(&c.email)
	cmd.Flag("admin-password", "First admin password (8+ chars)").Envar(svc.EnvName("WARPHOLD_ADMIN_PASSWORD")).StringVar(&c.password)
	cmd.Flag("passphrase", "Sealing passphrase (8+ chars); prompted if omitted").Envar(svc.EnvName("WARPHOLD_SEAL_PASSPHRASE")).StringVar(&c.passphrase)
	c.svc = svc
	c.out.setup(svc)
	cmd.Action(svc.noRepositoryAction(c.run))
}

func (c *commandFleetActivate) run(ctx context.Context) error {
	if c.passphrase == "" {
		p, err := askPass(c.out.stdout(), "Sealing passphrase: ")
		if err != nil {
			return err
		}
		// Activation happens once and this passphrase seals every escrowed
		// secret; a typo here is only discovered when unsealing later fails.
		again, err := askPass(c.out.stdout(), "Confirm sealing passphrase: ")
		if err != nil {
			return err
		}
		if p != again {
			return errors.New("passphrases do not match")
		}
		c.passphrase = p
	}
	if c.password == "" {
		p, err := askPass(c.out.stdout(), "Admin password: ")
		if err != nil {
			return err
		}
		// Same reason as the sealing passphrase: this is the only admin, and
		// a mistyped password is only discovered at the first sign-in.
		again, err := askPass(c.out.stdout(), "Confirm admin password: ")
		if err != nil {
			return err
		}
		if p != again {
			return errors.New("admin passwords do not match")
		}
		c.password = p
	}
	s := api.New(fleet.StateDirFor(c.svc.repositoryConfigFileName()))
	defer s.Close()
	if err := s.Activate(ctx, c.passphrase, c.email, c.password); err != nil {
		return errors.Wrap(err, "activate")
	}
	fmt.Fprintln(c.out.stdout(), "Fleet is on. Start the server with 'warphold server start' and sign in at /api/v1/fleet/session.") //nolint:errcheck
	return nil
}
