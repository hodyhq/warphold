package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/fleet"
	"github.com/kopia/kopia/fleet/jobs"
	"github.com/kopia/kopia/fleet/store"
)

// commandFleetJobs groups the job commands.
type commandFleetJobs struct {
	run commandFleetJobsRun
}

func (c *commandFleetJobs) setup(svc appServices, parent commandParent) {
	cmd := parent.Command("jobs", "Fleet scheduled jobs.")
	c.run.setup(svc, cmd)
}

// commandFleetJobsRun queues a job for the Fleet server to run. It writes the
// row and returns: the server's scheduler claims it on its next tick, so this
// works whether or not the server is up right now.
//
// It deliberately does NOT take the <stateDir>/.lock that `fleet
// rotate-passphrase` takes (fleet/lock.go, Task 26). Enqueueing is one INSERT
// into the jobs table - it opens no repository, unseals nothing, and runs no
// job - so it is safe against a running Fleet, and refusing it while the
// server is up would make the command useless in the only situation it is for.
type commandFleetJobsRun struct {
	kind  string
	agent string
	svc   appServices
	out   textOutput
}

func (c *commandFleetJobsRun) setup(svc appServices, parent commandParent) {
	cmd := parent.Command("run", "Queue a job for the Fleet server to run.")
	cmd.Flag("kind", "Job kind ("+strings.Join(jobs.KindList(), ", ")+")").Required().StringVar(&c.kind)
	cmd.Flag("agent", "Run for one agent instead of the whole fleet").StringVar(&c.agent)
	c.svc = svc
	c.out.setup(svc)
	cmd.Action(svc.noRepositoryAction(c.runCmd))
}

func (c *commandFleetJobsRun) runCmd(ctx context.Context) error {
	if !jobs.HasKind(c.kind) {
		return errors.Errorf("kind must be one of %s", strings.Join(jobs.KindList(), ", "))
	}

	paths := fleet.PathsFor(fleet.StateDirFor(c.svc.repositoryConfigFileName()))

	// store.Open creates the database, which would leave an empty one behind
	// for a fleet that was never activated - and silently accept the job.
	if _, err := os.Stat(paths.DB); err != nil {
		return errors.New("this WarpHold is not a Fleet server; run 'warphold fleet activate' first")
	}

	st, err := store.Open(paths.DB)
	if err != nil {
		return errors.Wrap(err, "opening the fleet database")
	}

	defer st.Close() //nolint:errcheck // read-mostly, nothing buffered

	if c.agent != "" {
		if _, err := st.Agent(ctx, c.agent); err != nil {
			return errors.Errorf("no such agent: %s", c.agent)
		}
	}

	id, err := st.EnqueueJob(ctx, &store.Job{Kind: c.kind, AgentID: c.agent, ScheduledFor: time.Now()})
	if err != nil {
		return errors.Wrap(err, "queueing the job")
	}

	fmt.Fprintf(c.out.stdout(), "Queued %s job %d.\n", c.kind, id) //nolint:errcheck

	return nil
}
