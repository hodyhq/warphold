package jobs

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/store"
)

func TestTestRestoreRestoresAndComparesAFile(t *testing.T) {
	fx := newRepoFixture(t, "ag_1", "ag_2")

	detail, err := TestRestore(fx.st, fx.key.Open, nil)(context.Background(), store.Job{Kind: "test-restore"})
	require.NoError(t, err)
	require.Equal(t, "restored 2/2 ok; 0 failed", detail)
}

// The sample is restored from the *latest* snapshot: a file that only exists
// in the newest one still has to come back.
func TestTestRestoreUsesTheLatestSnapshot(t *testing.T) {
	fx := newRepoFixture(t, "ag_1")

	newest := t.TempDir()
	body := make([]byte, 256<<10)
	_, err := rand.Read(body)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(newest, "only.bin"), body, 0o600))

	fx.snapshotFrom(t, "ag_1", newest)

	detail, err := TestRestore(fx.st, fx.key.Open, nil)(context.Background(), store.Job{Kind: "test-restore"})
	require.NoError(t, err)
	require.Equal(t, "restored 1/1 ok; 0 failed", detail)

	// And with the data packs gone it fails loudly, with the raw error.
	fx.dropDataPacks(t, "ag_1")

	detail, err = TestRestore(fx.st, fx.key.Open, nil)(context.Background(), store.Job{Kind: "test-restore"})
	require.Error(t, err)
	require.Contains(t, detail, "restored 0/1 ok; 1 failed")
	require.Contains(t, detail, "ag_1: ")
	require.Contains(t, detail, "only.bin")
}

// The comparison is the job. If the bytes on disk ever stop matching the
// repository, it has to say so - with both digests, not a shrug.
func TestRestoredBytesMismatchIsLoud(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sample")
	require.NoError(t, os.WriteFile(path, []byte("restored"), 0o600))

	want := sha256.Sum256([]byte("original"))

	err := checkRestored("notes.md", want[:], 8, path)
	require.ErrorContains(t, err, "restored bytes differ for notes.md")
	require.ErrorContains(t, err, "sha256 ")

	// A short restore is caught on the length, before any hashing.
	err = checkRestored("notes.md", want[:], 99, path)
	require.ErrorContains(t, err, "8 bytes restored, 99 in the repository")

	// The matching case must pass, or the two assertions above prove nothing.
	ok := sha256.Sum256([]byte("restored"))
	require.NoError(t, checkRestored("notes.md", ok[:], 8, path))
}

// A never-snapshotted agent has nothing to restore and prove, so it must not
// be indistinguishable from a device the job actually verified: it is
// recorded as skipped, not ok, and the fleet result is non-ok so the digest
// surfaces it rather than hiding it behind a clean "1/1 ok".
func TestTestRestoreIsFineWithAnAgentThatHasNoSnapshots(t *testing.T) {
	fx := newRepoFixture(t)
	ctx := context.Background()

	// An agent whose repository exists but was never snapshotted.
	fx.provision(t, "ag_new")

	detail, err := TestRestore(fx.st, fx.key.Open, nil)(ctx, store.Job{Kind: "test-restore"})
	require.Error(t, err)
	require.Contains(t, detail, "restored 0/1 ok; 0 failed; 1 skipped")
	require.Contains(t, detail, "ag_new: no finished snapshot yet")
}
