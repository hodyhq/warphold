package jobs

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/store"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/maintenance"
)

// schedule reads the repository's own record of when maintenance last ran.
func schedule(t *testing.T, fx *repoFixture, id string) *maintenance.Schedule {
	t.Helper()

	r := fx.open(t, id, false)

	dr, ok := r.Repository.(repo.DirectRepository)
	require.True(t, ok)

	s, err := maintenance.GetSchedule(context.Background(), dr)
	require.NoError(t, err)

	return s
}

func owner(t *testing.T, fx *repoFixture, id string) string {
	t.Helper()

	r := fx.open(t, id, false)

	p, err := maintenance.GetParams(context.Background(), r.Repository)
	require.NoError(t, err)

	return p.Owner
}

func TestMaintenanceRunsAndRecordsIt(t *testing.T) {
	fx := newRepoFixture(t, "ag_1")

	require.True(t, schedule(t, fx, "ag_1").NextFullMaintenanceTime.IsZero(), "nothing has maintained this repository yet")

	detail, err := Maintenance(fx.st, fx.key.Open, nil)(context.Background(), store.Job{Kind: "maintenance"})
	require.NoError(t, err)
	require.Equal(t, "maintained 1/1 ok; 0 failed", detail)

	// The repository itself records the run, which is what makes this a real
	// maintenance cycle rather than a no-op that returned nil.
	after := schedule(t, fx, "ag_1")
	require.False(t, after.NextFullMaintenanceTime.IsZero())
	require.True(t, after.NextFullMaintenanceTime.After(time.Now()), "the next full cycle is scheduled ahead")
	require.NotEmpty(t, after.Runs, "the run log names the maintenance tasks that ran")
}

// A repository whose owner string no longer matches this Fleet (it moved, or
// it predates public_url) must be taken over rather than skipped forever.
func TestMaintenanceTakesOverAnOtherwiseOwnedRepository(t *testing.T) {
	fx := newRepoFixture(t, "ag_1")
	ctx := context.Background()

	r := fx.open(t, "ag_1", false)
	dr, ok := r.Repository.(repo.DirectRepository)
	require.True(t, ok)
	require.NoError(t, repo.DirectWriteSession(ctx, dr, repo.WriteSessionOptions{Purpose: "test"},
		func(ctx context.Context, w repo.DirectRepositoryWriter) error {
			p, err := maintenance.GetParams(ctx, w)
			require.NoError(t, err)
			p.Owner = "someone@elsewhere"

			return maintenance.SetParams(ctx, w, p)
		}))
	r.close(ctx)

	require.Equal(t, "someone@elsewhere", owner(t, fx, "ag_1"))

	detail, err := Maintenance(fx.st, fx.key.Open, nil)(ctx, store.Job{Kind: "maintenance"})
	require.NoError(t, err)
	require.Equal(t, "maintained 1/1 ok; 0 failed", detail)

	require.Equal(t, fleetIdentity(ctx, fx.st).owner(), owner(t, fx, "ag_1"))
	require.False(t, schedule(t, fx, "ag_1").NextFullMaintenanceTime.IsZero(), "and it actually ran")
}

func TestMaintenanceReportsTheDeviceThatFailed(t *testing.T) {
	fx := newRepoFixture(t, "ag_1", "ag_2")
	ctx := context.Background()

	// An agent whose escrowed bundle cannot be unsealed - the shape a wrong
	// sealing key or a corrupted row takes.
	require.NoError(t, fx.st.CreateAgent(ctx, &store.Agent{
		ID: "ag_bad", Name: "bad", Hostname: "bad", OS: "linux", Arch: "amd64", Scope: "user", GroupID: fx.group,
		BearerHash: []byte("h_bad"), SealedBundle: []byte("not sealed at all"), EnrolledAt: time.Now(),
	}))

	detail, err := Maintenance(fx.st, fx.key.Open, nil)(ctx, store.Job{Kind: "maintenance"})
	require.Error(t, err)
	require.Contains(t, detail, "maintained 2/3 ok; 1 failed")
	require.Contains(t, detail, "ag_bad: unsealing the escrowed bundle failed")
}
