package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/store"
	"github.com/kopia/kopia/internal/clock"
)

// seedAgents creates the group chain and the named agents, which the jobs and
// repo_stats foreign keys require.
func seedAgents(t *testing.T, s *store.Store, ids ...string) {
	t.Helper()
	ctx := context.Background()
	now := clock.Now().UTC().Truncate(time.Second)
	tid, err := s.CreateTarget(ctx, &store.Target{Name: "disk", Kind: "hosted", Path: "/srv/warphold/hosted", CreatedAt: now})
	require.NoError(t, err)
	tpl, err := s.CreateTemplate(ctx, &store.Template{Name: "default", Sources: []string{"~"}, PolicyJSON: json.RawMessage(`{}`), CreatedAt: now})
	require.NoError(t, err)
	gid, err := s.CreateGroup(ctx, &store.Group{Name: "Laptops", TargetID: tid, TemplateID: tpl, CreatedAt: now})
	require.NoError(t, err)
	for _, id := range ids {
		require.NoError(t, s.CreateAgent(ctx, &store.Agent{
			ID: id, Name: id, Hostname: id, OS: "linux", Arch: "amd64", Scope: "user", GroupID: gid,
			BearerHash: []byte("h_" + id), SealedBundle: []byte("b"), EnrolledAt: now,
		}))
	}
}

func TestJobsRoundTrip(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := clock.Now().UTC().Truncate(time.Second)
	seedAgents(t, s, "ag_1", "ag_2")

	j := store.Job{Kind: "verify", AgentID: "ag_1", ScheduledFor: now}
	id, err := s.EnqueueJob(ctx, &j)
	require.NoError(t, err)
	require.Equal(t, id, j.ID)

	// A fleet-wide job has no agent, which must be NULL and not "" - the column
	// has a foreign key to agents(id).
	_, err = s.EnqueueJob(ctx, &store.Job{Kind: "mirror", ScheduledFor: now})
	require.NoError(t, err)

	got, err := s.RecentJobs(ctx, "mirror", 10)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Empty(t, got[0].AgentID)
	require.Equal(t, now, got[0].ScheduledFor.UTC())
	require.Equal(t, "pending", got[0].Status)

	all, err := s.RecentJobs(ctx, "", 10)
	require.NoError(t, err)
	require.Len(t, all, 2)

	mine, err := s.JobsForAgent(ctx, "ag_1", 10)
	require.NoError(t, err)
	require.Len(t, mine, 1)
	require.Equal(t, "verify", mine[0].Kind)

	theirs, err := s.JobsForAgent(ctx, "ag_2", 10)
	require.NoError(t, err)
	require.Empty(t, theirs)
}

func TestEnqueueJobNeedsAKind(t *testing.T) {
	s := openTemp(t)
	_, err := s.EnqueueJob(context.Background(), &store.Job{ScheduledFor: clock.Now()})
	require.Error(t, err)
}

func TestClaimDueJobSkipsWhatIsNotDue(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := clock.Now().UTC().Truncate(time.Second)

	_, err := s.EnqueueJob(ctx, &store.Job{Kind: "mirror", ScheduledFor: now.Add(time.Hour)})
	require.NoError(t, err)

	_, err = s.ClaimDueJob(ctx, now)
	require.ErrorIs(t, err, store.ErrNotFound)

	claimed, err := s.ClaimDueJob(ctx, now.Add(2*time.Hour))
	require.NoError(t, err)
	require.Equal(t, "running", claimed.Status)
	require.NotNil(t, claimed.StartedAt)
}

func TestClaimDueJobTakesTheOldestFirst(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := clock.Now().UTC().Truncate(time.Second)

	_, err := s.EnqueueJob(ctx, &store.Job{Kind: "mirror", ScheduledFor: now})
	require.NoError(t, err)
	_, err = s.EnqueueJob(ctx, &store.Job{Kind: "stats", ScheduledFor: now.Add(-time.Hour)})
	require.NoError(t, err)

	claimed, err := s.ClaimDueJob(ctx, now)
	require.NoError(t, err)
	require.Equal(t, "stats", claimed.Kind)
}

// TestClaimDueJobHasExactlyOneWinner is the atomic-claim guarantee: two
// claimers racing for one row, a hundred times over.
func TestClaimDueJobHasExactlyOneWinner(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := clock.Now().UTC().Truncate(time.Second)

	for range 100 {
		_, err := s.EnqueueJob(ctx, &store.Job{Kind: "mirror", ScheduledFor: now})
		require.NoError(t, err)

		var (
			wg   sync.WaitGroup
			mu   sync.Mutex
			won  []int64
			lost int
		)

		start := make(chan struct{})

		for range 2 {
			wg.Add(1)

			go func() {
				defer wg.Done()
				<-start

				j, err := s.ClaimDueJob(ctx, now)

				mu.Lock()
				defer mu.Unlock()

				switch {
				case err == nil:
					won = append(won, j.ID)
				case errors.Is(err, store.ErrNotFound):
					lost++
				default:
					t.Errorf("claim: %v", err)
				}
			}()
		}

		close(start)
		wg.Wait()

		require.Len(t, won, 1, "exactly one claimer must win")
		require.Equal(t, 1, lost)

		require.NoError(t, s.FinishJob(ctx, won[0], now, "ok", "done"))
	}
}

func TestFinishJob(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := clock.Now().UTC().Truncate(time.Second)

	id, err := s.EnqueueJob(ctx, &store.Job{Kind: "mirror", ScheduledFor: now})
	require.NoError(t, err)

	_, err = s.ClaimDueJob(ctx, now)
	require.NoError(t, err)

	require.NoError(t, s.FinishJob(ctx, id, now.Add(time.Minute), "error", "boom"))

	got, err := s.RecentJobs(ctx, "mirror", 1)
	require.NoError(t, err)
	require.Equal(t, "error", got[0].Status)
	require.Equal(t, "boom", got[0].Detail)
	require.NotNil(t, got[0].FinishedAt)
	require.Equal(t, now.Add(time.Minute), got[0].FinishedAt.UTC())

	require.ErrorIs(t, s.FinishJob(ctx, 9999, now, "ok", ""), store.ErrNotFound)
}

func TestFinishJobTruncatesADetailToAValidString(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := clock.Now()

	id, err := s.EnqueueJob(ctx, &store.Job{Kind: "mirror", ScheduledFor: now})
	require.NoError(t, err)

	long := ""
	for range 3000 {
		long += "é" // two bytes, so the cut lands mid-rune unless it is guarded
	}

	require.NoError(t, s.FinishJob(ctx, id, now, "error", long))

	got, err := s.RecentJobs(ctx, "mirror", 1)
	require.NoError(t, err)
	require.Less(t, len(got[0].Detail), len(long))
	require.True(t, utf8.ValidString(got[0].Detail))
}

func TestRequeueStaleJobs(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := clock.Now().UTC().Truncate(time.Second)

	// A claim from seven hours ago, left behind by a crash.
	stale, err := s.EnqueueJob(ctx, &store.Job{Kind: "mirror", ScheduledFor: now.Add(-8 * time.Hour)})
	require.NoError(t, err)
	_, err = s.ClaimDueJob(ctx, now.Add(-7*time.Hour))
	require.NoError(t, err)

	// Another kind is never touched by a mirror requeue.
	other, err := s.EnqueueJob(ctx, &store.Job{Kind: "stats", ScheduledFor: now.Add(-8 * time.Hour)})
	require.NoError(t, err)
	_, err = s.ClaimDueJob(ctx, now.Add(-7*time.Hour))
	require.NoError(t, err)

	// A claim from a minute ago, which is still running.
	fresh, err := s.EnqueueJob(ctx, &store.Job{Kind: "mirror", ScheduledFor: now.Add(-2 * time.Minute)})
	require.NoError(t, err)
	_, err = s.ClaimDueJob(ctx, now.Add(-time.Minute))
	require.NoError(t, err)

	n, err := s.RequeueStaleJobs(ctx, "mirror", now.Add(-6*time.Hour))
	require.NoError(t, err)
	require.EqualValues(t, 1, n)

	all, err := s.RecentJobs(ctx, "", 10)
	require.NoError(t, err)

	byID := map[int64]store.Job{}
	for _, j := range all {
		byID[j.ID] = j
	}

	require.Equal(t, "pending", byID[stale].Status)
	require.Nil(t, byID[stale].StartedAt)
	require.Equal(t, "running", byID[fresh].Status)
	require.Equal(t, "running", byID[other].Status)
}

func TestRepoStatsMirrorProgress(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := clock.Now().UTC().Truncate(time.Second)
	seedAgents(t, s, "ag_1")

	_, err := s.RepoStat(ctx, "ag_1")
	require.ErrorIs(t, err, store.ErrNotFound)

	require.NoError(t, s.SetMirrored(ctx, "ag_1", now, 4096))

	got, err := s.RepoStat(ctx, "ag_1")
	require.NoError(t, err)
	require.NotNil(t, got.MirroredAt)
	require.Equal(t, now, got.MirroredAt.UTC())
	require.EqualValues(t, 4096, got.MirroredBytes)

	later := now.Add(time.Hour)
	require.NoError(t, s.SetMirrored(ctx, "ag_1", later, 8192))

	got, err = s.RepoStat(ctx, "ag_1")
	require.NoError(t, err)
	require.Equal(t, later, got.MirroredAt.UTC())
	require.EqualValues(t, 8192, got.MirroredBytes)
}
