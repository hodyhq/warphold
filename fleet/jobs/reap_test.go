package jobs

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/enroll"

	"github.com/kopia/kopia/fleet/store"
)

// repoDir is where the fixture's filesystem target keeps one agent's data.
func repoDir(fx *repoFixture, id string) string {
	return filepath.Join(fx.root, "agents", id)
}

func revoke(t *testing.T, fx *repoFixture, id string, ago time.Duration) {
	t.Helper()

	require.NoError(t, fx.st.RevokeAgent(context.Background(), id, time.Now().Add(-ago)))
}

func TestReapRemovesARevokedRepositoryPastItsRetention(t *testing.T) {
	fx := newRepoFixture(t, "ag_1", "ag_2")
	ctx := context.Background()

	require.NoError(t, fx.st.CreateDeviceKey(ctx, &store.DeviceKey{
		AccessKeyID: "WHKEY1", AgentID: "ag_1", SealedSecret: []byte("sealed"), Prefix: "ag_1/", CreatedAt: time.Now(),
	}))

	revoke(t, fx, "ag_1", 31*24*time.Hour)

	detail, err := Reap(fx.st)(ctx, store.Job{Kind: "reap"})
	require.NoError(t, err)
	require.Equal(t, "reaped 1/1 ok; 0 failed", detail)

	require.NoDirExists(t, repoDir(fx, "ag_1"))
	require.DirExists(t, repoDir(fx, "ag_2"), "a live device is untouched")

	a, err := fx.st.Agent(ctx, "ag_1")
	require.NoError(t, err)
	require.NotNil(t, a.RetiredAt)

	keys, err := fx.st.DeviceKeysForAgent(ctx, "ag_1")
	require.NoError(t, err)
	require.Empty(t, keys, "the gateway key row goes with the data")
}

func TestReapRefusesInsideTheRetentionWindow(t *testing.T) {
	fx := newRepoFixture(t, "ag_1")
	ctx := context.Background()

	revoke(t, fx, "ag_1", 29*24*time.Hour)

	detail, err := Reap(fx.st)(ctx, store.Job{Kind: "reap"})
	require.NoError(t, err)
	require.Equal(t, "reaped 0/0 ok; 0 failed, 1 still inside the retention window", detail)

	require.DirExists(t, repoDir(fx, "ag_1"))

	a, err := fx.st.Agent(ctx, "ag_1")
	require.NoError(t, err)
	require.Nil(t, a.RetiredAt)
}

func TestReapFollowsTheConfiguredRetention(t *testing.T) {
	fx := newRepoFixture(t, "ag_1")
	ctx := context.Background()

	revoke(t, fx, "ag_1", 8*24*time.Hour)
	require.NoError(t, fx.st.SetSetting(ctx, RevokedRetentionSetting, "7"))

	_, err := Reap(fx.st)(ctx, store.Job{Kind: "reap"})
	require.NoError(t, err)
	require.NoDirExists(t, repoDir(fx, "ag_1"))
}

// A value that is missing, unparsable or out of range must never shorten the
// window: the fallback is the default, not zero.
func TestReapIgnoresAnUnusableRetentionSetting(t *testing.T) {
	fx := newRepoFixture(t)
	ctx := context.Background()

	require.Equal(t, DefaultRetentionDays*24*time.Hour, retentionWindow(ctx, fx.st))

	for _, v := range []string{"", "soon", "0", "-1", "4000"} {
		require.NoError(t, fx.st.SetSetting(ctx, RevokedRetentionSetting, v))
		require.Equal(t, DefaultRetentionDays*24*time.Hour, retentionWindow(ctx, fx.st), "value %q", v)
	}
}

func TestReapIsIdempotent(t *testing.T) {
	fx := newRepoFixture(t, "ag_1")
	ctx := context.Background()

	revoke(t, fx, "ag_1", 40*24*time.Hour)

	_, err := Reap(fx.st)(ctx, store.Job{Kind: "reap"})
	require.NoError(t, err)

	retired, err := fx.st.Agent(ctx, "ag_1")
	require.NoError(t, err)
	require.NotNil(t, retired.RetiredAt)

	// A second sweep finds nothing left to do and says so without failing.
	detail, err := Reap(fx.st)(ctx, store.Job{Kind: "reap"})
	require.NoError(t, err)
	require.Equal(t, "reaped 0/0 ok; 0 failed", detail)

	again, err := fx.st.Agent(ctx, "ag_1")
	require.NoError(t, err)
	require.Equal(t, retired.RetiredAt.UTC(), again.RetiredAt.UTC(), "the retirement date is not rewritten")
}

// A device that was revoked and then brought back must never be reaped, even
// by a job that names it.
func TestReapRefusesAnAgentThatIsNoLongerRevoked(t *testing.T) {
	fx := newRepoFixture(t, "ag_1")
	ctx := context.Background()

	_, err := Reap(fx.st)(ctx, store.Job{Kind: "reap", AgentID: "ag_1"})
	require.ErrorContains(t, err, "ag_1 is not revoked")

	require.DirExists(t, repoDir(fx, "ag_1"))
}

func TestReapOfATargetWithNoLocalDataStillRetires(t *testing.T) {
	fx := newRepoFixture(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	// A b2 target keeps its bytes in a bucket the reap does not touch.
	tid, err := fx.st.CreateTarget(ctx, &store.Target{Name: "b2", Kind: "b2", Bucket: "warphold", CreatedAt: now})
	require.NoError(t, err)

	gid, err := fx.st.CreateGroup(ctx, &store.Group{Name: "Cloud", TargetID: tid, TemplateID: 1, CreatedAt: now})
	require.NoError(t, err)

	require.NoError(t, fx.st.CreateAgent(ctx, &store.Agent{
		ID: "ag_cloud", Name: "c", Hostname: "c", OS: "linux", Arch: "amd64", Scope: "user", GroupID: gid,
		BearerHash: []byte("h_c"), SealedBundle: []byte("b"), EnrolledAt: now,
	}))
	revoke(t, fx, "ag_cloud", 60*24*time.Hour)

	detail, err := Reap(fx.st)(ctx, store.Job{Kind: "reap"})
	require.NoError(t, err)
	require.Equal(t, "reaped 1/1 ok; 0 failed, 1 with no local repository", detail)

	a, err := fx.st.Agent(ctx, "ag_cloud")
	require.NoError(t, err)
	require.NotNil(t, a.RetiredAt)
}

func TestReapRefusesAnAgentIDThatIsNotAPathSegment(t *testing.T) {
	for _, bad := range []string{"", ".", "..", "../keep", "a/b", `a\b`, "./a"} {
		require.False(t, enroll.IsSafeAgentDir(bad), "%q", bad)
	}

	for _, good := range []string{"ag_1", "ag-2", "abcdef0123456789"} {
		require.True(t, enroll.IsSafeAgentDir(good), "%q", good)
	}
}

// The reap must not delete a directory a still-live agent shares a root with.
func TestReapLeavesTheRestOfTheTargetAlone(t *testing.T) {
	fx := newRepoFixture(t, "ag_1", "ag_2")
	ctx := context.Background()

	stray := filepath.Join(fx.root, "agents", "not-an-agent")
	require.NoError(t, os.MkdirAll(stray, 0o700))

	revoke(t, fx, "ag_1", 90*24*time.Hour)

	_, err := Reap(fx.st)(ctx, store.Job{Kind: "reap"})
	require.NoError(t, err)

	require.NoDirExists(t, repoDir(fx, "ag_1"))
	require.DirExists(t, repoDir(fx, "ag_2"))
	require.DirExists(t, stray)
}
