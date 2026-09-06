package enroll_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/enroll"
	"github.com/kopia/kopia/fleet/gateway"
	"github.com/kopia/kopia/fleet/seal"
	"github.com/kopia/kopia/fleet/store"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/s3"
	"github.com/kopia/kopia/repo/maintenance"
)

const publicHost = "fleet.example.com"

var sealKey = seal.Derive("test-passphrase", make([]byte, 16))

// seedAgent builds the target/template/group/agent chain a device key's
// foreign key needs, and returns the store.
func seedAgent(t *testing.T, agentID string) *store.Store {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "fleet.db"))
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() }) //nolint:errcheck

	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	tid, err := st.CreateTarget(ctx, &store.Target{Name: "hosted", Kind: "hosted", StorageMode: "disk", CreatedAt: now})
	require.NoError(t, err)
	tpl, err := st.CreateTemplate(ctx, &store.Template{Name: "default", Sources: []string{"~"}, PolicyJSON: json.RawMessage(`{}`), CreatedAt: now})
	require.NoError(t, err)
	gid, err := st.CreateGroup(ctx, &store.Group{Name: "Laptops", TargetID: tid, TemplateID: tpl, CreatedAt: now})
	require.NoError(t, err)
	require.NoError(t, st.CreateAgent(ctx, &store.Agent{
		ID: agentID, Name: agentID, Hostname: agentID, OS: "linux", Arch: "amd64", Scope: "user", GroupID: gid,
		BearerHash: []byte("hash-" + agentID), SealedBundle: []byte{}, EnrolledAt: now,
	}))

	return st
}

func hostedSpec(root string) enroll.TargetSpec {
	return enroll.TargetSpec{Kind: "hosted", StorageMode: "disk", HostedRoot: root, PublicHost: publicHost, TLS: true}
}

func TestNewGatewayCredentialsAreS3Shaped(t *testing.T) {
	seen := map[string]bool{}

	for range 100 {
		akid, secret, err := enroll.NewGatewayCredentials()
		require.NoError(t, err)
		require.Len(t, akid, 20)
		require.True(t, strings.HasPrefix(akid, "WH"))
		require.Regexp(t, `^WH[A-Z2-7]{18}$`, akid)
		require.Len(t, secret, 40)
		require.Regexp(t, `^[A-Za-z0-9_-]{40}$`, secret)
		require.False(t, seen[akid], "access key ids must not repeat")
		require.False(t, seen[secret], "secrets must not repeat")
		seen[akid], seen[secret] = true, true
	}
}

func TestProvisionHostedCreatesReadableRepositoryAndDeviceKey(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st := seedAgent(t, "ag_h1")

	p := &enroll.Provisioner{Owner: "fleet@" + publicHost, Store: st, SealKey: sealKey}
	b, err := p.Provision(ctx, hostedSpec(root), "ag_h1")
	require.NoError(t, err)

	// --- the bundle
	require.Equal(t, "ag_h1/", b.Prefix)
	require.Equal(t, "https://"+publicHost, b.Endpoint)
	require.Equal(t, "warphold", b.Bucket)
	require.Equal(t, "warphold", b.Region)
	require.Len(t, b.GatewayKeyID, 20)
	require.Len(t, b.GatewayKey, 40)
	require.Len(t, b.Password, 43)
	require.Empty(t, b.WriterKeyID, "a hosted device holds no cloud credential")

	// --- the device_keys row
	k, err := st.DeviceKey(ctx, b.GatewayKeyID)
	require.NoError(t, err)
	require.Equal(t, "ag_h1", k.AgentID)
	require.Equal(t, "ag_h1/", k.Prefix, "the gateway confines the credential to exactly this prefix")
	require.False(t, k.ReadOnly)
	require.Nil(t, k.DisabledAt)
	require.NotContains(t, string(k.SealedSecret), b.GatewayKey, "the secret must not be stored in the clear")

	plain, err := sealKey.Open(k.SealedSecret)
	require.NoError(t, err)
	require.Equal(t, b.GatewayKey, string(plain))

	// --- what the device is told: the stock S3 backend, path-style, our bucket
	ci, pw, err := repo.DecodeToken(b.ConnectToken)
	require.NoError(t, err)
	require.Equal(t, b.Password, pw)
	require.Equal(t, "s3", ci.Type)

	o, ok := ci.Config.(*s3.Options)
	require.True(t, ok, "connection info config is %T", ci.Config)
	require.Equal(t, "warphold", o.BucketName)
	require.Equal(t, "ag_h1/", o.Prefix)
	require.Equal(t, publicHost, o.Endpoint, "minio-go wants the bare host, not a URL")
	require.False(t, o.DoNotUseTLS)
	require.Equal(t, b.GatewayKeyID, o.AccessKeyID)
	require.Equal(t, b.GatewayKey, o.SecretAccessKey)
	require.Equal(t, "warphold", o.Region)

	// --- the repository is on disk, flat, where the gateway serves it
	require.FileExists(t, filepath.Join(root, "ag_h1", "kopia.repository"))

	ents, err := os.ReadDir(filepath.Join(root, "ag_h1"))
	require.NoError(t, err)
	for _, e := range ents {
		require.False(t, e.IsDir(), "the hosted layout is flat; %q is a directory", e.Name())
	}

	// --- and it opens, with Fleet as the maintenance owner
	adminSt, err := blob.NewStorage(ctx, blob.ConnectionInfo{
		Type: gateway.HostedStorageType, Config: &gateway.HostedOptions{Root: root, Prefix: "ag_h1/"},
	}, false)
	require.NoError(t, err)
	defer adminSt.Close(ctx) //nolint:errcheck

	cfg := filepath.Join(t.TempDir(), "repository.config")
	require.NoError(t, repo.Connect(ctx, cfg, adminSt, b.Password, &repo.ConnectOptions{}))

	r, err := repo.Open(ctx, cfg, b.Password, nil)
	require.NoError(t, err)
	defer r.Close(ctx) //nolint:errcheck

	params, err := maintenance.GetParams(ctx, r)
	require.NoError(t, err)
	require.Equal(t, "fleet@"+publicHost, params.Owner, "devices must never own maintenance")

	// --- a second device gets its own key, prefix and repository
	st2 := seedAgent(t, "ag_h2")
	p2 := &enroll.Provisioner{Owner: "fleet@" + publicHost, Store: st2, SealKey: sealKey}
	b2, err := p2.Provision(ctx, hostedSpec(root), "ag_h2")
	require.NoError(t, err)
	require.NotEqual(t, b.GatewayKeyID, b2.GatewayKeyID)
	require.NotEqual(t, b.Password, b2.Password)
	require.Equal(t, "ag_h2/", b2.Prefix)
	require.FileExists(t, filepath.Join(root, "ag_h2", "kopia.repository"))
}

func TestProvisionHostedDisablesTheKeyWhenTheRepositoryFails(t *testing.T) {
	ctx := context.Background()
	st := seedAgent(t, "ag_h1")
	root := t.TempDir()

	boom := errors.New("storage is on fire")
	p := &enroll.Provisioner{
		Owner: "fleet@" + publicHost, Store: st, SealKey: sealKey,
		// Fails *after* writing a real repository, which is the case that
		// matters: the rollback has something to clean up.
		InitializeForTesting: func(ctx context.Context, ci blob.ConnectionInfo, password string) error {
			bst, err := blob.NewStorage(ctx, ci, true)
			require.NoError(t, err)
			require.NoError(t, repo.Initialize(ctx, bst, &repo.NewRepositoryOptions{}, password))
			require.NoError(t, bst.Close(ctx))
			require.FileExists(t, filepath.Join(root, "ag_h1", "kopia.repository"))

			return boom
		},
	}

	_, err := p.Provision(ctx, hostedSpec(root), "ag_h1")
	require.ErrorIs(t, err, boom)

	require.NoDirExists(t, filepath.Join(root, "ag_h1"),
		"a repository from an enrollment that never completed is nobody else's to reap")

	keys, err := st.DeviceKeysForAgent(ctx, "ag_h1")
	require.NoError(t, err)
	require.Len(t, keys, 1, "the row is kept, disabled - never deleted, so the id is not reissued")
	require.NotNil(t, keys[0].DisabledAt, "an orphaned gateway key must not stay usable")

	_, err = st.DeviceKey(ctx, keys[0].AccessKeyID)
	require.ErrorIs(t, err, store.ErrNotFound)
}

// A cloud-direct target has nothing on disk for rollback to clean up -
// RemoveHostedRepository no-ops without a HostedRoot - so the only thing left
// to unwind is the device key, exactly as for a disk target.
func TestProvisionCloudDirectDisablesTheKeyWhenTheRepositoryFails(t *testing.T) {
	ctx := context.Background()
	st := seedAgent(t, "ag_c1")

	const adminSecret = "admin-secret-do-not-leak"

	objs, err := gateway.NewCloud(ctx, blob.ConnectionInfo{Type: "s3", Config: &s3.Options{
		BucketName: "customer-bucket", Region: "us-east-1", Endpoint: "s3.example.invalid",
		AccessKeyID: "AKIDADMINADMINADMIN", SecretAccessKey: adminSecret,
	}}, "")
	require.NoError(t, err)

	boom := errors.New("bucket is on fire")
	p := &enroll.Provisioner{
		Owner: "fleet@" + publicHost, Store: st, SealKey: sealKey,
		HostedCloudStore:     func(ctx context.Context) (gateway.ObjectStore, error) { return objs, nil },
		InitializeForTesting: func(ctx context.Context, ci blob.ConnectionInfo, password string) error { return boom },
	}

	spec := enroll.TargetSpec{Kind: "hosted", StorageMode: "cloud", PublicHost: publicHost, TLS: true}
	_, err = p.Provision(ctx, spec, "ag_c1")
	require.ErrorIs(t, err, boom)
	require.NotContains(t, err.Error(), adminSecret, "the admin secret must never reach an error string")
	require.NotContains(t, err.Error(), "AKIDADMINADMINADMIN", "the admin key id must never reach an error string")

	keys, err := st.DeviceKeysForAgent(ctx, "ag_c1")
	require.NoError(t, err)
	require.Len(t, keys, 1, "the row is kept, disabled - never deleted, so the id is not reissued")
	require.NotNil(t, keys[0].DisabledAt, "an orphaned gateway key must not stay usable")

	_, err = st.DeviceKey(ctx, keys[0].AccessKeyID)
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestProvisionHostedRejectsAnIncompleteTarget(t *testing.T) {
	ctx := context.Background()
	st := seedAgent(t, "ag_h1")
	p := &enroll.Provisioner{Owner: "fleet@x", Store: st, SealKey: sealKey}

	for name, spec := range map[string]enroll.TargetSpec{
		"no public host": {Kind: "hosted", StorageMode: "disk", HostedRoot: t.TempDir()},
		"no root":        {Kind: "hosted", StorageMode: "disk", PublicHost: publicHost},
		"cloud mode":     {Kind: "hosted", StorageMode: "cloud", HostedRoot: t.TempDir(), PublicHost: publicHost},
		"no mode":        {Kind: "hosted", HostedRoot: t.TempDir(), PublicHost: publicHost},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := p.Provision(ctx, spec, "ag_h1")
			require.Error(t, err)

			keys, err := st.DeviceKeysForAgent(ctx, "ag_h1")
			require.NoError(t, err)
			require.Empty(t, keys, "nothing is minted before the target is known to be usable")
		})
	}
}

// The agent id is server-minted, but RemoveHostedRepository unlinks a tree, so
// it refuses anything that could resolve outside the device's own directory.
func TestRemoveHostedRepositoryRefusesAMalformedAgentID(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "keep"), 0o700))

	for _, id := range []string{"..", ".", "../keep", "a/b", `a\b`} {
		require.Error(t, enroll.RemoveHostedRepository(hostedSpec(root), id), "id %q", id)
	}

	require.DirExists(t, filepath.Join(root, "keep"))

	// A non-hosted target owns no hosted repository, and an empty id names none.
	require.NoError(t, enroll.RemoveHostedRepository(enroll.TargetSpec{Kind: "b2"}, "ag_h1"))
	require.NoError(t, enroll.RemoveHostedRepository(hostedSpec(root), ""))
	require.DirExists(t, root)
}
