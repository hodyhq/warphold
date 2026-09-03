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
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/upload"
)

// newHostedFixture is newRepoFixture's hosted twin: one agent provisioned onto
// a hosted DISK target, exactly as enrollment does it, with one real snapshot
// in the repository.
//
// The point of the difference: the connect token escrowed for a hosted device
// is the device's own S3 view of the gateway - endpoint fleet.example.com,
// signed with an append-only credential - and nothing in this test serves it.
// A job that opened the escrowed connection would have to dial that name; a
// job that goes through fleetConnection reads the same bytes off disk through
// the "warphold-hosted" blob adapter.
func newHostedFixture(t *testing.T, id string) *repoFixture {
	t.Helper()

	ctx := context.Background()
	st := openTemp(t)
	now := time.Now().UTC().Truncate(time.Second)

	salt, err := seal.NewSalt()
	require.NoError(t, err)

	key := seal.Derive("a fleet passphrase", salt)
	root := t.TempDir()

	tid, err := st.CreateTarget(ctx, &store.Target{
		Name: "Fleet disk", Kind: "hosted", StorageMode: "disk", Path: root, CreatedAt: now,
	})
	require.NoError(t, err)

	tpl, err := st.CreateTemplate(ctx, &store.Template{
		Name: "default", Sources: []string{"~"}, PolicyJSON: json.RawMessage(`{}`), CreatedAt: now,
	})
	require.NoError(t, err)

	gid, err := st.CreateGroup(ctx, &store.Group{Name: "Devices", TargetID: tid, TemplateID: tpl, CreatedAt: now})
	require.NoError(t, err)

	f := &repoFixture{st: st, key: key, root: root, agents: []string{id}, target: tid, group: gid}

	// Order matters, and it is enrollment's own: the agent row goes in first
	// because device_keys.agent_id references it, and the bundle is filled in
	// once provisioning has produced one.
	require.NoError(t, st.CreateAgent(ctx, &store.Agent{
		ID: id, Name: id, Hostname: id, OS: "linux", Arch: "amd64", Scope: "user", GroupID: gid,
		BearerHash: []byte("h_" + id), SealedBundle: []byte{}, EnrolledAt: now,
	}))

	b, err := (&enroll.Provisioner{
		Owner: fleetIdentity(ctx, st).owner(), Store: st, SealKey: key,
	}).Provision(ctx, enroll.TargetSpec{
		Kind: "hosted", StorageMode: "disk", HostedRoot: root, PublicHost: "fleet.example.com", TLS: true,
	}, id)
	require.NoError(t, err)

	// The escrowed token really is the unreachable device view; if that ever
	// stops being true this test is no longer proving anything.
	ci, _, err := repo.DecodeToken(b.ConnectToken)
	require.NoError(t, err)
	require.Equal(t, "s3", ci.Type, "a hosted device is enrolled against the gateway, not the disk")

	raw, err := json.Marshal(b)
	require.NoError(t, err)

	sealed, err := key.Seal(raw)
	require.NoError(t, err)

	require.NoError(t, st.SetAgentBundle(ctx, id, sealed))

	f.source = t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(f.source, "small.txt"), []byte("hello warphold"), 0o600))

	r := f.open(t, id, false)
	require.NoError(t, repo.WriteSession(ctx, r.Repository, repo.WriteSessionOptions{Purpose: "test-snapshot"},
		func(ctx context.Context, w repo.RepositoryWriter) error {
			dir, err := localfs.Directory(f.source)
			if err != nil {
				return err
			}

			man, err := upload.NewUploader(w).Upload(ctx, dir, nil, snapshot.SourceInfo{
				Host: "testhost", UserName: "testuser", Path: f.source,
			})
			if err != nil {
				return err
			}

			_, err = snapshot.SaveSnapshot(ctx, w, man)

			return err
		}))

	return f
}

// TestVerifyRunsAgainstAHostedDiskRepository is Task 28 concern #1 pinned: the
// repository jobs must be able to open a HOSTED device's repository
// server-side. Before fleetConnection they could not - the escrowed token
// names the Fleet's own S3 gateway, so a job would have had to make an HTTP
// round trip through itself with the device's append-only credential.
func TestVerifyRunsAgainstAHostedDiskRepository(t *testing.T) {
	f := newHostedFixture(t, "ag_hosted")

	detail, err := Verify(f.st, f.key, nil)(context.Background(), store.Job{Kind: "verify"})
	require.NoError(t, err, detail)
	require.Contains(t, detail, "verified 1/1 ok; 0 failed")
}

// The bytes the job wrote are the bytes the gateway serves: same flat key
// space, under the device's own prefix. A repository written through Kopia's
// stock "filesystem" provider would be sharded into <root>/xx/yy/ and be
// invisible to the device.
func TestHostedJobRepositoryIsTheGatewaysKeySpace(t *testing.T) {
	f := newHostedFixture(t, "ag_hosted")

	entries, err := os.ReadDir(filepath.Join(f.root, "ag_hosted"))
	require.NoError(t, err)
	require.NotEmpty(t, entries, "the repository must be under the device's flat prefix")

	for _, e := range entries {
		require.False(t, e.IsDir(), "the gateway key space is flat, got directory %q", e.Name())
	}
}

// A hosted target row with no path fails loudly, rather than silently falling
// back to the device's unreachable S3 view.
func TestFleetConnectionRefusesAPathlessHostedTarget(t *testing.T) {
	f := newHostedFixture(t, "ag_hosted")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	tid, err := f.st.CreateTarget(ctx, &store.Target{Name: "broken", Kind: "hosted", StorageMode: "disk", CreatedAt: now})
	require.NoError(t, err)

	tpl, err := f.st.CreateTemplate(ctx, &store.Template{
		Name: "t2", Sources: []string{"~"}, PolicyJSON: json.RawMessage(`{}`), CreatedAt: now,
	})
	require.NoError(t, err)

	gid, err := f.st.CreateGroup(ctx, &store.Group{Name: "broken", TargetID: tid, TemplateID: tpl, CreatedAt: now})
	require.NoError(t, err)

	a := store.Agent{ID: "ag_pathless", Name: "x", Hostname: "x", OS: "linux", Arch: "amd64", Scope: "user", GroupID: gid, EnrolledAt: now}

	_, _, err = fleetConnection(ctx, f.st, nil, a, "ag_pathless/")
	require.ErrorContains(t, err, "has no path")
}

// A cloud-direct hosted target on a Fleet with no cloud opener is an error,
// not a fallback to the device's credential.
func TestFleetConnectionRefusesCloudWithoutAnOpener(t *testing.T) {
	f := newHostedFixture(t, "ag_hosted")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	tid, err := f.st.CreateTarget(ctx, &store.Target{
		Name: "cloud", Kind: "hosted", StorageMode: "cloud", Bucket: "b", Region: "r", CreatedAt: now,
	})
	require.NoError(t, err)

	tpl, err := f.st.CreateTemplate(ctx, &store.Template{
		Name: "t3", Sources: []string{"~"}, PolicyJSON: json.RawMessage(`{}`), CreatedAt: now,
	})
	require.NoError(t, err)

	gid, err := f.st.CreateGroup(ctx, &store.Group{Name: "cloud", TargetID: tid, TemplateID: tpl, CreatedAt: now})
	require.NoError(t, err)

	a := store.Agent{ID: "ag_cloud", Name: "x", Hostname: "x", OS: "linux", Arch: "amd64", Scope: "user", GroupID: gid, EnrolledAt: now}

	_, _, err = fleetConnection(ctx, f.st, nil, a, "ag_cloud/")
	require.ErrorContains(t, err, "cannot open cloud-direct repositories server-side")
}
