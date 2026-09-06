package jobs

import (
	"context"
	"errors"
	"fmt"

	"github.com/kopia/kopia/fleet/seal"
	"github.com/kopia/kopia/fleet/store"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/maintenance"
	"github.com/kopia/kopia/snapshot/snapshotmaintenance"
)

// Maintenance returns the runner for the "maintenance" job: a full maintenance
// cycle on every (or one) agent's repository, daily (spec §3.3). Agents run
// with --no-auto-maintenance, so this is the only process that maintains a
// device repository - it happens here, exactly once per repository, and this
// is the only job that opens one for writing.
func Maintenance(st *store.Store, open seal.Opener, cloud CloudStoreFn) Runner {
	return perAgent(st, open, cloud, "maintained", false, maintainRepo)
}

func maintainRepo(ctx context.Context, rep repo.Repository, _ store.Agent) error {
	dr, ok := rep.(repo.DirectRepository)
	if !ok {
		return errors.New("maintenance needs a direct repository connection")
	}

	// The connection was made as the Fleet, so this is the owner string the
	// repository has to carry for maintenance to be ours to run.
	me := dr.ClientOptions().UsernameAtHost()

	p, err := maintenance.GetParams(ctx, dr)
	if err != nil {
		return fmt.Errorf("reading the maintenance parameters: %w", err)
	}

	if p.Owner != me {
		// Read before the closure runs: it sets p.Owner = me, so reporting
		// p.Owner afterwards would print the NEW owner as the old one and both
		// messages would lose the one fact they exist to record.
		prev := p.Owner

		// Provisioning stamps the Fleet as the owner, but the string embeds a
		// hostname, and a Fleet that moved (or a repository provisioned before
		// public_url was set) would otherwise never be maintained again. Taking
		// it back is safe because no agent ever maintains its own repository.
		if err := repo.DirectWriteSession(ctx, dr, repo.WriteSessionOptions{Purpose: "fleet-maintenance-owner"},
			func(ctx context.Context, w repo.DirectRepositoryWriter) error {
				p.Owner = me

				return maintenance.SetParams(ctx, w, p)
			}); err != nil {
			return fmt.Errorf("taking maintenance ownership from %q: %w", prev, err)
		}

		logf("maintenance owner of a device repository moved from %q to %q", prev, me)
	}

	return repo.DirectWriteSession(ctx, dr, repo.WriteSessionOptions{Purpose: "fleet-maintenance"},
		func(ctx context.Context, w repo.DirectRepositoryWriter) error {
			// force is false: the owner check above is the safety, and it is
			// now true rather than bypassed.
			return snapshotmaintenance.Run(ctx, w, maintenance.ModeFull, false, maintenance.SafetyFull)
		})
}
