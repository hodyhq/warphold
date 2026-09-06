package jobs

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/store"
)

func TestVerifyPassesOnAGoodRepository(t *testing.T) {
	fx := newRepoFixture(t, "ag_1", "ag_2")

	detail, err := Verify(fx.st, fx.key.Open, nil)(context.Background(), store.Job{Kind: "verify"})
	require.NoError(t, err)
	require.Equal(t, "verified 2/2 ok; 0 failed", detail)
}

func TestVerifyFailsWithTheRawErrorOnACorruptedRepository(t *testing.T) {
	fx := newRepoFixture(t, "ag_1", "ag_2")
	gone := fx.deleteAPackBlob(t, "ag_2")

	detail, err := Verify(fx.st, fx.key.Open, nil)(context.Background(), store.Job{Kind: "verify"})
	require.Error(t, err)

	// One device's corruption does not cost the other its verification, and
	// the row carries the verifier's own words, not a paraphrase.
	require.Contains(t, detail, "verified 1/2 ok; 1 failed")
	require.Contains(t, detail, "ag_2: ")
	require.Contains(t, detail, string(gone), "the detail names the missing blob")
}

func TestVerifyRunsOnlyTheAgentTheJobNames(t *testing.T) {
	fx := newRepoFixture(t, "ag_1", "ag_2")
	fx.deleteAPackBlob(t, "ag_2")

	detail, err := Verify(fx.st, fx.key.Open, nil)(context.Background(), store.Job{Kind: "verify", AgentID: "ag_1"})
	require.NoError(t, err)
	require.Equal(t, "verified 1/1 ok; 0 failed", detail)
}

func TestVerifySkipsARevokedAgent(t *testing.T) {
	fx := newRepoFixture(t, "ag_1", "ag_2")
	ctx := context.Background()

	require.NoError(t, fx.st.RevokeAgent(ctx, "ag_2", time.Now()))

	detail, err := Verify(fx.st, fx.key.Open, nil)(ctx, store.Job{Kind: "verify"})
	require.NoError(t, err)
	require.Equal(t, "verified 1/1 ok; 0 failed", detail)
}

func TestVerifyStopsWhenTheContextIsDone(t *testing.T) {
	fx := newRepoFixture(t, "ag_1")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Verify(fx.st, fx.key.Open, nil)(ctx, store.Job{Kind: "verify"})
	require.ErrorIs(t, err, context.Canceled)
}
