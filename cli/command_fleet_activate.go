package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/fleet"
	"github.com/kopia/kopia/fleet/api"
)

type commandFleetActivate struct {
	email      string
	password   string
	passphrase string
	publicURL  string
	verifyURL  bool
	dataDir    string
	svc        appServices
	out        textOutput
}

func (c *commandFleetActivate) setup(svc appServices, parent commandParent) {
	cmd := parent.Command("activate", "Turn this WarpHold into a Fleet server (creates the state DB and the first admin).")
	// Every secret here reads from the environment as well as from a flag, so
	// the one-command install can activate without putting a password or a
	// passphrase in argv, where "ps" shows it to every user on the host.
	// Required() is still satisfied by an environment value: kingpin's
	// needsValue() treats an envar value as provided.
	cmd.Flag("email", "First admin email").Required().Envar(svc.EnvName("WARPHOLD_SETUP_EMAIL")).StringVar(&c.email)
	// kingpin binds one environment variable per flag, so the installer's
	// WARPHOLD_SETUP_* spellings are read in run() and named here instead, to
	// keep them in --help.
	cmd.Flag("admin-password", "First admin password (8+ chars); or WARPHOLD_ADMIN_PASSWORD, or WARPHOLD_SETUP_PASSWORD").Envar(svc.EnvName("WARPHOLD_ADMIN_PASSWORD")).StringVar(&c.password)
	cmd.Flag("passphrase", "Sealing passphrase (8+ chars), prompted if omitted; or WARPHOLD_SEAL_PASSPHRASE, or WARPHOLD_SETUP_PASSPHRASE").Envar(svc.EnvName("WARPHOLD_SEAL_PASSPHRASE")).StringVar(&c.passphrase)
	cmd.Flag("public-url", "Public URL devices and browsers reach this Fleet on, e.g. https://fleet.example.com").Envar(svc.EnvName("WARPHOLD_SETUP_PUBLIC_URL")).StringVar(&c.publicURL)
	cmd.Flag("verify-public-url", "Fetch --public-url end to end before finishing and fail if it does not answer as this Fleet").BoolVar(&c.verifyURL)
	cmd.Flag("data-dir", "Directory for this host's own Fleet data; its repository is created in <data-dir>/"+fleetRepoDirName).StringVar(&c.dataDir)
	c.svc = svc
	c.out.setup(svc)
	cmd.Action(svc.noRepositoryAction(c.run))
}

// setupEnvAliases fills in the secrets from the WARPHOLD_SETUP_* names that
// scripts/install/fleet.sh exports, so the installer can hand its own
// environment straight to this command. A flag - or the older
// WARPHOLD_ADMIN_PASSWORD / WARPHOLD_SEAL_PASSPHRASE names, which stay
// supported - wins, because kingpin has already filled the field in by now.
func (c *commandFleetActivate) setupEnvAliases() {
	if c.password == "" {
		c.password = os.Getenv(c.svc.EnvName("WARPHOLD_SETUP_PASSWORD"))
	}

	if c.passphrase == "" {
		c.passphrase = os.Getenv(c.svc.EnvName("WARPHOLD_SETUP_PASSPHRASE"))
	}
}

func (c *commandFleetActivate) run(ctx context.Context) error {
	c.setupEnvAliases()

	// The URL is checked before the first prompt and long before the first
	// write: activation happens once, and finishing it with a rejected public
	// URL would leave an activated Fleet whose setup step cannot be redone.
	if c.publicURL != "" {
		if err := api.ValidatePublicURL(c.publicURL); err != nil {
			return err
		}
	} else if c.verifyURL {
		return errors.New("--verify-public-url needs --public-url")
	}

	// Same reason: a --data-dir that is relative or symlinked must fail here,
	// not after the Fleet has been activated.
	dataDir, err := resolveDataDir(c.dataDir, c.svc.repositoryConfigFileName())
	if err != nil {
		return err
	}

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
	configFile := c.svc.repositoryConfigFileName()
	s := api.New(fleet.StateDirFor(configFile))
	defer s.Close()
	if err := s.Activate(ctx, c.passphrase, c.email, c.password, c.publicURL); err != nil {
		return errors.Wrap(err, "activate")
	}
	fmt.Fprintln(c.out.stdout(), "Fleet is on.") //nolint:errcheck

	// The Fleet host is a machine that needs backing up too, so setup leaves
	// it with a repository of its own rather than "Repository not configured".
	// A failure here is not fatal: the Fleet is activated and every other
	// machine's backups work, and the next `server start` retries this.
	path, _, err := ensureFleetRepo(ctx, s, configFile, dataDir)
	if err != nil {
		fmt.Fprintf(c.out.stdout(), "Warning: this host has no repository of its own yet: %v\n", err) //nolint:errcheck
	} else {
		fmt.Fprintf(c.out.stdout(), "This host's own repository: %s\n", path) //nolint:errcheck
	}

	// The probe runs after activation, not before: it requires a URL that
	// answers as an *activated* Fleet, which is only true once this call has
	// written the state the running server picks up. A failure therefore
	// leaves an activated Fleet with public_url set to a URL that does not
	// work yet - which is exactly what has to be fixed in the proxy, and the
	// dashboard's Settings page can re-test and change it.
	if c.verifyURL {
		if err := s.VerifyPublicURL(ctx, c.publicURL); err != nil {
			return errors.Wrapf(err, "fleet is activated and %s is stored as its public URL, but nothing there answers as this Fleet; fix the proxy and re-test the URL in Settings (or change it there)", c.publicURL)
		}
		fmt.Fprintf(c.out.stdout(), "Public URL %s answers as this Fleet.\n", c.publicURL) //nolint:errcheck
	} else if c.publicURL != "" {
		fmt.Fprintf(c.out.stdout(), "Public URL: %s\n", c.publicURL) //nolint:errcheck
	}

	fmt.Fprintln(c.out.stdout(), "Start the server with 'warphold server start' and sign in at /api/v1/fleet/session.") //nolint:errcheck

	return nil
}
