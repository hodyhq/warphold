package jobs

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/store"
	"github.com/kopia/kopia/repo/blob"
)

// blobCountAndBytes lists an agent's repository storage directly, the way the
// fixture corrupts it, as the ground truth for what the stats job should have
// recorded.
func blobCountAndBytes(t *testing.T, fx *repoFixture, id string) (count int64, bytes int64) {
	t.Helper()

	ctx := context.Background()
	require.NoError(t, fx.blobs(t, id).ListBlobs(ctx, "", func(bm blob.Metadata) error {
		count++
		bytes += bm.Length

		return nil
	}))

	return count, bytes
}

func TestStatsRecordsRepositorySize(t *testing.T) {
	fx := newRepoFixture(t, "ag_1", "ag_2")
	ctx := context.Background()

	wantBlobs, wantBytes := blobCountAndBytes(t, fx, "ag_1")
	require.NotZero(t, wantBlobs, "the fixture must have written at least one blob")

	detail, err := Stats(fx.st, fx.key, nil)(ctx, store.Job{Kind: "stats"})
	require.NoError(t, err)
	require.Equal(t, "measured 2/2 ok; 0 failed", detail)

	got, err := fx.st.RepoStat(ctx, "ag_1")
	require.NoError(t, err)
	require.Equal(t, wantBlobs, got.BlobCount)
	require.Equal(t, wantBytes, got.StoredBytes)
	require.NotZero(t, got.LogicalBytes, "a snapshot was taken, so some content exists")
	require.Nil(t, got.MirroredAt, "stats never touches the mirror job's half of the row")

	all, err := fx.st.RepoStats(ctx)
	require.NoError(t, err)
	require.Len(t, all, 2)
}

func TestStatsScopesToOneAgent(t *testing.T) {
	fx := newRepoFixture(t, "ag_1", "ag_2")
	ctx := context.Background()

	detail, err := Stats(fx.st, fx.key, nil)(ctx, store.Job{Kind: "stats", AgentID: "ag_1"})
	require.NoError(t, err)
	require.Equal(t, "measured 1/1 ok; 0 failed", detail)

	_, err = fx.st.RepoStat(ctx, "ag_1")
	require.NoError(t, err)

	_, err = fx.st.RepoStat(ctx, "ag_2")
	require.ErrorIs(t, err, store.ErrNotFound, "the other agent was never measured")
}

// TestStatsPreservesMirrorProgress proves the stats job does not clobber
// mirrored_at/mirrored_bytes even though it upserts the same row.
func TestStatsPreservesMirrorProgress(t *testing.T) {
	fx := newRepoFixture(t, "ag_1")
	ctx := context.Background()

	require.NoError(t, fx.st.SetMirrored(ctx, "ag_1", time.Now(), 4096))

	_, err := Stats(fx.st, fx.key, nil)(ctx, store.Job{Kind: "stats"})
	require.NoError(t, err)

	got, err := fx.st.RepoStat(ctx, "ag_1")
	require.NoError(t, err)
	require.NotNil(t, got.MirroredAt)
	require.EqualValues(t, 4096, got.MirroredBytes)
	require.NotZero(t, got.StoredBytes)
}
