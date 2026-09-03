package jobs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/enroll"
	"github.com/kopia/kopia/fleet/seal"
	"github.com/kopia/kopia/fleet/store"
	"github.com/kopia/kopia/fs/localfs"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/upload"
)

// repoFixture is a filesystem target with one real Kopia repository per agent,
// provisioned by fleet/enroll exactly as enrollment does, plus the escrowed
// sealed bundle in the agents table. Every repository job runs against this.
type repoFixture struct {
	st     *store.Store
	key    seal.Key
	root   string   // the target's path
	source string   // the directory the snapshots were taken from
	agents []string // agent ids, in order
	target int64
	group  int64
}

func newRepoFixture(t *testing.T, ids ...string) *repoFixture {
	t.Helper()

	ctx := context.Background()
	st := openTemp(t)
	now := time.Now().UTC().Truncate(time.Second)

	salt, err := seal.NewSalt()
	require.NoError(t, err)

	key := seal.Derive("a fleet passphrase", salt)
	root := t.TempDir()

	tid, err := st.CreateTarget(ctx, &store.Target{Name: "disk", Kind: "filesystem", Path: root, CreatedAt: now})
	require.NoError(t, err)

	tpl, err := st.CreateTemplate(ctx, &store.Template{Name: "default", Sources: []string{"~"}, PolicyJSON: json.RawMessage(`{}`), CreatedAt: now})
	require.NoError(t, err)

	gid, err := st.CreateGroup(ctx, &store.Group{Name: "Laptops", TargetID: tid, TemplateID: tpl, CreatedAt: now})
	require.NoError(t, err)

	fx := &repoFixture{st: st, key: key, root: root, agents: ids, target: tid, group: gid}

	prov := &enroll.Provisioner{Owner: fleetIdentity(ctx, st).owner()}

	for _, id := range ids {
		b, err := prov.Provision(ctx, enroll.TargetSpec{Kind: "filesystem", Path: root}, id)
		require.NoError(t, err)

		raw, err := json.Marshal(b)
		require.NoError(t, err)

		sealed, err := key.Seal(raw)
		require.NoError(t, err)

		require.NoError(t, st.CreateAgent(ctx, &store.Agent{
			ID: id, Name: id, Hostname: id, OS: "linux", Arch: "amd64", Scope: "user", GroupID: gid,
			BearerHash: []byte("h_" + id), SealedBundle: sealed, EnrolledAt: now,
		}))
	}

	fx.source = t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(fx.source, "small.txt"), []byte("hello warphold"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(fx.source, "notes.md"), []byte("# notes\nsecond file\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(fx.source, "sub"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(fx.source, "sub", "nested.bin"), make([]byte, 4096), 0o600))

	for _, id := range ids {
		fx.snapshot(t, id)
	}

	return fx
}

func (f *repoFixture) agent(t *testing.T, id string) store.Agent {
	t.Helper()

	a, err := f.st.Agent(context.Background(), id)
	require.NoError(t, err)

	return *a
}

// open opens one agent's repository the way the jobs do.
func (f *repoFixture) open(t *testing.T, id string, readOnly bool) *openedRepo {
	t.Helper()

	ctx := context.Background()

	r, err := openAgentRepo(ctx, f.key, f.agent(t, id), fleetIdentity(ctx, f.st), readOnly)
	require.NoError(t, err)
	t.Cleanup(func() { r.close(context.Background()) })

	return r
}

// snapshot takes one real snapshot of the fixture's source directory.
func (f *repoFixture) snapshot(t *testing.T, id string) {
	t.Helper()

	ctx := context.Background()
	r := f.open(t, id, false)

	require.NoError(t, repo.WriteSession(ctx, r.Repository, repo.WriteSessionOptions{Purpose: "test-snapshot"}, func(ctx context.Context, w repo.RepositoryWriter) error {
		dir, err := localfs.Directory(f.source)
		if err != nil {
			return err
		}

		man, err := upload.NewUploader(w).Upload(ctx, dir, nil, snapshot.SourceInfo{Host: "testhost", UserName: "testuser", Path: f.source})
		if err != nil {
			return err
		}

		_, err = snapshot.SaveSnapshot(ctx, w, man)

		return err
	}))
}

// blobs opens the raw blob storage behind one agent's repository, which is how
// a test corrupts a repository the way the world does.
func (f *repoFixture) blobs(t *testing.T, id string) blob.Storage {
	t.Helper()

	ctx := context.Background()

	plain, err := f.key.Open(f.agent(t, id).SealedBundle)
	require.NoError(t, err)

	var b enroll.Bundle
	require.NoError(t, json.Unmarshal(plain, &b))

	ci, _, err := repo.DecodeToken(b.ConnectToken)
	require.NoError(t, err)

	st, err := blob.NewStorage(ctx, ci, false)
	require.NoError(t, err)

	t.Cleanup(func() { st.Close(ctx) }) //nolint:errcheck // test cleanup

	return st
}

// deleteAPackBlob removes one data pack, so the repository's index still
// references content nothing backs any more.
func (f *repoFixture) deleteAPackBlob(t *testing.T, id string) blob.ID {
	t.Helper()

	ctx := context.Background()
	st := f.blobs(t, id)

	var found blob.ID

	require.NoError(t, st.ListBlobs(ctx, "p", func(m blob.Metadata) error {
		if found == "" {
			found = m.BlobID
		}

		return nil
	}))
	require.NotEmpty(t, found, "the fixture must have written at least one data pack")
	require.NoError(t, st.DeleteBlob(ctx, found))

	return found
}
