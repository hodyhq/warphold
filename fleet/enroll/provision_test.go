package enroll_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/b2api"
	"github.com/kopia/kopia/fleet/enroll"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/maintenance"
)

func TestProvisionFilesystemCreatesConnectableRepo(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	p := &enroll.Provisioner{Owner: "fleet@test"}
	b, err := p.Provision(ctx, enroll.TargetSpec{Kind: "filesystem", Path: root}, "ag_1")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(root, "agents", "ag_1"), b.Prefix)
	require.Len(t, b.Password, 43)
	require.Empty(t, b.WriterKeyID)

	// The agent side: connect using only the token.
	ci, pw, err := repo.DecodeToken(b.ConnectToken)
	require.NoError(t, err)
	require.Equal(t, b.Password, pw)
	st, err := blob.NewStorage(ctx, ci, false)
	require.NoError(t, err)
	cfg := filepath.Join(t.TempDir(), "repository.config")
	require.NoError(t, repo.Connect(ctx, cfg, st, pw, &repo.ConnectOptions{}))
	r, err := repo.Open(ctx, cfg, pw, nil)
	require.NoError(t, err)
	defer r.Close(ctx)
	params, err := maintenance.GetParams(ctx, r)
	require.NoError(t, err)
	require.Equal(t, "fleet@test", params.Owner)

	// A second agent gets its own directory.
	b2, err := p.Provision(ctx, enroll.TargetSpec{Kind: "filesystem", Path: root}, "ag_2")
	require.NoError(t, err)
	require.NotEqual(t, b.Prefix, b2.Prefix)
	require.NotEqual(t, b.Password, b2.Password)
}

func TestProvisionB2UsesWriterKeyInTokenAndReaderKeyInBundle(t *testing.T) {
	ctx := context.Background()
	fake := &fakeB2{}
	p := &enroll.Provisioner{B2: fake, Owner: "fleet@test", InitializeForTesting: func(context.Context, blob.ConnectionInfo, string) error { return nil }}
	b, err := p.Provision(ctx, enroll.TargetSpec{Kind: "b2", Bucket: "hody-backups", AdminKeyID: "adm", AdminKey: "sec"}, "ag_9")
	require.NoError(t, err)
	require.Equal(t, "agents/ag_9/", b.Prefix)
	require.Len(t, fake.created, 2)
	require.Equal(t, "warphold-ag_9-writer", fake.created[0].Name)
	require.NotContains(t, fake.created[0].Capabilities, "deleteFiles")
	require.Equal(t, "warphold-ag_9-reader", fake.created[1].Name)
	require.NotContains(t, fake.created[1].Capabilities, "writeFiles")
	ci, _, err := repo.DecodeToken(b.ConnectToken)
	require.NoError(t, err)
	require.Equal(t, "b2", ci.Type)
	require.Equal(t, b.WriterKeyID, "kid-warphold-ag_9-writer")
	require.Equal(t, b.ReaderKeyID, "kid-warphold-ag_9-reader")

	require.NoError(t, p.Revoke(ctx, enroll.TargetSpec{Kind: "b2", AdminKeyID: "adm", AdminKey: "sec"}, b))
	require.ElementsMatch(t, []string{b.WriterKeyID, b.ReaderKeyID}, fake.deleted)
}

func TestProvisionB2CleansUpKeysWhenInitializeFails(t *testing.T) {
	ctx := context.Background()
	fake := &fakeB2{}
	wantErr := errors.New("boom")
	p := &enroll.Provisioner{B2: fake, Owner: "fleet@test", InitializeForTesting: func(context.Context, blob.ConnectionInfo, string) error { return wantErr }}
	_, err := p.Provision(ctx, enroll.TargetSpec{Kind: "b2", Bucket: "hody-backups", AdminKeyID: "adm", AdminKey: "sec"}, "ag_9")
	require.ErrorIs(t, err, wantErr)
	require.Len(t, fake.created, 2)
	require.ElementsMatch(t, []string{fake.created[0].Name, fake.created[1].Name}, []string{"warphold-ag_9-writer", "warphold-ag_9-reader"})
	require.ElementsMatch(t, []string{"kid-warphold-ag_9-writer", "kid-warphold-ag_9-reader"}, fake.deleted)
}

type fakeB2 struct {
	created []b2api.KeyRequest
	deleted []string
}

func (f *fakeB2) BucketInfo(context.Context, string, string, string) (b2api.BucketInfo, error) {
	return b2api.BucketInfo{ID: "bkt1", ObjectLockEnabled: true}, nil
}
func (f *fakeB2) CreateKey(_ context.Context, _, _ string, r b2api.KeyRequest) (b2api.CreatedKey, error) {
	f.created = append(f.created, r)
	return b2api.CreatedKey{KeyID: "kid-" + r.Name, Key: "sec-" + r.Name}, nil
}
func (f *fakeB2) DeleteKey(_ context.Context, _, _, id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}
